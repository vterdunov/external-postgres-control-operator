//go:build integration

package integration

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	epco "github.com/vterdunov/external-postgres-control-operator/api/v1alpha1"
	pg "github.com/vterdunov/external-postgres-control-operator/internal/postgres"
	kubeenv "github.com/vterdunov/external-postgres-control-operator/test/envtest"
	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type extensionAPIFixture struct {
	*extensionSQLFixture
	scope    *kubeenv.Scope
	database *epco.Database
}

func newExtensionAPIFixture(ctx context.Context, environment *kubeenv.Environment) *extensionAPIFixture {
	f := &extensionAPIFixture{extensionSQLFixture: newExtensionSQLFixture(ctx)}
	var err error
	f.scope, _, err = environment.NewScope(ctx, "r5-extension-acl-")
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	DeferCleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		Expect(f.scope.Cleanup(cleanup)).To(Succeed())
	})
	cluster := recoveryCluster(ctx, f.scope, f.Fixture)
	cluster.Spec.Connection.Database = roleSQLAdmin
	ExpectWithOffset(1, environment.Client.Update(ctx, cluster)).To(Succeed())
	f.database = &epco.Database{Name: "extensions", Namespace: f.scope.Namespace.Name, Spec: epco.DatabaseSpec{
		Name: epco.SQLName(f.Database), Owner: roleSQLAdmin, Cluster: epco.ClusterReference{Name: cluster.Name},
		ReclaimPolicy: epco.ReclaimRetain,
		Schemas:       []epco.SchemaSpec{{Name: "objects"}},
	}}
	return f
}

func (f *extensionAPIFixture) ready(ctx context.Context, api client.Client) *epco.Database {
	return recoverDatabaseReady(ctx, api, databaseRecoveryReconciler(api, pg.NewDatabaseFactory()), f.database)
}

