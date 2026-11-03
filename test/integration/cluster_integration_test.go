//go:build integration

package integration

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	epco "github.com/vterdunov/external-postgres-control-operator/api/v1alpha1"
	pgcheck "github.com/vterdunov/external-postgres-control-operator/internal/postgres"
	"github.com/vterdunov/external-postgres-control-operator/test/utils"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	clusterAuthenticationName = "auth"
	tlsRequire                = "require"
	tlsVerifyCA               = "verify-ca"
)

type stalledQueryConnection struct {
	net.Conn
	armed   atomic.Bool
	written chan struct{}
	once    sync.Once
}

func (c *stalledQueryConnection) Write(bytes []byte) (int,
	error) {
	if c.armed.Load() {
		c.once.Do(func() { close(c.written) })
		return len(bytes),
			nil
	}
	return c.Conn.Write(bytes)
}

func productionConfig(f *Fixture, mode string, clientCert bool) *pgcheck.Config {
	return productionConfigUsingCA(f, mode, clientCert, f.Server.TLS.CA)
}
func productionConfigUsingCA(f *Fixture, mode string, clientCert bool, caPath string) *pgcheck.Config {
	c := &epco.ExternalPostgres{Namespace: "integration", Spec: epco.ExternalPostgresSpec{
		Connection: epco.PostgresConnection{Host: loopbackHost,
			Port:     int32(f.Server.port),
			Database: epco.SQLName(f.Database)},
		CredentialsSecret: epco.LocalReference{Name: clusterAuthenticationName},
		TLS: &epco.PostgresTLS{Mode: mode,
			CASecret: &epco.SecretKeyReference{Name: "ca",
				Key: roleCAKey}},
		ConnectTimeout:   "2s",
		StatementTimeout: "2s"}}
	ca, err := os.ReadFile(caPath)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	secrets := map[string]*corev1.Secret{
		clusterAuthenticationName: {ObjectMeta: metav1.ObjectMeta{Name: clusterAuthenticationName,
			Namespace: c.Namespace},
			Type: corev1.SecretTypeBasicAuth,
			Data: map[string][]byte{corev1.BasicAuthUsernameKey: []byte(f.Role),
				corev1.BasicAuthPasswordKey: []byte(f.Password)}},
		"ca": {ObjectMeta: metav1.ObjectMeta{Name: "ca",
			Namespace: c.Namespace},
			Data: map[string][]byte{roleCAKey: ca}}}
	if clientCert {
		crt, err := os.ReadFile(f.Server.TLS.ClientCertificate)
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
		key, err := os.ReadFile(f.Server.TLS.ClientKey)
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
		c.Spec.TLS.ClientCertificateSecret = &epco.LocalReference{Name: "client"}
		secrets["client"] = &corev1.Secret{Name: "client",
			Namespace: c.Namespace,
			Type:      corev1.SecretTypeTLS,
			Data: map[string][]byte{corev1.TLSCertKey: crt,
				corev1.TLSPrivateKeyKey: key}}
	}
	b, err := pgcheck.NewConfigBuilder()
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	config, err := b.Build(c, secrets)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	return config
}
func failureReason(err error) string {
	if f, ok := errors.AsType[*pgcheck.Failure](err); ok {
		return f.Reason
	}
	return ""
}

