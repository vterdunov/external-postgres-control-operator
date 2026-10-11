package postgres_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	epco "github.com/vterdunov/external-postgres-control-operator/api/v1alpha1"
	"github.com/vterdunov/external-postgres-control-operator/internal/postgres"
	"github.com/vterdunov/external-postgres-control-operator/test/utils"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	tlsVerifyFull      = "verify-full"
	testHost           = "db.example.test"
	tlsRequire         = "require"
	ambientUser        = "intruder"
	testNamespace      = "test"
	authenticationName = "auth"
	invalidValue       = "invalid"
	tlsDisable         = "disable"
	ambientPassword    = "env-secret"
)

func cluster() *epco.PostgresCluster {
	return &epco.PostgresCluster{Name: "cluster", Namespace: testNamespace, Spec: epco.PostgresClusterSpec{
		Connection: epco.PostgresConnection{Host: testHost}, CredentialsSecret: epco.LocalReference{Name: authenticationName},
	}}
}
func credentials() map[string]*corev1.Secret {
	return map[string]*corev1.Secret{authenticationName: {ObjectMeta: metav1.ObjectMeta{Name: authenticationName, Namespace: testNamespace}, Type: corev1.SecretTypeBasicAuth,
		Data: map[string][]byte{"username": []byte("user-special'=?"), "password": []byte("password-special\\'=?")}}}
}
func builder(t *testing.T) *postgres.ConfigBuilder {
	t.Helper()
	b, err := postgres.NewConfigBuilder()
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func build(t *testing.T, c *epco.PostgresCluster, secrets map[string]*corev1.Secret) *postgres.Config {
	t.Helper()
	config, err := builder(t).Build(c, secrets)
	if err != nil {
		t.Fatalf("configuration rejected: %v", err)
	}
	if config == nil || config.Connection == nil {
		t.Fatal("missing typed connection configuration")
	}
	return config
}
func reason(t *testing.T, err error, want string) {
	t.Helper()
	var failure *postgres.Failure
	if !errors.As(err, &failure) {
		t.Fatalf("want sanitized failure %q, got %T", want, err)
	}
	if failure.Reason != want {
		t.Errorf("reason=%q want=%q", failure.Reason, want)
	}
	if strings.Contains(failure.Error(), "password-special") || strings.Contains(failure.Error(), "user-special") {
		t.Fatal("failure publishes credentials")
	}
}

func TestClusterDefaultsAndTypedCredentials(t *testing.T) {
	config := build(t, cluster(), credentials())
	conn := config.Connection
	if conn.Host != testHost || conn.Port != 5432 || conn.Database != "postgres" {
		t.Errorf("connection defaults: host=%q port=%d database=%q", conn.Host, conn.Port, conn.Database)
	}
	if conn.User != "user-special'=?" || conn.Password != "password-special\\'=?" {
		t.Error("credentials did not survive typed assignment")
	}
	if config.ConnectTimeout != 10*time.Second || config.StatementTimeout != 30*time.Second || config.ReconcileInterval != 5*time.Minute {
		t.Error("API duration defaults not preserved")
	}
	if conn.TLSConfig == nil || conn.TLSConfig.ServerName != conn.Host || conn.TLSConfig.InsecureSkipVerify {
		t.Error("default must verify TLS chain and hostname")
	}
	if len(conn.Fallbacks) != 0 {
		t.Error("verify-full must never fall back to plaintext")
	}
}

func TestCredentialContract(t *testing.T) {
	for _, tc := range []struct {
		name   string
		modify func(map[string]*corev1.Secret)
		want   string
	}{
		{"missing", func(s map[string]*corev1.Secret) { delete(s, authenticationName) }, postgres.SecretNotFound},
		{"wrong type", func(s map[string]*corev1.Secret) { s[authenticationName].Type = corev1.SecretTypeOpaque }, postgres.InvalidSecret},
		{"missing username", func(s map[string]*corev1.Secret) { delete(s[authenticationName].Data, "username") }, postgres.InvalidSecret},
		{"empty username", func(s map[string]*corev1.Secret) { s[authenticationName].Data["username"] = nil }, postgres.InvalidSecret},
		{"missing password", func(s map[string]*corev1.Secret) { delete(s[authenticationName].Data, "password") }, postgres.InvalidSecret},
		{"foreign namespace", func(s map[string]*corev1.Secret) { s[authenticationName].Namespace = "foreign" }, postgres.InvalidSecret},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := credentials()
			tc.modify(s)
			_, err := builder(t).Build(cluster(), s)
			reason(t, err, tc.want)
		})
	}
	s := credentials()
	s[authenticationName].Data["password"] = []byte{}
	if got := build(t, cluster(), s).Connection.Password; got != "" {
		t.Error("empty password must be accepted")
	}
}

