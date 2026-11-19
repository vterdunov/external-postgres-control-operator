package postgres

import (
	"context"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/xdg-go/stringprep"
)

const (
	scramMechanism  = "SCRAM-SHA-256"
	scramIterations = 4096
	// Bound the work of verifying catalog values supplied outside the operator.
	maxSCRAMIterations   = 1_000_000
	maxSCRAMVerifierSize = 1024
)

type rolePasswordCatalog struct {
	started  time.Time
	readable bool
}

func (catalog rolePasswordCatalog) needsWrite(ctx context.Context, q roleQuerier, state *RoleState, desired RoleDesired) (bool, error) {
	if desired.Password == nil {
		return false, nil
	}
	if strings.ContainsRune(*desired.Password, 0) {
		return false, &Failure{InvalidSecret, "Role password must not contain a NUL byte"}
	}
	if !state.Exists {
		return true, nil
	}
	if !catalog.readable {
		previous := desired.PreviousPasswordWrite
		matches := previous != nil && previous.RoleOID == int64(state.OID) && previous.ServerStartUnixMicro == catalog.started.UnixMicro()
		return !matches, nil
	}
	var verifier *string
	err := q.QueryRow(ctx, `SELECT rolpassword FROM pg_catalog.pg_authid WHERE oid OPERATOR(pg_catalog.=) $1`, state.OID).Scan(&verifier)
	if errors.Is(err, pgx.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return !matchesPasswordVerifier(verifier, *desired.Password), nil
}

func passwordVerifier(password string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", &Failure{InvalidConfiguration, "Unable to encode role password"}
	}
	storedKey, serverKey, err := scramKeys(normalizePassword(password), salt, scramIterations)
	if err != nil {
		return "", err
	}
	encode := base64.StdEncoding.EncodeToString
	return fmt.Sprintf("%s$%d:%s$%s:%s", scramMechanism, scramIterations, encode(salt), encode(storedKey[:]), encode(serverKey)), nil
}

func matchesPasswordVerifier(verifier *string, password string) bool {
	password = normalizePassword(password)
	// PostgreSQL clears the stored password when the supplied verifier matches an empty password.
	if password == "" {
		return verifier == nil
	}
	if verifier == nil || len(*verifier) > maxSCRAMVerifierSize {
		return false
	}
	payload, ok := strings.CutPrefix(*verifier, scramMechanism+"$")
	if !ok {
		return false
	}
	parameters, keys, _ := strings.Cut(payload, "$")
	iterationsText, saltText, _ := strings.Cut(parameters, ":")
	iterations, err := strconv.Atoi(iterationsText)
	if err != nil || iterations <= 0 || iterations > maxSCRAMIterations {
		return false
	}
	decode := base64.StdEncoding.DecodeString
	salt, err := decode(saltText)
	if err != nil || len(salt) == 0 {
		return false
	}
	storedText, serverText, _ := strings.Cut(keys, ":")
	stored, storedErr := decode(storedText)
	server, serverErr := decode(serverText)
	if storedErr != nil || serverErr != nil || len(stored) != sha256.Size || len(server) != sha256.Size {
		return false
	}
	storedKey, serverKey, err := scramKeys(password, salt, iterations)
	if err != nil {
		return false
	}
	storedMatches := hmac.Equal(storedKey[:], stored)
	serverMatches := hmac.Equal(serverKey, server)
	return storedMatches && serverMatches
}

func normalizePassword(password string) string {
	// PostgreSQL uses RFC4013 SASLprep and raw bytes on invalid UTF-8 or prohibited input.
	if utf8.ValidString(password) {
		if normalized, err := stringprep.SASLprep.Prepare(password); err == nil {
			return normalized
		}
	}
	return password
}

func scramKeys(password string, salt []byte, iterations int) ([sha256.Size]byte, []byte, error) {
	key, err := pbkdf2.Key(sha256.New, password, salt, iterations, sha256.Size)
	if err != nil {
		return [sha256.Size]byte{}, nil, &Failure{InvalidConfiguration, "Unable to encode role password"}
	}
	client := hmac.New(sha256.New, key)
	_, _ = client.Write([]byte("Client Key"))
	stored := sha256.Sum256(client.Sum(nil))
	server := hmac.New(sha256.New, key)
	_, _ = server.Write([]byte("Server Key"))
	return stored, server.Sum(nil), nil
}