func recordFixtureAuthentication(ctx context.Context, f *Fixture, policy, stage string) {
	diagnostic, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var hbaPath, dataDirectory string
	var loaded time.Time
	err := f.Server.Admin.QueryRowContext(diagnostic,
		"SELECT current_setting('hba_file'), current_setting('data_directory'), pg_conf_load_time()").
		Scan(&hbaPath, &dataDirectory, &loaded)
	GinkgoWriter.Printf("HBA fixture policy=%s stage=%s same_server=%t hba_file=%q data_directory=%q "+
		"backend_config_load_time=%s metadata_error_type=%T sqlstate=%q\n",
		policy, stage, f.Server == postgres, hbaPath, dataDirectory,
		loaded.UTC().Format(time.RFC3339Nano), err, SQLState(err))
	// This view describes disk contents, not proof of the last successfully loaded rules.
	rows, err := f.Server.Admin.QueryContext(diagnostic, `SELECT line_number, type, COALESCE(address,''),
	 COALESCE(netmask,''), auth_method, COALESCE('clientcert=verify-ca'=ANY(options),false), error IS NULL
	 FROM pg_hba_file_rules WHERE $1=ANY(database) AND $2=ANY(user_name) ORDER BY line_number`, f.Database, f.Role)
	if err != nil {
		GinkgoWriter.Printf("HBA fixture policy=%s stage=%s row_query_error_type=%T sqlstate=%q\n",
			policy, stage, err, SQLState(err))
		return
	}
	defer func() { _ = rows.Close() }()
	var count, rejected, certificates int
	for rows.Next() {
		var line int
		var transport, address, mask, authentication string
		var certificate, valid bool
		if err := rows.Scan(&line, &transport, &address, &mask, &authentication, &certificate, &valid); err != nil {
			GinkgoWriter.Printf("HBA fixture policy=%s stage=%s row_decode_error_type=%T\n", policy, stage, err)
			return
		}
		count++
		if authentication == "reject" {
			rejected++
		}
		if certificate {
			certificates++
		}
		family := "ipv4"
		if strings.Contains(address, ":") {
			family = "ipv6"
		} else if address == "all" || address == "" {
			family = "any-or-local"
		}
		GinkgoWriter.Printf("HBA fixture policy=%s stage=%s line=%d transport=%s address=%q netmask=%q "+
			"address_family=%s authentication=%s clientcert_verify_ca=%t valid=%t\n",
			policy, stage, line, transport, address, mask, family, authentication, certificate, valid)
	}
	expectedCertificates := 0
	if policy == "client-cert" {
		expectedCertificates = 1
	}
	GinkgoWriter.Printf("HBA fixture policy=%s stage=%s fixture_rows=%d reject_rows=%d expected_reject_rows=1 "+
		"clientcert_rows=%d expected_clientcert_rows=%d row_error_type=%T\n",
		policy, stage, count, rejected, certificates, expectedCertificates, rows.Err())
}

func recordFixtureAuthenticationCandidate(ctx context.Context, f *Fixture, mode string) {
	config := productionConfig(f, mode, false)
	connect, cancel := context.WithTimeout(ctx, config.ConnectTimeout)
	candidate, err := pgx.ConnectConfig(connect, config.Connection.Copy())
	cancel()
	GinkgoWriter.Printf("HBA fresh candidate mode=%s connected=%t error_type=%T reason=%q sqlstate=%q "+
		"deadline_exceeded=%t canceled=%t\n", mode, err == nil, err, failureReason(err), SQLState(err),
		errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled))
	if err != nil {
		return
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = candidate.Close(cleanup)
	}()
	query, cancel := context.WithTimeout(ctx, config.StatementTimeout)
	defer cancel()
	var address, role, database string
	var family int
	var encrypted bool
	err = candidate.QueryRow(query, `SELECT COALESCE(inet_client_addr()::text,''),
	 COALESCE(family(inet_client_addr()),0), current_user, current_database(),
	 COALESCE((SELECT ssl FROM pg_stat_ssl WHERE pid=pg_backend_pid()),false)`).
		Scan(&address, &family, &role, &database, &encrypted)
	GinkgoWriter.Printf("HBA fresh candidate mode=%s backend_pid=%d client_address=%q address_family=%d "+
		"sql_role=%q sql_database=%q ssl=%t metadata_error_type=%T sqlstate=%q\n",
		mode, candidate.PgConn().PID(), address, family, role, database, encrypted, err, SQLState(err))
}

