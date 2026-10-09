// Package integration provides suite-owned PostgreSQL fixtures. It has no dependency
// on envtest, so future controller suites can combine the two environments.
package integration

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/vterdunov/external-postgres-control-operator/test/utils"
)

const (
	loopbackHost = "127.0.0.1"
	tlsPrefer    = "prefer"
)

// ImageForVersion pins every supported PostgreSQL major to an explicit patch tag.
func ImageForVersion(major string) (string, error) {
	images := map[string]string{
		"15": "postgres:15.19-bookworm", "16": "postgres:16.15-bookworm",
		"17": "postgres:17.11-bookworm", "18": "postgres:18.6-bookworm",
	}
	image, ok := images[major]
	if !ok {
		return "", fmt.Errorf("unsupported PostgreSQL version %q (supported: 15-18)", major)
	}
	return "public.ecr.aws/docker/library/" + image, nil
}

// Options configures an isolated PostgreSQL suite. RunID is the runner's ownership label.
type Options struct {
	Version, RunID, ArtifactsDir string
	DisableTLS                   bool
}

// Server owns exactly one container, a verified TLS endpoint and an administrative SQL client.
type Server struct {
	Admin                           *sql.DB
	TLS                             TLSFixture
	Container, RunID                string
	port                            uint16
	password, tempDir, artifactsDir string
	disableTLS                      bool
}

func randomName(prefix string) (string, error) {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(b), nil
}
func quoteIdentifier(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }
func quoteLiteral(s string) string    { return `'` + strings.ReplaceAll(s, `'`, `''`) + `'` }

