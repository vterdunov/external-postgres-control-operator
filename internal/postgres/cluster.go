// Package postgres provides isolated administrative sessions and connection checks.
package postgres

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	epco "github.com/vterdunov/external-postgres-control-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
)

const (
	SecretNotFound                = "SecretNotFound"
	InvalidSecret                 = "InvalidSecret"
	InvalidConfiguration          = "InvalidConfiguration"
	ConnectionUnavailable         = "ConnectionUnavailable"
	NotWritable                   = "NotWritable"
	tlsDisable                    = "disable"
	tlsAllow                      = "allow"
	tlsPrefer                     = "prefer"
	tlsRequire                    = "require"
	tlsVerifyCA                   = "verify-ca"
	tlsVerifyFull                 = "verify-full"
	defaultAdministrativeDatabase = "postgres"
	readOnlyMessage               = "PostgreSQL is read-only"
)

// Config contains one isolated connection and the exact client-side limits.
type Config struct {
	Connection        *pgx.ConnConfig
	ConnectTimeout    time.Duration
	StatementTimeout  time.Duration
	ReconcileInterval time.Duration
}

// Failure is safe to publish in Kubernetes status. It never wraps driver errors.
type Failure struct{ Reason, Message string }

func (f *Failure) Error() string { return f.Message }

// ConfigBuilder copies an immutable pgx template initialized before concurrent work.
type ConfigBuilder struct{ template *pgx.ConnConfig }

var neutralTemplate = sync.OnceValues(makeNeutralTemplate)

// NewConfigBuilder initializes pgx without consuming ambient PG settings or credentials.
// Call it before starting manager goroutines: pgx 5.7.5 requires temporarily
// removing PGSERVICE even when all connection-string options are explicit.
func NewConfigBuilder() (*ConfigBuilder, error) {
	config, err := neutralTemplate()
	if err != nil {
		return nil, err
	}
	return &ConfigBuilder{template: config}, nil
}

func makeNeutralTemplate() (config *pgx.ConnConfig, result error) {
	service, present := os.LookupEnv("PGSERVICE")
	if err := os.Unsetenv("PGSERVICE"); err != nil {
		return nil, &Failure{InvalidConfiguration, "Unable to initialize PostgreSQL configuration"}
	}
	defer func() {
		var err error
		if present {
			err = os.Setenv("PGSERVICE", service)
		} else {
			err = os.Unsetenv("PGSERVICE")
		}
		if err != nil {
			config = nil
			result = &Failure{InvalidConfiguration, "Unable to restore PostgreSQL environment"}
		}
	}()
	// Override every mapping in the pinned pgconn.parseEnvSettings. Parsing a
	// neutral template preserves pgx's private createdByParseConfig invariant.
	const settings = "host=localhost port=5432 database=postgres user=epco password='' " +
		"passfile=/dev/null servicefile=/dev/null sslmode=disable sslcert='' sslkey='' " +
		"sslrootcert='' sslpassword='' sslsni=1 sslnegotiation=postgres " +
		"connect_timeout=0 target_session_attrs=any timezone='' options='' application_name=''"
	var err error
	config, err = pgx.ParseConfig(settings)
	if err != nil {
		return nil, &Failure{InvalidConfiguration, "Unable to initialize PostgreSQL configuration"}
	}
	config.RuntimeParams = map[string]string{}
	return config, nil
}

// Build resolves only the referring namespace's Secrets and assigns typed values.
func (b *ConfigBuilder) Build(cluster *epco.PostgresCluster, secrets map[string]*corev1.Secret) (*Config, error) {
	if cluster == nil || b == nil || b.template == nil {
		return nil, &Failure{InvalidConfiguration, "PostgreSQL configuration is unavailable"}
	}
	spec := cluster.Spec
	if strings.TrimSpace(spec.Connection.Host) == "" || strings.ContainsRune(spec.Connection.Host, 0) ||
		strings.HasPrefix(spec.Connection.Host, "/") || spec.Connection.Port < 0 || spec.Connection.Port > 65535 {
		return nil, &Failure{InvalidConfiguration, "PostgreSQL endpoint is invalid"}
	}
	config := &Config{Connection: b.template.Copy()}
	var err error
	if config.ConnectTimeout, err = duration(spec.ConnectTimeout, 10*time.Second); err != nil {
		return nil, err
	}
	if config.StatementTimeout, err = duration(spec.StatementTimeout, 30*time.Second); err != nil {
		return nil, err
	}
	if config.ReconcileInterval, err = duration(spec.ReconcileInterval, 5*time.Minute); err != nil {
		return nil, err
	}
	auth, err := localSecret(cluster, secrets, spec.CredentialsSecret.Name)
	if err != nil {
		return nil, err
	}
	password, ok := auth.Data[corev1.BasicAuthPasswordKey]
	if auth.Type != corev1.SecretTypeBasicAuth || len(auth.Data[corev1.BasicAuthUsernameKey]) == 0 || !ok {
		return nil, &Failure{InvalidSecret, "Credentials Secret must contain a basic-auth username and password key"}
	}
	conn := config.Connection
	conn.Host = spec.Connection.Host
	conn.Port = 5432
	if spec.Connection.Port != 0 {
		conn.Port = uint16(spec.Connection.Port)
	}
	conn.Database = string(spec.Connection.Database)
	if conn.Database == "" {
		conn.Database = defaultAdministrativeDatabase
	}
	if strings.ContainsRune(conn.Database, 0) {
		return nil, &Failure{InvalidConfiguration, "PostgreSQL database is invalid"}
	}
	conn.User = string(auth.Data[corev1.BasicAuthUsernameKey])
	conn.Password = string(password)
	conn.ConnectTimeout = config.ConnectTimeout
	milliseconds := config.StatementTimeout / time.Millisecond
	if config.StatementTimeout%time.Millisecond != 0 {
		milliseconds++
	}
	// PostgreSQL represents statement_timeout as signed 32-bit milliseconds.
	// Reject an unrepresentable limit instead of silently shortening it.
	if milliseconds > 2147483647 {
		return nil, &Failure{InvalidConfiguration, "Statement timeout exceeds PostgreSQL's maximum supported duration"}
	}
	conn.RuntimeParams = map[string]string{"statement_timeout": strconv.FormatInt(int64(milliseconds), 10)}
	if err = configureTLS(conn, cluster, secrets); err != nil {
		return nil, err
	}
	return config, nil
}