func setFixtureAuthentication(ctx context.Context, f *Fixture, policy string) {
	server := f.Server
	// Match only this fixture; suite administrator and other fixtures remain usable.
	var rule string
	switch policy {
	case "client-cert":
		rule = "hostnossl " + f.Database + " " + f.Role + " 0.0.0.0/0 reject\n" +
			"hostssl " + f.Database + " " + f.Role + " 0.0.0.0/0 scram-sha-256 clientcert=verify-ca\n"
	case "plaintext":
		rule = "hostssl " + f.Database + " " + f.Role + " 0.0.0.0/0 reject\n"
	case "tls":
		rule = "hostnossl " + f.Database + " " + f.Role + " 0.0.0.0/0 reject\n"
	}
	var hbaPath, original string
	Expect(server.Admin.QueryRowContext(ctx,
		"SELECT current_setting('hba_file'), pg_read_file(current_setting('hba_file'))").
		Scan(&hbaPath, &original)).To(Succeed())
	write := func(ctx context.Context, content string) {
		// Docker archive copies do not reach PGDATA's mounted tmpfs. Write from
		// inside the container, preserving the active file's owner and permissions.
		command := exec.CommandContext(ctx, "docker", "exec", "-i", "--user", "postgres", server.Container,
			"sh", "-c", `cat > "$1"`, "sh", hbaPath)
		command.Stdin = strings.NewReader(content)
		_, err := utils.Run(command)
		Expect(err).NotTo(HaveOccurred())
		var observed string
		Expect(server.Admin.QueryRowContext(ctx, "SELECT pg_read_file($1)", hbaPath).Scan(&observed)).To(Succeed())
		matches := observed == content
		Expect(matches).To(BeTrue(), "the active HBA file must contain the intended fixture policy")
		_, err = server.Admin.ExecContext(ctx, "SELECT pg_reload_conf()")
		Expect(err).NotTo(HaveOccurred())
	}
	recordFixtureAuthentication(ctx, f, policy, "before-reload")
	DeferCleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		write(cleanup, original)
		Eventually(func() error {
			return pgcheck.NewChecker().Check(cleanup,
				productionConfig(f,
					"disable",
					false))
		}).WithContext(cleanup).WithTimeout(5 * time.Second).Should(Succeed())
	})
	write(ctx, rule+original)
	// Reload signals are processed asynchronously; verify the desired rule is parsed.
	Eventually(func() bool {
		var valid bool
		err := server.Admin.QueryRowContext(ctx,
			"SELECT count(*)>0 AND bool_and(error IS NULL) FROM pg_hba_file_rules").Scan(&valid)
		return err == nil && valid
	}).WithContext(ctx).Should(BeTrue())
	recordFixtureAuthentication(ctx, f, policy, "after-reload-disk-rules")
	forbiddenMode := "disable"
	if policy == "plaintext" {
		forbiddenMode = tlsRequire
	}
	recordedCandidate := false
	Eventually(func() string {
		err := pgcheck.NewChecker().Check(ctx, productionConfig(f, forbiddenMode, false))
		reason := failureReason(err)
		GinkgoWriter.Printf("HBA forbidden checker policy=%s mode=%s error_nil=%t error_type=%T reason=%q "+
			"sqlstate=%q deadline_exceeded=%t canceled=%t\n", policy, forbiddenMode, err == nil, err, reason, SQLState(err),
			errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled))
		if err == nil && !recordedCandidate {
			recordedCandidate = true
			recordFixtureAuthentication(ctx, f, policy, "forbidden-checker-success")
			recordFixtureAuthenticationCandidate(ctx, f, forbiddenMode)
		}
		return reason
	}).WithContext(ctx).WithTimeout(5 * time.Second).Should(Equal("ConnectionUnavailable"))
}

