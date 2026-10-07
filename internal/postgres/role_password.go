package postgres

import (
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"unicode/utf8"

	"github.com/xdg-go/stringprep"
)

func passwordVerifier(password string) (string, error) {
	// PostgreSQL uses RFC4013 SASLprep and raw bytes on invalid UTF-8 or prohibited input.
	if utf8.ValidString(password) {
		if normalized, err := stringprep.SASLprep.Prepare(password); err == nil {
			password = normalized
		}
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", &Failure{InvalidConfiguration, "Unable to encode role password"}
	}
	key, err := pbkdf2.Key(sha256.New, password, salt, 4096, 32)
	if err != nil {
		return "", &Failure{InvalidConfiguration, "Unable to encode role password"}
	}
	client := hmac.New(sha256.New, key)
	_, _ = client.Write([]byte("Client Key"))
	stored := sha256.Sum256(client.Sum(nil))
	server := hmac.New(sha256.New, key)
	_, _ = server.Write([]byte("Server Key"))
	encode := base64.StdEncoding.EncodeToString
	return fmt.Sprintf("SCRAM-SHA-256$4096:%s$%s:%s", encode(salt), encode(stored[:]), encode(server.Sum(nil))), nil
}