func TestConnectionValidation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		modify func(*epco.PostgresCluster)
	}{
		{"host", func(c *epco.PostgresCluster) { c.Spec.Connection.Host = "" }},
		{"port negative", func(c *epco.PostgresCluster) { c.Spec.Connection.Port = -1 }},
		{"port overflow", func(c *epco.PostgresCluster) { c.Spec.Connection.Port = 65536 }},
		{"unknown TLS", func(c *epco.PostgresCluster) { c.Spec.TLS = &epco.PostgresTLS{Mode: invalidValue} }},
		{"invalid duration", func(c *epco.PostgresCluster) { c.Spec.ConnectTimeout = "garbage" }},
		{"zero duration", func(c *epco.PostgresCluster) { c.Spec.StatementTimeout = "0s" }},
		{"negative duration", func(c *epco.PostgresCluster) { c.Spec.ReconcileInterval = "-1s" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := cluster()
			tc.modify(c)
			_, err := builder(t).Build(c, credentials())
			reason(t, err, postgres.InvalidConfiguration)
		})
	}
}

func TestTLSModes(t *testing.T) {
	for _, mode := range []string{tlsDisable, "allow", "prefer", tlsRequire, "verify-ca", tlsVerifyFull} {
		t.Run(mode, func(t *testing.T) {
			c := cluster()
			c.Spec.TLS = &epco.PostgresTLS{Mode: mode}
			conn := build(t, c, credentials()).Connection
			switch mode {
			case tlsDisable:
				if conn.TLSConfig != nil || len(conn.Fallbacks) != 0 {
					t.Error("disable must have plaintext only")
				}
			case "allow":
				if conn.TLSConfig != nil || len(conn.Fallbacks) != 1 || conn.Fallbacks[0].TLSConfig == nil {
					t.Error("allow must try plaintext then TLS")
				}
			case "prefer":
				if conn.TLSConfig == nil || len(conn.Fallbacks) != 1 || conn.Fallbacks[0].TLSConfig != nil {
					t.Error("prefer must try TLS then plaintext")
				}
			case tlsRequire:
				if conn.TLSConfig == nil || !conn.TLSConfig.InsecureSkipVerify || len(conn.Fallbacks) != 0 {
					t.Error("require without CA must require TLS without certificate verification")
				}
			case "verify-ca":
				if conn.TLSConfig == nil || !conn.TLSConfig.InsecureSkipVerify || conn.TLSConfig.VerifyConnection == nil || len(conn.Fallbacks) != 0 {
					t.Error("verify-ca must verify chain without requiring hostname")
				}
			case tlsVerifyFull:
				if conn.TLSConfig == nil || conn.TLSConfig.InsecureSkipVerify || conn.TLSConfig.ServerName != c.Spec.Connection.Host || len(conn.Fallbacks) != 0 {
					t.Error("verify-full must require verified hostname and chain")
				}
			}
		})
	}
}