var _ = Describe("Production PostgreSQL connection checker", func() {
	var fixture *Fixture
	BeforeEach(func(ctx SpecContext) {
		var err error
		fixture, err = postgres.NewFixture(ctx)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			cleanup, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			Expect(fixture.Close(cleanup)).To(Succeed())
		})
	}, NodeTimeout(20*time.Second))
	It("authenticates a restricted login through every TLS mode and closes each connection", func(ctx SpecContext) {
		checker := pgcheck.NewChecker()
		for _, mode := range []string{"disable", "allow", tlsPrefer, tlsRequire, tlsVerifyCA, roleTLSMode} {
			Expect(checker.Check(ctx, productionConfig(fixture, mode, false))).To(Succeed(), "mode %s", mode)
		}
		var connections int
		Eventually(func() int {
			if postgres.Admin.QueryRowContext(ctx,
				"SELECT count(*) FROM pg_stat_activity WHERE usename=$1",
				fixture.Role).Scan(&connections) != nil {
				return 100
			}
			return connections
		}).WithContext(ctx).Should(BeNumerically("<=", 1), "only the fixture's persistent limited client may remain")
	}, NodeTimeout(25*time.Second))
	It("tries plaintext before TLS for allow and TLS before plaintext for prefer", func(ctx SpecContext) {
		for _, mode := range []string{"allow", tlsPrefer} {
			config := productionConfig(fixture, mode, false)
			observed := false
			connected := false
			config.Connection.AfterConnect = func(_ context.Context, connection *pgconn.PgConn) error {
				_, observed = connection.Conn().(*tls.Conn)
				connected = true
				return nil
			}
			Expect(pgcheck.NewChecker().Check(ctx, config)).To(Succeed())
			Expect(connected).To(BeTrue())
			Expect(observed).To(Equal(mode == tlsPrefer))
		}
	}, NodeTimeout(20*time.Second))
	It("falls back to plaintext for prefer when the server disables TLS", func(ctx SpecContext) {
		plain,
			err := Start(ctx,
			Options{Version: os.Getenv("EPCO_PG_VERSION"),
				RunID:        postgres.RunID,
				ArtifactsDir: os.Getenv("EPCO_ARTIFACTS_DIR"),
				DisableTLS:   true})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			cleanup, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			Expect(plain.Close(cleanup)).To(Succeed())
		})
		f, err := plain.NewFixture(ctx)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			cleanup, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			Expect(f.Close(cleanup)).To(Succeed())
		})
		Expect(pgcheck.NewChecker().Check(ctx, productionConfig(f, tlsPrefer, false))).To(Succeed())
		Expect(failureReason(pgcheck.NewChecker().Check(ctx,
			productionConfig(f,
				tlsRequire,
				false)))).To(Equal("ConnectionUnavailable"))
	}, NodeTimeout(90*time.Second))

	It("closes an authenticated production connection when its parent is cancelled", func(ctx SpecContext) {
		config := productionConfig(fixture, roleTLSMode, false)
		cancelled, cancel := context.WithCancel(ctx)
		defer cancel()
		config.Connection.AfterConnect = func(context.Context, *pgconn.PgConn) error { cancel(); return nil }
		Expect(failureReason(pgcheck.NewChecker().Check(cancelled, config))).To(Equal("ConnectionUnavailable"))
		Eventually(func() int {
			var connections int
			err := postgres.Admin.QueryRowContext(ctx,
				"SELECT count(*) FROM pg_stat_activity WHERE usename=$1",
				fixture.Role).Scan(&connections)
			if err != nil {
				return 100
			}
			return connections
		}).WithContext(ctx).Should(BeNumerically("<=", 1))
		Expect(pgcheck.NewChecker().Check(ctx, productionConfig(fixture, roleTLSMode, false))).To(Succeed())
	}, NodeTimeout(20*time.Second))
	It("cancels a blocked production SQL probe and disposes its connection", func(ctx SpecContext) {
		checkctx, cancel := context.WithCancel(ctx)
		defer cancel()
		config := productionConfig(fixture, "disable", false)
		written := make(chan struct{})
		config.Connection.DialFunc = func(ctx context.Context,
			network,
			address string) (net.Conn,
			error) {
			connection,
				err := (&net.Dialer{}).DialContext(ctx,
				network,
				address)
			if err != nil {
				return nil,
					err
			}
			return &stalledQueryConnection{Conn: connection,
					written: written},
				nil
		}
		config.Connection.AfterConnect = func(_ context.Context,
			connection *pgconn.PgConn) error {
			connection.Conn().(*stalledQueryConnection).armed.Store(true)
			return nil
		}
		done := make(chan error, 1)
		go func() { done <- pgcheck.NewChecker().Check(checkctx, config) }()
		Eventually(written).WithContext(ctx).WithTimeout(3 * time.Second).Should(BeClosed())
		cancel()
		Eventually(done).WithContext(ctx).WithTimeout(2 * time.Second).Should(Receive(HaveOccurred()))
		Eventually(func() int {
			var connections int
			err := postgres.Admin.QueryRowContext(ctx,
				"SELECT count(*) FROM pg_stat_activity WHERE usename=$1",
				fixture.Role).Scan(&connections)
			if err != nil {
				return 100
			}
			return connections
		}).WithContext(ctx).WithTimeout(3 * time.Second).Should(BeNumerically("<=",
			1))
		Expect(pgcheck.NewChecker().Check(ctx, productionConfig(fixture, roleTLSMode, false))).To(Succeed())
	}, NodeTimeout(20*time.Second))

	It("rejects incorrect credentials and recovers with repaired input", func(ctx SpecContext) {
		checker := pgcheck.NewChecker()
		config := productionConfig(fixture, roleTLSMode, false)
		password := config.Connection.Password
		config.Connection.Password = "incorrect"
		Expect(failureReason(checker.Check(ctx, config))).To(Equal("ConnectionUnavailable"))
		config.Connection.Password = password
		Expect(checker.Check(ctx, config)).To(Succeed())
	}, NodeTimeout(20*time.Second))
	It("rejects a read-only session and recovers after restoring writability", func(ctx SpecContext) {
		checker := pgcheck.NewChecker()
		config := productionConfig(fixture, roleTLSMode, false)
		_,
			err := postgres.Admin.ExecContext(ctx,
			"ALTER ROLE "+quoteIdentifier(fixture.Role)+" SET default_transaction_read_only=on")
		Expect(err).NotTo(HaveOccurred())
		Expect(failureReason(checker.Check(ctx, config))).To(Equal("NotWritable"))
		_,
			err = postgres.Admin.ExecContext(ctx,
			"ALTER ROLE "+quoteIdentifier(fixture.Role)+" RESET default_transaction_read_only")
		Expect(err).NotTo(HaveOccurred())
		Expect(checker.Check(ctx, config)).To(Succeed())
	}, NodeTimeout(20*time.Second))
	It("requires client certificates when the server HBA demands them", func(ctx SpecContext) {
		setFixtureAuthentication(ctx, fixture, "client-cert")
		checker := pgcheck.NewChecker()
		Eventually(func() string {
			return failureReason(checker.Check(ctx, productionConfig(fixture, roleTLSMode, false)))
		}).WithContext(ctx).Should(Equal("ConnectionUnavailable"))
		Expect(checker.Check(ctx, productionConfig(fixture, roleTLSMode, true))).To(Succeed())
	}, NodeTimeout(25*time.Second))
	It("uses allow plaintext first and prefer TLS first with real transport policies", func(ctx SpecContext) {
		setFixtureAuthentication(ctx, fixture, "plaintext")
		checker := pgcheck.NewChecker()
		Expect(checker.Check(ctx, productionConfig(fixture, "allow", false))).To(Succeed())
		Expect(failureReason(checker.Check(ctx,
			productionConfig(fixture,
				tlsRequire,
				false)))).To(Equal("ConnectionUnavailable"))
	}, NodeTimeout(25*time.Second))
	It("falls back from plaintext to TLS for allow on a TLS-only login", func(ctx SpecContext) {
		setFixtureAuthentication(ctx, fixture, "tls")
		checker := pgcheck.NewChecker()
		Expect(failureReason(checker.Check(ctx,
			productionConfig(fixture,
				"disable",
				false)))).To(Equal("ConnectionUnavailable"))
		Expect(checker.Check(ctx, productionConfig(fixture, "allow", false))).To(Succeed())
		Expect(checker.Check(ctx, productionConfig(fixture, tlsPrefer, false))).To(Succeed())
	}, NodeTimeout(25*time.Second))
	It("accepts a trusted hostname mismatch only for verify-ca", func(ctx SpecContext) {
		checker := pgcheck.NewChecker()
		for _, mode := range []string{tlsVerifyCA, roleTLSMode} {
			config := productionConfig(fixture, mode, false)
			config.Connection.Host = "mismatched.example.test"
			if config.Connection.TLSConfig != nil {
				config.Connection.TLSConfig.ServerName = config.Connection.Host
			}
			config.Connection.DialFunc = func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, net.JoinHostPort(loopbackHost, strconv.Itoa(int(postgres.port))))
			}
			config.Connection.LookupFunc = func(context.Context, string) ([]string, error) { return []string{loopbackHost}, nil }
			err := checker.Check(ctx, config)
			if mode == tlsVerifyCA {
				Expect(err).NotTo(HaveOccurred())
			} else {
				Expect(failureReason(err)).To(Equal("ConnectionUnavailable"))
			}
		}
	}, NodeTimeout(20*time.Second))
	It("rejects an untrusted server in require when a custom CA was supplied", func(ctx SpecContext) {
		other, err := NewTLSFixture(GinkgoT().TempDir())
		Expect(err).NotTo(HaveOccurred())
		config := productionConfigUsingCA(fixture, tlsRequire, false, other.CA)
		Expect(failureReason(pgcheck.NewChecker().Check(ctx, config))).To(Equal("ConnectionUnavailable"))
	}, NodeTimeout(20*time.Second))
})