// Start waits for a successful authenticated SELECT over verified TLS, rather than
// relying on pg_isready. If setup fails, diagnostics are saved before cleanup.
func Start(ctx context.Context, options Options) (_ *Server, resultErr error) {
	if options.Version == "" {
		options.Version = "16"
	}
	image, err := ImageForVersion(options.Version)
	if err != nil {
		return nil, err
	}
	s := &Server{RunID: options.RunID, artifactsDir: options.ArtifactsDir, disableTLS: options.DisableTLS}
	s.Container, err = randomName("epco-pg-")
	if err != nil {
		return nil, err
	}
	if s.RunID == "" {
		s.RunID = s.Container
	}
	s.password, err = randomName("password-")
	if err != nil {
		return nil, err
	}
	s.tempDir, err = os.MkdirTemp(os.Getenv("EPCO_TEST_TMPDIR"), "epco-postgres-")
	if err != nil {
		return nil, err
	}
	defer func() {
		if resultErr != nil {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			resultErr = errors.Join(resultErr, s.Diagnostics(cleanupCtx), s.Close(cleanupCtx))
		}
	}()
	s.TLS, err = NewTLSFixture(s.tempDir)
	if err != nil {
		return nil, err
	}
	// Stage keys in the container and let the image entrypoint drop root privileges.
	// Host UID assumptions are avoided by copying from the read-only bind mount.
	envFile := filepath.Join(s.tempDir, "postgres.env")
	envContent := "POSTGRES_PASSWORD=" + s.password + "\nPGDATA=/var/lib/postgresql/data\n"
	if err = os.WriteFile(envFile, []byte(envContent), 0600); err != nil {
		return nil, err
	}
	stageKeys := "cp /fixtures/server.crt /fixtures/server.key /fixtures/ca.crt /tmp/; " +
		"chown postgres:postgres /tmp/server.key /tmp/server.crt /tmp/ca.crt; chmod 600 /tmp/server.key; " +
		"exec docker-entrypoint.sh postgres -c ssl=on -c ssl_cert_file=/tmp/server.crt " +
		"-c ssl_key_file=/tmp/server.key -c ssl_ca_file=/tmp/ca.crt -c log_statement=none"
	if options.DisableTLS {
		stageKeys = strings.Replace(stageKeys, "ssl=on", "ssl=off", 1)
	}
	_, err = s.docker(ctx, "run", "--detach", "--name", s.Container, "--label", "epco.test.run="+s.RunID,
		"--publish", "127.0.0.1::5432", "--env-file", envFile,
		"--mount", "type=bind,source="+s.tempDir+",target=/fixtures,readonly",
		"--entrypoint", "/bin/sh", image, "-ec", stageKeys)
	if err != nil {
		return nil, err
	}
	output, err := s.docker(ctx, "port", s.Container, "5432/tcp")
	if err != nil {
		return nil, err
	}
	portText := strings.TrimPrefix(strings.TrimSpace(output), "127.0.0.1:")
	port, err := strconv.ParseUint(portText, 10, 16)
	if err != nil {
		return nil, fmt.Errorf("read fixture port: %w", err)
	}
	s.port = uint16(port)
	s.Admin, err = s.Open("postgres", "postgres", s.password)
	if err != nil {
		return nil, err
	}
	readyCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	for {
		var one int
		queryCtx, queryCancel := context.WithTimeout(readyCtx, 2*time.Second)
		err = s.Admin.QueryRowContext(queryCtx, "SELECT 1").Scan(&one)
		queryCancel()
		if err == nil && one == 1 {
			return s, nil
		}
		select {
		case <-readyCtx.Done():
			return nil, fmt.Errorf("PostgreSQL SQL readiness: %w", readyCtx.Err())
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func (s *Server) docker(ctx context.Context, args ...string) (string, error) {
	return utils.Run(exec.CommandContext(ctx, "docker", args...))
}

// Open constructs connection configuration without embedding credentials in a DSN
// or logs. The certificate is verified against the suite CA and loopback identity.
func (s *Server) Open(database, role, password string) (*sql.DB, error) {
	// Override libpq client environment without changing process-global variables.
	settings := url.Values{
		"service": {"epco-fixture"}, "servicefile": {s.TLS.Service}, "sslmode": {"disable"},
		"connect_timeout": {"2"}, "target_session_attrs": {"any"}, "sslnegotiation": {"postgres"},
	}
	config, err := pgx.ParseConfig("postgres://postgres:unused@127.0.0.1:5432/postgres?" + settings.Encode())
	if err != nil {
		return nil, err
	}
	config.Host = loopbackHost
	config.Port = s.port
	config.Database = database
	config.User = role
	config.Password = password
	config.ConnectTimeout = 2 * time.Second
	tlsConfig, err := tlsConfig(s.TLS.CA)
	if err != nil {
		return nil, err
	}
	config.TLSConfig = tlsConfig
	if s.disableTLS {
		config.TLSConfig = nil
	}
	config.Fallbacks = nil
	config.RuntimeParams = map[string]string{"application_name": "epco-tests", "statement_timeout": "5000"}
	db := stdlib.OpenDB(*config)
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(1)
	return db, nil
}

// Diagnostics saves logs and state without secrets or docker inspect's environment.
func (s *Server) Diagnostics(ctx context.Context) error {
	if s.artifactsDir == "" {
		return nil
	}
	if err := os.MkdirAll(s.artifactsDir, 0700); err != nil {
		return err
	}
	logs, logErr := s.docker(ctx, "logs", s.Container)
	state, stateErr := s.docker(ctx, "inspect", "--format", "{{json .State}}", s.Container)
	content := logs + "\n" + state
	if s.password != "" {
		content = strings.ReplaceAll(content, s.password, "[REDACTED]")
	}
	content = utils.Redact(content)
	writeErr := os.WriteFile(filepath.Join(s.artifactsDir, s.Container+".log"), []byte(content), 0600)
	return errors.Join(logErr, stateErr, writeErr)
}

// Close removes only this server's labelled container and its temporary files.
func (s *Server) Close(ctx context.Context) error {
	var errs []error
	if s.Admin != nil {
		errs = append(errs, s.Admin.Close())
		s.Admin = nil
	}
	output, err := s.docker(ctx, "inspect", "--format", "{{json .Config.Labels}}", s.Container)
	if err == nil {
		var labels map[string]string
		if err = json.Unmarshal([]byte(output), &labels); err != nil {
			errs = append(errs, err)
		} else if labels["epco.test.run"] != s.RunID {
			errs = append(errs, errors.New("refusing cleanup: PostgreSQL ownership label differs"))
		} else {
			_, err = s.docker(ctx, "rm", "--force", "--volumes", s.Container)
			errs = append(errs, err)
		}
	} else if !strings.Contains(err.Error(), "No such object") && !strings.Contains(err.Error(), "No such container") {
		errs = append(errs, err)
	}
	if s.tempDir != "" {
		errs = append(errs, os.RemoveAll(s.tempDir))
		s.tempDir = ""
	}
	return errors.Join(errs...)
}
