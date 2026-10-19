//go:build integration

package integration

import (
	"bytes"
	"errors"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	epco "github.com/vterdunov/external-postgres-control-operator/api/v1alpha1"
	pg "github.com/vterdunov/external-postgres-control-operator/internal/postgres"
	kubeenv "github.com/vterdunov/external-postgres-control-operator/test/envtest"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const r4DatabaseWildcard = "database"

var _ = Describe("R4 actual libpq and pgJDBC application consumers", Ordered, ContinueOnFailure, func() {
	var environment *kubeenv.Environment
	var fixture *r4ConnectionFixture
	var consumers *r4ApplicationClients
	BeforeAll(func(ctx SpecContext) {
		environment = r4StartAPI(ctx)
		consumers = r4NewApplicationClients(ctx)
	}, NodeTimeout(90*time.Second))
	BeforeEach(func(ctx SpecContext) { fixture = r4NewFixture(ctx, environment) }, NodeTimeout(30*time.Second))
	makeSpecialNames := func(ctx SpecContext, wildcard string) {
		prefix, err := randomName("r4_")
		Expect(err).NotTo(HaveOccurred())
		role, name := prefix+":\\* role/%+日本", prefix+":\\* db/%+日本"
		switch wildcard {
		case "user":
			role = "*"
		case r4DatabaseWildcard:
			name = "*"
		}
		application, source := fixture.addRole(ctx, "special-app", role, string(r4SpecialPassword()))
		fixture.role, fixture.source = application, source
		database := &epco.Database{Name: "special-db", Namespace: fixture.scope.Namespace.Name,
			Spec: epco.DatabaseSpec{Name: epco.SQLName(name), Owner: roleSQLAdmin,
				Cluster: epco.ClusterReference{Name: fixture.cluster.Name},
				ConnectionSecrets: []epco.ConnectionSecret{{Name: r4OutputName,
					RoleRef: epco.LocalReference{Name: application.Name}}}}}
		// Retire the unused default declaration so two live Databases cannot
		// legitimately claim the same output name during this formatting case.
		original := fixture.database
		fixture.editDatabase(ctx, func(spec *epco.DatabaseSpec) { spec.ConnectionSecrets = nil })
		fixture.database = database
		Expect(fixture.scope.Create(ctx, database)).To(Succeed())
		fixture.dbs = append(fixture.dbs, database)
		Expect(client.ObjectKeyFromObject(original)).NotTo(Equal(client.ObjectKeyFromObject(database)))
	}
	bootstrapSpecial := func(ctx SpecContext) *corev1.Secret {
		recoverUntilReady(ctx, fixture.api, recoveryReconciler(fixture.api, pg.NewRoleFactory()), fixture.role)
		output := fixture.published(ctx, fixture.database, r4OutputName)
		Eventually(func() bool {
			fixture.reconcile(ctx, fixture.database, fixture.api)
			exists, err := fixture.Server.DatabaseExists(ctx, string(fixture.database.Spec.Name))
			return err == nil && exists && fixture.login(ctx, string(fixture.database.Spec.Name),
				string(fixture.role.Spec.Name), string(r4SpecialPassword()))
		}).WithContext(ctx).WithTimeout(5*time.Second).Should(BeTrue(), "special application inputs are accepted by SQL")
		r4Password(output, r4SpecialPassword())
		return output
	}
	It("decodes IPv6, one database segment and percent-only CA query values using both actual parsers",
		func(ctx SpecContext) {
			makeSpecialNames(ctx, "")
			path := consumers.publicCA(ctx, fixture)
			cluster := &epco.ExternalPostgres{}
			Expect(fixture.api.Get(ctx, client.ObjectKeyFromObject(fixture.cluster), cluster)).To(Succeed())
			// Own a local IPv6 endpoint that immediately rejects SQL startup.
			// An external documentation address can blackhole until the operator's
			// connection deadline, obscuring this parser and early-export contract.
			listener, err := net.Listen("tcp6", "[::1]:0")
			Expect(err).NotTo(HaveOccurred())
			var connections sync.WaitGroup
			connections.Go(func() {
				defer GinkgoRecover()
				for {
					connection, acceptErr := listener.Accept()
					if errors.Is(acceptErr, net.ErrClosed) {
						return
					}
					Expect(acceptErr).NotTo(HaveOccurred())
					Expect(connection.Close()).To(Succeed())
				}
			})
			DeferCleanup(func() {
				Expect(listener.Close()).To(Succeed())
				connections.Wait()
			})
			cluster.Spec.Connection.Host = "::1"
			cluster.Spec.Connection.Port = int32(listener.Addr().(*net.TCPAddr).Port)
			Expect(fixture.api.Update(ctx, cluster)).To(Succeed())
			output := fixture.published(ctx, fixture.database, r4OutputName)
			Expect(string(output.Data["sslrootcert"])).To(Equal(path))
			Expect(strings.Contains(string(output.Data["uri"]), "%20")).To(BeTrue())
			Expect(strings.Contains(string(output.Data["uri"]), "%2B")).To(BeTrue())
			Expect(consumers.consume(ctx, output, r4LibPQDriver, "parse")).To(BeTrue(), "libpq decodes exact application inputs")
			Expect(consumers.consume(ctx, output, r4JDBCDriver, "parse")).To(BeTrue(), "pgJDBC decodes exact application inputs")
		}, NodeTimeout(35*time.Second))
	It("preserves an explicitly empty source password in both actual client parsers", func(ctx SpecContext) {
		consumers.publicCA(ctx, fixture)
		fixture.rotate(ctx, fixture.source, []byte{})
		output := fixture.published(ctx, fixture.database, r4OutputName)
		_, present := output.Data["password"]
		Expect(present).To(BeTrue())
		r4Password(output, []byte{})
		for _, driver := range []string{r4LibPQDriver, r4JDBCDriver} {
			Expect(consumers.consume(ctx, output, driver, "parse")).To(BeTrue(), "empty password parser "+driver)
		}
	}, NodeTimeout(35*time.Second))
	It("connects with both URI families and the emitted private pgpass including colon and backslash values",
		func(ctx SpecContext) {
			makeSpecialNames(ctx, "")
			consumers.publicCA(ctx, fixture)
			output := bootstrapSpecial(ctx)
			for _, driver := range []string{r4LibPQDriver, r4JDBCDriver} {
				Expect(consumers.consume(ctx, output, driver, "parse")).To(BeTrue())
				Expect(consumers.consume(ctx, output, driver, "connect")).To(BeTrue(), "actual application consumer "+driver)
			}
			Expect(consumers.pgpass(ctx, output, string(fixture.database.Spec.Name),
				string(fixture.role.Spec.Name))).To(BeTrue(),
				"libpq authenticates through the emitted escaped passfile")
		}, NodeTimeout(45*time.Second))
	It("escapes a literal pgpass wildcard so it cannot authenticate an unrelated login", func(ctx SpecContext) {
		makeSpecialNames(ctx, "user")
		consumers.publicCA(ctx, fixture)
		output := bootstrapSpecial(ctx)
		Expect(bytes.Contains(output.Data["pgpass"], []byte(`:\*:`))).To(BeTrue(), "literal user wildcard is escaped")
		Expect(consumers.pgpass(ctx, output, string(fixture.database.Spec.Name), string(fixture.role.Spec.Name))).To(BeTrue())
		other, err := randomName("r4_wildcard_")
		Expect(err).NotTo(HaveOccurred())
		role, _ := fixture.addRole(ctx, "wildcard-control", other, string(r4SpecialPassword()))
		recoverUntilReady(ctx, fixture.api, recoveryReconciler(fixture.api, pg.NewRoleFactory()), role)
		Expect(consumers.pgpass(ctx, output, string(fixture.database.Spec.Name), other)).To(BeFalse(),
			"escaped wildcard cannot supply another user's password")
	}, NodeTimeout(45*time.Second))
	It("escapes a literal database wildcard so the passfile cannot authenticate in another accessible database",
		func(ctx SpecContext) {
			makeSpecialNames(ctx, r4DatabaseWildcard)
			consumers.publicCA(ctx, fixture)
			output := bootstrapSpecial(ctx)
			Expect(bytes.Contains(output.Data["pgpass"], []byte(`:\*:`))).To(BeTrue(), "literal database wildcard is escaped")
			name := string(fixture.role.Spec.Name)
			Expect(consumers.pgpass(ctx, output, "*", name)).To(BeTrue())
			roleSQL(ctx, fixture.Fixture,
				"GRANT CONNECT ON DATABASE "+quoteIdentifier(fixture.Database)+" TO "+quoteIdentifier(name))
			DeferCleanup(func(ctx SpecContext) {
				roleSQL(ctx, fixture.Fixture,
					"REVOKE CONNECT ON DATABASE "+quoteIdentifier(fixture.Database)+" FROM "+quoteIdentifier(name))
			})
			Expect(fixture.login(ctx, fixture.Database, name, string(r4SpecialPassword()))).To(BeTrue(),
				"fixture grants make the alternative database usable with an explicit password")
			Expect(consumers.pgpass(ctx, output, fixture.Database, name)).To(BeFalse(),
				"escaped database wildcard cannot supply credentials to another accessible database")
		}, NodeTimeout(45*time.Second))
	It("matches require-plus-CA verification in libpq and JDBC and both reject an untrusted CA", func(ctx SpecContext) {
		consumers.publicCA(ctx, fixture)
		cluster := &epco.ExternalPostgres{}
		Expect(fixture.api.Get(ctx, client.ObjectKeyFromObject(fixture.cluster), cluster)).To(Succeed())
		cluster.Spec.TLS.Mode = tlsRequire
		Expect(fixture.api.Update(ctx, cluster)).To(Succeed())
		output := fixture.bootstrap(ctx)
		Expect(string(output.Data["sslmode"])).To(Equal(tlsRequire))
		for _, driver := range []string{r4LibPQDriver, r4JDBCDriver} {
			Expect(consumers.consume(ctx, output, driver, "parse")).To(BeTrue())
			Expect(consumers.consume(ctx, output, driver, "connect")).To(BeTrue())
		}
		untrusted, err := NewTLSFixture(GinkgoT().TempDir())
		Expect(err).NotTo(HaveOccurred())
		ca, err := os.ReadFile(untrusted.CA)
		Expect(err).NotTo(HaveOccurred())
		Expect(os.WriteFile(string(output.Data["sslrootcert"]), ca, 0600)).To(Succeed())
		for _, driver := range []string{r4LibPQDriver, r4JDBCDriver} {
			Expect(consumers.consume(ctx, output, driver, "deny")).To(BeTrue(),
				"the explicit untrusted public CA is rejected by "+driver)
		}
	}, NodeTimeout(45*time.Second))
})