func duration(value epco.PositiveDuration, fallback time.Duration) (time.Duration, error) {
	if value == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(string(value))
	if err != nil || d <= 0 {
		return 0, &Failure{InvalidConfiguration, "PostgreSQL timeouts and intervals must be positive durations"}
	}
	return d, nil
}

func localSecret(cluster *epco.PostgresCluster, secrets map[string]*corev1.Secret, name string) (*corev1.Secret, error) {
	if name == "" {
		return nil, &Failure{InvalidConfiguration, "Secret reference must have a name"}
	}
	secret := secrets[name]
	if secret == nil {
		return nil, &Failure{SecretNotFound, "A referenced Secret does not exist"}
	}
	if secret.Name != name || secret.Namespace != cluster.Namespace {
		return nil, &Failure{InvalidSecret, "Referenced Secret must belong to the PostgresCluster namespace"}
	}
	return secret, nil
}

func configureTLS(conn *pgx.ConnConfig, cluster *epco.PostgresCluster, secrets map[string]*corev1.Secret) error {
	mode := tlsVerifyFull
	if cluster.Spec.TLS != nil && cluster.Spec.TLS.Mode != "" {
		mode = cluster.Spec.TLS.Mode
	}
	conn.TLSConfig = nil
	conn.Fallbacks = nil
	if mode == tlsDisable {
		return nil
	}
	switch mode {
	case tlsAllow, tlsPrefer, tlsRequire, tlsVerifyCA, tlsVerifyFull:
	default:
		return &Failure{InvalidConfiguration, "PostgreSQL TLS mode is invalid"}
	}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: conn.Host}
	if cluster.Spec.TLS != nil {
		if ref := cluster.Spec.TLS.CASecret; ref != nil {
			secret, err := localSecret(cluster, secrets, ref.Name)
			if err != nil {
				return err
			}
			pool, err := certificatePool(secret.Data[ref.Key])
			if err != nil {
				return err
			}
			tlsConfig.RootCAs = pool
			if mode == tlsRequire {
				mode = tlsVerifyCA
			}
		}
		if ref := cluster.Spec.TLS.ClientCertificateSecret; ref != nil {
			secret, err := localSecret(cluster, secrets, ref.Name)
			if err != nil {
				return err
			}
			if secret.Type != corev1.SecretTypeTLS {
				return &Failure{InvalidSecret, "Client certificate Secret must have type kubernetes.io/tls"}
			}
			certificate, err := tls.X509KeyPair(secret.Data[corev1.TLSCertKey], secret.Data[corev1.TLSPrivateKeyKey])
			if err != nil {
				return &Failure{InvalidSecret, "Client certificate Secret must contain a valid certificate and matching key"}
			}
			tlsConfig.Certificates = []tls.Certificate{certificate}
		}
	}
	switch mode {
	case tlsAllow, tlsPrefer, tlsRequire:
		tlsConfig.InsecureSkipVerify = true // These sslmodes intentionally do not authenticate the server.
	case tlsVerifyCA:
		tlsConfig.InsecureSkipVerify = true // VerifyConnection verifies the chain without a hostname constraint.
		tlsConfig.VerifyConnection = verifyChain(tlsConfig.RootCAs)
	}
	fallback := &pgconn.FallbackConfig{Host: conn.Host, Port: conn.Port}
	if mode == tlsAllow {
		fallback.TLSConfig = tlsConfig
		conn.Fallbacks = []*pgconn.FallbackConfig{fallback}
	} else {
		conn.TLSConfig = tlsConfig
		if mode == tlsPrefer {
			conn.Fallbacks = []*pgconn.FallbackConfig{fallback}
		}
	}
	return nil
}