func tlsSecrets(t *testing.T) map[string]*corev1.Secret {
	t.Helper()
	fixture, err := utils.NewTLSFixture(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ca, err := os.ReadFile(fixture.CA)
	if err != nil {
		t.Fatal(err)
	}
	crt, err := os.ReadFile(fixture.Certificate)
	if err != nil {
		t.Fatal(err)
	}
	key, err := os.ReadFile(fixture.Key)
	if err != nil {
		t.Fatal(err)
	}
	s := credentials()
	s["ca"] = &corev1.Secret{Name: "ca", Namespace: testNamespace, Data: map[string][]byte{"root": ca}}
	s["client"] = &corev1.Secret{Name: "client", Namespace: testNamespace, Type: corev1.SecretTypeTLS, Data: map[string][]byte{corev1.TLSCertKey: crt, corev1.TLSPrivateKeyKey: key}}
	return s
}
func tlsCluster() *epco.PostgresCluster {
	c := cluster()
	c.Spec.TLS = &epco.PostgresTLS{Mode: tlsVerifyFull, CASecret: &epco.SecretKeyReference{Name: "ca", Key: "root"}, ClientCertificateSecret: &epco.LocalReference{Name: "client"}}
	return c
}
func TestTLSSecretValidationAndIsolation(t *testing.T) {
	s := tlsSecrets(t)
	config := build(t, tlsCluster(), s)
	tlsConfig := config.Connection.TLSConfig
	expectedRoots := x509.NewCertPool()
	expectedRoots.AppendCertsFromPEM(s["ca"].Data["root"])
	if tlsConfig.RootCAs == nil || !expectedRoots.Equal(tlsConfig.RootCAs) || len(tlsConfig.Certificates) != 1 {
		t.Error("custom CA must replace trust roots and client key pair must be loaded")
	}
	c := tlsCluster()
	c.Spec.TLS.Mode = tlsRequire
	required := build(t, c, s).Connection.TLSConfig
	if required.VerifyConnection == nil {
		t.Error("require with supplied CA must validate certificate chain")
	}
	for _, tc := range []struct {
		name   string
		modify func(map[string]*corev1.Secret)
		want   string
	}{
		{"missing CA", func(s map[string]*corev1.Secret) { delete(s, "ca") }, postgres.SecretNotFound},
		{"missing CA key", func(s map[string]*corev1.Secret) { delete(s["ca"].Data, "root") }, postgres.InvalidSecret},
		{"malformed prefix before valid PEM", func(s map[string]*corev1.Secret) {
			s["ca"].Data["root"] = append([]byte("-----BEGIN CERTIFICATE-----\ninvalid-base64!\n-----END CERTIFICATE-----\n"), s["ca"].Data["root"]...)
		}, postgres.InvalidSecret},
		{"invalid PEM", func(s map[string]*corev1.Secret) { s["ca"].Data["root"] = []byte("not a certificate") }, postgres.InvalidSecret},
		{"missing client", func(s map[string]*corev1.Secret) { delete(s, "client") }, postgres.SecretNotFound},
		{"client type", func(s map[string]*corev1.Secret) { s["client"].Type = corev1.SecretTypeOpaque }, postgres.InvalidSecret},
		{"invalid pair", func(s map[string]*corev1.Secret) { s["client"].Data[corev1.TLSPrivateKeyKey] = []byte("not a key") }, postgres.InvalidSecret},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := tlsSecrets(t)
			tc.modify(s)
			_, err := builder(t).Build(tlsCluster(), s)
			reason(t, err, tc.want)
		})
	}
	c = tlsCluster()
	c.Spec.TLS.Mode = tlsDisable
	if build(t, c, credentials()).Connection.TLSConfig != nil {
		t.Error("disable must ignore missing TLS Secrets")
	}
	tlsConfig.MinVersion = tls.VersionTLS13
	if build(t, tlsCluster(), s).Connection.TLSConfig.MinVersion == tls.VersionTLS13 {
		t.Error("configuration copies must not share mutable TLS configuration")
	}
}

func TestStatementTimeoutRoundsUpAndPreservesExactDurations(t *testing.T) {
	for _, tc := range []struct {
		duration epco.PositiveDuration
		want     string
	}{{"1ns", "1"}, {"1000001ns", "2"}, {"1.5ms", "2"}, {"2ms", "2"}} {
		t.Run(string(tc.duration), func(t *testing.T) {
			c := cluster()
			c.Spec.StatementTimeout = tc.duration
			c.Spec.ConnectTimeout = "1.5ms"
			config := build(t, c, credentials())
			if config.Connection.RuntimeParams["statement_timeout"] != tc.want {
				t.Errorf("server timeout=%q want=%q", config.Connection.RuntimeParams["statement_timeout"], tc.want)
			}
			if config.ConnectTimeout != 1500*time.Microsecond {
				t.Error("context deadline must retain sub-millisecond precision")
			}
		})
	}
}

