//go:build integration

package integration

import (
	"context"
	"errors"
	"os"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var postgres *Server

var _ = BeforeSuite(func(ctx SpecContext) {
	var err error
	postgres, err = Start(ctx, Options{
		Version: os.Getenv("EPCO_PG_VERSION"), RunID: os.Getenv("EPCO_TEST_RUN_ID"),
		ArtifactsDir: os.Getenv("EPCO_ARTIFACTS_DIR"),
	})
	Expect(err).NotTo(HaveOccurred())
}, NodeTimeout(3*time.Minute))

var _ = ReportAfterSuite("PostgreSQL diagnostics and teardown", func(report Report) {
	if postgres == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var diagnosticErr error
	if !report.SuiteSucceeded {
		diagnosticErr = postgres.Diagnostics(ctx)
	}
	cleanupErr := postgres.Close(ctx)
	Expect(errors.Join(diagnosticErr, cleanupErr)).NotTo(HaveOccurred())
})

var _ = Describe("PostgreSQL fixtures", func() {
	var fixture *Fixture
	BeforeEach(func(ctx SpecContext) {
		var err error
		fixture, err = postgres.NewFixture(ctx)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			Expect(fixture.Close(ctx)).To(Succeed())
		})
	}, NodeTimeout(20*time.Second))

	It("connects through verified TLS and creates a genuinely restricted login", func(ctx SpecContext) {
		var ssl, superuser, createdb, createrole bool
		err := fixture.Limited.QueryRowContext(ctx, "SELECT ssl FROM pg_stat_ssl WHERE pid=pg_backend_pid()").Scan(&ssl)
		Expect(err).NotTo(HaveOccurred())
		Expect(ssl).To(BeTrue())
		err = fixture.Admin.QueryRowContext(ctx,
			"SELECT rolsuper,rolcreatedb,rolcreaterole FROM pg_roles WHERE rolname=$1", fixture.Role).
			Scan(&superuser, &createdb, &createrole)
		Expect(err).NotTo(HaveOccurred())
		Expect(superuser).To(BeFalse())
		Expect(createdb).To(BeFalse())
		Expect(createrole).To(BeFalse())
		exists, err := postgres.DatabaseExists(ctx, fixture.Database)
		Expect(err).NotTo(HaveOccurred())
		Expect(exists).To(BeTrue())
	}, NodeTimeout(20*time.Second))

	It("verifies catalog privileges and actual grant/revoke behavior", func(ctx SpecContext) {
		_, err := fixture.Admin.ExecContext(ctx,
			"CREATE TABLE public.fixture_values (value int); INSERT INTO public.fixture_values VALUES (42)")
		Expect(err).NotTo(HaveOccurred())
		allowed, err := fixture.HasTablePrivilege(ctx, "public.fixture_values", "SELECT")
		Expect(err).NotTo(HaveOccurred())
		Expect(allowed).To(BeFalse())
		var value int
		err = fixture.Limited.QueryRowContext(ctx, "SELECT value FROM public.fixture_values").Scan(&value)
		Expect(SQLState(err)).To(Equal("42501"))
		_, err = fixture.Admin.ExecContext(ctx, "GRANT SELECT ON public.fixture_values TO "+quoteIdentifier(fixture.Role))
		Expect(err).NotTo(HaveOccurred())
		allowed, err = fixture.HasTablePrivilege(ctx, "public.fixture_values", "SELECT")
		Expect(err).NotTo(HaveOccurred())
		Expect(allowed).To(BeTrue())
		Expect(fixture.Limited.QueryRowContext(ctx, "SELECT value FROM public.fixture_values").Scan(&value)).To(Succeed())
		Expect(value).To(Equal(42))
		_, err = fixture.Admin.ExecContext(ctx, "REVOKE SELECT ON public.fixture_values FROM "+quoteIdentifier(fixture.Role))
		Expect(err).NotTo(HaveOccurred())
		err = fixture.Limited.QueryRowContext(ctx, "SELECT value FROM public.fixture_values").Scan(&value)
		Expect(SQLState(err)).To(Equal("42501"))
	}, NodeTimeout(20*time.Second))

	It("isolates concurrent fixtures and refuses cross-database access", func(ctx SpecContext) {
		other, err := postgres.NewFixture(ctx)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			Expect(other.Close(cleanupCtx)).To(Succeed())
		})
		Expect(other.Database).NotTo(Equal(fixture.Database))
		Expect(other.Role).NotTo(Equal(fixture.Role))
		unauthorized, err := postgres.Open(other.Database, fixture.Role, fixture.Password)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { Expect(unauthorized.Close()).To(Succeed()) })
		Expect(SQLState(unauthorized.PingContext(ctx))).To(Equal("42501"))
		_, err = fixture.Admin.ExecContext(ctx, "CREATE TABLE public.only_first (value int)")
		Expect(err).NotTo(HaveOccurred())
		var exists bool
		err = other.Admin.QueryRowContext(ctx, "SELECT to_regclass('public.only_first') IS NOT NULL").Scan(&exists)
		Expect(err).NotTo(HaveOccurred())
		Expect(exists).To(BeFalse())
	}, NodeTimeout(20*time.Second))

	It("rejects incorrect credentials and untrusted certificates", func(ctx SpecContext) {
		invalid, err := postgres.Open(fixture.Database, fixture.Role, "invalid-credentials")
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { Expect(invalid.Close()).To(Succeed()) })
		Expect(SQLState(invalid.PingContext(ctx))).To(Equal("28P01"))
		untrusted, err := NewTLSFixture(GinkgoT().TempDir())
		Expect(err).NotTo(HaveOccurred())
		altered := *postgres
		altered.TLS = untrusted
		invalidTLS, err := altered.Open(fixture.Database, fixture.Role, fixture.Password)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { Expect(invalidTLS.Close()).To(Succeed()) })
		Expect(invalidTLS.PingContext(ctx)).NotTo(Succeed())
	}, NodeTimeout(20*time.Second))

	It("drops all owned objects even when a session is still connected", func(ctx SpecContext) {
		held, err := postgres.Open(fixture.Database, fixture.Role, fixture.Password)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { Expect(held.Close()).To(Succeed()) })
		Expect(held.PingContext(ctx)).To(Succeed())
		Expect(fixture.Close(ctx)).To(Succeed())
		exists, err := postgres.DatabaseExists(ctx, fixture.Database)
		Expect(err).NotTo(HaveOccurred())
		Expect(exists).To(BeFalse())
		exists, err = postgres.RoleExists(ctx, fixture.Role)
		Expect(err).NotTo(HaveOccurred())
		Expect(exists).To(BeFalse())
		Expect(held.PingContext(ctx)).NotTo(Succeed())
		Expect(fixture.Close(ctx)).To(Succeed())
	}, NodeTimeout(20*time.Second))
})