var _ = Describe("Database extension ACL composition with real API and PostgreSQL", Ordered,
	ContinueOnFailure, func() {
		var environment *kubeenv.Environment
		var fixture *extensionAPIFixture
		BeforeAll(func(ctx SpecContext) { environment = r4StartAPI(ctx) }, NodeTimeout(60*time.Second))
		BeforeEach(func(ctx SpecContext) { fixture = newExtensionAPIFixture(ctx, environment) }, NodeTimeout(30*time.Second))

		It("discovers extension-created tables and sequences before broad grants and retires dropped identities",
			func(ctx SpecContext) {
				pkg := fixture.packageFiles(ctx, "",
					map[string]string{"--omega.sql": "CREATE TABLE @members@ (value integer); " +
						"INSERT INTO @members@ VALUES (42); CREATE SEQUENCE @sequence@;"})
				grantee := []epco.Permission{{Name: epco.SQLName(fixture.Role)}}
				fixture.database.Spec.Extensions = []epco.ExtensionSpec{extensionSpec(pkg.Name, "", "objects", "")}
				fixture.database.Spec.Schemas[0].Permissions = &epco.SchemaPermissions{Usage: grantee,
					Tables: &epco.TablePermissions{Select: grantee}, Sequences: &epco.SequencePermissions{Usage: grantee}}
				Expect(fixture.scope.Create(ctx, fixture.database)).To(Succeed())
				current := fixture.ready(ctx, environment.Client)
				var value int
				Expect(fixture.Limited.QueryRowContext(ctx, "SELECT value FROM objects."+quoteIdentifier(pkg.Name+
					"_members")).Scan(&value)).To(Succeed())
				Expect(value).To(Equal(42))
				var sequence int64
				Expect(fixture.Limited.QueryRowContext(ctx, "SELECT pg_catalog.nextval($1::regclass)", "objects."+
					quoteIdentifier(pkg.Name+"_sequence")).Scan(&sequence)).To(Succeed())
				Expect(sequence).To(Equal(int64(1)))
				var tableOID, sequenceOID uint32
				Expect(fixture.Admin.QueryRowContext(ctx,
					"SELECT pg_catalog.to_regclass($1)::oid, pg_catalog.to_regclass($2)::oid", "objects."+
						quoteIdentifier(pkg.Name+"_members"), "objects."+quoteIdentifier(pkg.Name+
						"_sequence")).Scan(&tableOID, &sequenceOID)).To(Succeed())
				var managed []uint32
				for _, target := range current.Status.ManagedACLTargets {
					for _, binding := range target.Bindings {
						managed = append(managed, binding.ObjectOID)
					}
				}
				Expect(managed).To(ContainElements(tableOID, sequenceOID))
				current.Spec.Extensions[0].Ensure = epco.EnsureAbsent
				current.Spec.Schemas[0].Permissions = nil
				Expect(environment.Client.Update(ctx, current)).To(Succeed())
				current = fixture.ready(ctx, environment.Client)
				Expect(extensionCatalog(ctx, fixture.Admin, pkg.Name).Exists).To(BeFalse())
				for _, target := range current.Status.ManagedACLTargets {
					for _, binding := range target.Bindings {
						Expect(binding.ObjectOID).NotTo(BeElementOf([]uint32{tableOID, sequenceOID}),
							"dropped members must leave no cleanup responsibility")
					}
				}
				fixture.sql(ctx, "CREATE TABLE objects."+quoteIdentifier(pkg.Name+"_members")+
					" (value integer); INSERT INTO objects."+quoteIdentifier(pkg.Name+"_members")+" VALUES (81)")
				fixture.ready(ctx, environment.Client)
				var granted bool
				Expect(fixture.Admin.QueryRowContext(ctx, "SELECT pg_catalog.has_table_privilege($1,$2,'SELECT')",
					fixture.Role, "objects."+quoteIdentifier(pkg.Name+"_members")).Scan(&granted)).To(Succeed())
				Expect(granted).To(BeFalse(), "replacement table must not inherit an old relation identity grant")
			}, NodeTimeout(45*time.Second))

		It("does not apply newly declared defaults retroactively to extension script tables", func(ctx SpecContext) {
			pkg := fixture.packageFiles(ctx, "", nil)
			fixture.database.Spec.Extensions = []epco.ExtensionSpec{extensionSpec(pkg.Name, "", "objects", "")}
			fixture.database.Spec.Schemas[0].DefaultPrivileges = []epco.DefaultPrivileges{{ForRole: roleSQLAdmin,
				Tables: &epco.TablePermissions{Select: []epco.Permission{{Name: epco.SQLName(fixture.Role)}}}}}
			Expect(fixture.scope.Create(ctx, fixture.database)).To(Succeed())
			fixture.ready(ctx, environment.Client)
			var allowed bool
			Expect(fixture.Admin.QueryRowContext(ctx, "SELECT pg_catalog.has_table_privilege($1,$2,'SELECT')",
				fixture.Role, "objects."+quoteIdentifier(pkg.Name+"_members")).Scan(&allowed)).To(Succeed())
			Expect(allowed).To(BeFalse(), "new defaults follow the extension phase and affect only future objects")
			fixture.sql(ctx, "CREATE TABLE objects.future (value integer)")
			Expect(fixture.Admin.QueryRowContext(ctx,
				"SELECT pg_catalog.has_table_privilege($1,'objects.future','SELECT')", fixture.Role).Scan(&allowed)).To(Succeed())
			Expect(allowed).To(BeTrue())
		}, NodeTimeout(45*time.Second))

		It("honors preexisting defaults of the bootstrap script creator for trusted restricted installs",
			func(ctx SpecContext) {
				pkg := fixture.packageFiles(ctx, "", nil)
				fixture.sql(ctx, "CREATE SCHEMA objects; GRANT CREATE ON DATABASE "+quoteIdentifier(fixture.Database)+
					" TO "+quoteIdentifier(fixture.Role)+"; GRANT USAGE,CREATE ON SCHEMA objects TO "+
					quoteIdentifier(fixture.Role)+
					"; ALTER DEFAULT PRIVILEGES FOR ROLE postgres IN SCHEMA objects GRANT SELECT ON TABLES TO "+
					quoteIdentifier(fixture.Role))
				secret := &corev1.Secret{}
				Expect(environment.Client.Get(ctx, client.ObjectKey{Namespace: fixture.scope.Namespace.Name,
					Name: recoveryAdminSecret}, secret)).To(Succeed())
				secret.Data[corev1.BasicAuthUsernameKey] = []byte(fixture.Role)
				secret.Data[corev1.BasicAuthPasswordKey] = []byte(fixture.Password)
				Expect(environment.Client.Update(ctx, secret)).To(Succeed())
				fixture.database.Spec.Extensions = []epco.ExtensionSpec{extensionSpec(pkg.Name, "", "objects", "")}
				Expect(fixture.scope.Create(ctx, fixture.database)).To(Succeed())
				fixture.ready(ctx, environment.Client)
				Expect(extensionCatalog(ctx, fixture.Admin, pkg.Name).Owner).To(Equal(fixture.Role))
				var owner string
				Expect(fixture.Admin.QueryRowContext(ctx,
					"SELECT pg_catalog.pg_get_userbyid(relowner) FROM pg_catalog.pg_class WHERE "+
						"oid=pg_catalog.to_regclass($1)", "objects."+quoteIdentifier(pkg.Name+"_members")).Scan(&owner)).To(Succeed())
				Expect(owner).To(Equal(roleSQLAdmin), "trusted script objects use bootstrap creator defaults")
				var value int
				Expect(fixture.Limited.QueryRowContext(ctx, "SELECT value FROM objects."+quoteIdentifier(pkg.Name+
					"_members")).Scan(&value)).To(Succeed())
				Expect(value).To(Equal(3))
			}, NodeTimeout(45*time.Second))

		DescribeTable("checkpoints extension-only operator CONNECT restoration before SQL", func(ctx SpecContext,
			failCheckpoint bool) {
			pkg := fixture.packageFiles(ctx, "", nil)
			fixture.sql(ctx, "ALTER DATABASE "+quoteIdentifier(fixture.Database)+" OWNER TO "+quoteIdentifier(fixture.Role)+
				"; REVOKE CONNECT ON DATABASE "+quoteIdentifier(fixture.Database)+" FROM "+quoteIdentifier(fixture.Role))
			fixture.database.Spec.Owner = epco.SQLName(fixture.Role)
			fixture.database.Spec.Schemas = nil
			fixture.database.Spec.Extensions = []epco.ExtensionSpec{extensionSpec(pkg.Name, "", "", "")}
			secret := &corev1.Secret{}
			Expect(environment.Client.Get(ctx, client.ObjectKey{Namespace: fixture.scope.Namespace.Name,
				Name: recoveryAdminSecret}, secret)).To(Succeed())
			secret.Data[corev1.BasicAuthUsernameKey] = []byte(fixture.Role)
			secret.Data[corev1.BasicAuthPasswordKey] = []byte(fixture.Password)
			Expect(environment.Client.Update(ctx, secret)).To(Succeed())
			Expect(fixture.scope.Create(ctx, fixture.database)).To(Succeed())
			connected := func() bool {
				var allowed bool
				Expect(fixture.Server.Admin.QueryRowContext(ctx,
					"SELECT pg_catalog.has_database_privilege($1,$2,'CONNECT')", fixture.Role, fixture.Database).Scan(
					&allowed)).To(Succeed())
				return allowed
			}
			Expect(connected()).To(BeFalse(), "the administrative database remains reachable while target CONNECT is absent")
			if failCheckpoint {
				denied := &r3StatusClient{Client: environment.Client, reject: func(database *epco.Database) bool {
					for _, target := range database.Status.ManagedACLTargets {
						for _, binding := range target.Bindings {
							if binding.Administrative && binding.Privilege == r3Connect && binding.Pending == "grant" {
								return true
							}
						}
					}
					return false
				}}
				_, _ = databaseRecoveryReconciler(denied, pg.NewDatabaseFactory()).Reconcile(ctx,
					ctrl.Request{NamespacedName: client.ObjectKeyFromObject(fixture.database)})
				Expect(denied.failed.Load()).To(BeNumerically(">", 0), "required access must reach a pre-SQL durable checkpoint")
				Expect(connected()).To(BeFalse(), "failed intent checkpoint forbids the corresponding CONNECT grant")
				Expect(extensionCatalog(ctx, fixture.Admin, pkg.Name).Exists).To(BeFalse())
			}
			current := fixture.ready(ctx, environment.Client)
			Expect(connected()).To(BeTrue())
			Expect(extensionCatalog(ctx, fixture.Admin, pkg.Name).Owner).To(Equal(fixture.Role))
			var recorded bool
			for _, target := range current.Status.ManagedACLTargets {
				for _, binding := range target.Bindings {
					if binding.Administrative && binding.Privilege == r3Connect && binding.Grantee == fixture.Role {
						recorded = true
					}
				}
			}
			Expect(recorded).To(BeTrue(), "confirmed restored access remains represented in R3 responsibility")
		}, Entry("successful durable grant", false, NodeTimeout(45*time.Second)), Entry(
			"failed checkpoint and ordinary retry", true, NodeTimeout(45*time.Second)))
	})