func certificatePool(data []byte) (*x509.CertPool, error) {
	pool := x509.NewCertPool()
	count := 0
	for remaining := bytes.TrimSpace(data); len(remaining) > 0; {
		if !bytes.HasPrefix(remaining, []byte("-----BEGIN CERTIFICATE-----")) {
			return nil, invalidCA()
		}
		// Decode only the first complete block. pem.Decode otherwise skips a
		// malformed block and may silently accept a later valid certificate.
		const endMarker = "-----END CERTIFICATE-----"
		end := bytes.Index(remaining, []byte(endMarker))
		if end < 0 {
			return nil, invalidCA()
		}
		end += len(endMarker)
		firstBlock := remaining[:end]
		if bytes.Count(firstBlock, []byte("-----BEGIN CERTIFICATE-----")) != 1 {
			return nil, invalidCA()
		}
		block, extra := pem.Decode(firstBlock)
		if block == nil || len(bytes.TrimSpace(extra)) != 0 || block.Type != "CERTIFICATE" || len(block.Headers) != 0 {
			return nil, invalidCA()
		}
		certificate, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, invalidCA()
		}
		pool.AddCert(certificate)
		count++
		remaining = bytes.TrimSpace(remaining[end:])
	}
	if count == 0 {
		return nil, invalidCA()
	}
	return pool, nil
}
func invalidCA() error {
	return &Failure{InvalidSecret, "CA Secret must contain valid PEM certificates"}
}

func verifyChain(roots *x509.CertPool) func(tls.ConnectionState) error {
	return func(state tls.ConnectionState) error {
		if len(state.PeerCertificates) == 0 {
			return errors.New("server did not present a certificate")
		}
		intermediates := x509.NewCertPool()
		for _, cert := range state.PeerCertificates[1:] {
			intermediates.AddCert(cert)
		}
		_, err := state.PeerCertificates[0].Verify(x509.VerifyOptions{Roots: roots, Intermediates: intermediates})
		return err
	}
}

// Checker tests authentication and writable primary state with a fresh connection.
type Checker interface {
	Check(context.Context, *Config) error
}
type probeConnection interface {
	QueryRow(context.Context, string, ...any) pgx.Row
	Close(context.Context) error
}
type connectionChecker struct {
	connect func(context.Context, *pgx.ConnConfig) (probeConnection, error)
}

func NewChecker() Checker { return &connectionChecker{} }
func (c *connectionChecker) Check(ctx context.Context, config *Config) error {
	connect := c.connect
	if connect == nil {
		connect = func(ctx context.Context, config *pgx.ConnConfig) (probeConnection, error) {
			connection, err := pgx.ConnectConfig(ctx, config)
			if err != nil {
				return nil, err
			}
			socket := connection.PgConn().Conn()
			if tlsSocket, ok := socket.(*tls.Conn); ok {
				// Forced disposal must bypass a potentially blocking TLS close_notify.
				socket = tlsSocket.NetConn()
			}
			return &pgxProbeConnection{
				probeConnection: connection,
				cleanupDone:     connection.PgConn().CleanupDone(),
				socket:          socket,
			}, nil
		}
	}
	connectCtx, cancel := context.WithTimeout(ctx, config.ConnectTimeout)
	connection, err := connect(connectCtx, config.Connection)
	cancel()
	if err != nil {
		return &Failure{ConnectionUnavailable, "Unable to authenticate or connect to PostgreSQL"}
	}
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), time.Second)
		defer cleanupCancel()
		// Close always disposes the socket even if the bounded graceful close fails.
		_ = connection.Close(cleanupCtx)
	}()
	statementCtx, statementCancel := context.WithTimeout(ctx, config.StatementTimeout)
	defer statementCancel()
	var recovery, readOnly bool
	err = connection.QueryRow(statementCtx,
		"SELECT pg_catalog.pg_is_in_recovery(), pg_catalog.current_setting('transaction_read_only')::boolean").Scan(&recovery, &readOnly)
	if err != nil {
		return &Failure{ConnectionUnavailable, "Unable to check PostgreSQL writable state"}
	}
	if recovery || readOnly {
		return &Failure{NotWritable, "PostgreSQL is in recovery or the session is read-only"}
	}
	return nil
}

type pgxProbeConnection struct {
	probeConnection
	cleanupDone <-chan struct{}
	socket      net.Conn
}

func (c *pgxProbeConnection) Close(ctx context.Context) error {
	closeError := c.probeConnection.Close(ctx)
	// pgx marks cancelled connections closed before its asynchronous cleanup
	// disposes the socket. Wait only within our independent cleanup deadline.
	select {
	case <-c.cleanupDone:
		return closeError
	case <-ctx.Done():
		return errors.Join(closeError, ctx.Err(), c.socket.Close())
	}
}