func TestPGEnvironmentCannotChangeConnectionAndIsRestored(t *testing.T) {
	if os.Getenv("EPCO_HOSTILE_PG_CHILD") == "" {
		executable, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		for _, serviceState := range []string{"unset", "empty", "named"} {
			t.Run(serviceState, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				command := exec.CommandContext(ctx, executable, "-test.run=^TestPGEnvironmentCannotChangeConnectionAndIsRestored$")
				command.Env = append(os.Environ(), "EPCO_HOSTILE_PG_CHILD="+serviceState)
				output, err := command.CombinedOutput()
				if err != nil {
					t.Fatalf("hostile fresh-process failed: %v\n%s", err, output)
				}
			})
		}
		return
	}
	path := filepath.Join(t.TempDir(), "untrusted.conf")
	if err := os.WriteFile(path, []byte("[intruder]\nhost=untrusted\npassword=file-secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for key, value := range map[string]string{"PGSERVICE": ambientUser, "PGSERVICEFILE": path, "PGHOST": "untrusted", "PGPORT": "1", "PGDATABASE": "untrusted", "PGUSER": ambientUser, "PGPASSWORD": ambientPassword, "PGPASSFILE": path, "PGSSLMODE": tlsDisable, "PGSSLROOTCERT": path, "PGSSLCERT": path, "PGSSLKEY": path, "PGCONNECT_TIMEOUT": "999", "PGTARGETSESSIONATTRS": "read-only", "PGOPTIONS": "-c default_transaction_read_only=on", "PGAPPNAME": ambientUser, "PGTZ": "intruder-zone", "PGSSLPASSWORD": "intruder-key-pass", "PGSSLNEGOTIATION": invalidValue, "PGSSLSNI": invalidValue} {
		t.Setenv(key, value)
	}
	if os.Getenv("EPCO_HOSTILE_PG_CHILD") == "empty" {
		t.Setenv("PGSERVICE", "")
	}
	if os.Getenv("EPCO_HOSTILE_PG_CHILD") == "unset" {
		if err := os.Unsetenv("PGSERVICE"); err != nil {
			t.Fatal(err)
		}
	}
	originalService, originalServicePresent := os.LookupEnv("PGSERVICE")
	b := builder(t)
	config, err := b.Build(cluster(), credentials())
	if err != nil {
		t.Fatalf("untrusted environment broke configuration: %v", err)
	}
	if config == nil || config.Connection == nil {
		t.Fatal("missing configuration")
	}
	if config.Connection.Host != testHost || config.Connection.Password == ambientPassword || config.Connection.Password == "file-secret" {
		t.Error("ambient credentials or endpoint used")
	}
	if _, ok := config.Connection.RuntimeParams["options"]; ok {
		t.Error("ambient options reached the server")
	}
	service, servicePresent := os.LookupEnv("PGSERVICE")
	if service != originalService || servicePresent != originalServicePresent || os.Getenv("PGPASSWORD") != ambientPassword {
		t.Error("builder did not restore environment")
	}
}

func TestCheckerCancelledContext(t *testing.T) {
	config := build(t, cluster(), credentials())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	err := postgres.NewChecker().Check(ctx, config)
	reason(t, err, postgres.ConnectionUnavailable)
	if time.Since(start) > time.Second {
		t.Error("cancelled check blocked")
	}
}

func TestServerStatementTimeoutRange(t *testing.T) {
	c := cluster()
	c.Spec.StatementTimeout = "2147483647ms"
	config := build(t, c, credentials())
	if config.Connection.RuntimeParams["statement_timeout"] != "2147483647" {
		t.Error("maximum server timeout must be preserved")
	}
	c.Spec.StatementTimeout = "2147483647000001ns"
	_, err := builder(t).Build(c, credentials())
	reason(t, err, postgres.InvalidConfiguration)
}
