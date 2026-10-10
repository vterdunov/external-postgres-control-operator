//go:build integration

package integration

import (
	"context"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	epco "github.com/vterdunov/external-postgres-control-operator/api/v1alpha1"
	pg "github.com/vterdunov/external-postgres-control-operator/internal/postgres"
	kubeenv "github.com/vterdunov/external-postgres-control-operator/test/envtest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func r4ACLBindings(database *epco.Database) []epco.ACLBinding {
	var bindings []epco.ACLBinding
	for _, target := range database.Status.ManagedACLTargets {
		bindings = append(bindings, target.Bindings...)
	}
	return bindings
}

func r4ChildACLKey(binding epco.ACLBinding) string {
	return fmt.Sprintf("%s/%s/%d/%d/%d/%d/%d", binding.Declaration, binding.Class,
		binding.ObjectOID, binding.SchemaOID, binding.CreatorOID, binding.GranteeOID, binding.GrantorOID)
}

func r4ChildACLs(database *epco.Database, grantee string) map[string]bool {
	bindings := map[string]bool{}
	for _, binding := range r4ACLBindings(database) {
		if binding.Scope != r3DatabaseScope && binding.Grantee == grantee && !binding.Administrative {
			bindings[r4ChildACLKey(binding)] = true
		}
	}
	return bindings
}

var _ = Describe("R4 connection refresh preserves R3 owner and child ACL responsibility", Ordered, func() {
	var environment *kubeenv.Environment
	BeforeAll(func(ctx SpecContext) { environment = r4StartAPI(ctx) }, NodeTimeout(60*time.Second))

	It("checkpoints every managed database grant before owner transfer and cleans child history after rotation",
		func(ctx SpecContext) {
			fixture := r4NewFixture(ctx, environment)
			fixture.bootstrap(ctx)
			target := fixture.target()
			_, err := target.ExecContext(ctx,
				"CREATE SCHEMA app AUTHORIZATION postgres; CREATE TABLE app.managed_values(value int); "+
					"INSERT INTO app.managed_values VALUES(42)")
			Expect(err).NotTo(HaveOccurred())
			fixture.editDatabase(ctx, func(spec *epco.DatabaseSpec) {
				grants := r3Permissions(fixture.Role, epco.EnsurePresent)
				spec.Permissions = &epco.DatabasePermissions{Connect: grants, Create: grants, Temporary: grants}
				spec.Schemas = []epco.SchemaSpec{{Name: databaseRecoveryApplicationName,
					Permissions: &epco.SchemaPermissions{Usage: grants},
					TablePermissions: []epco.NamedTablePermissions{{Name: "managed_values",
						Permissions: epco.TablePermissions{Select: grants}}},
					DefaultPrivileges: []epco.DefaultPrivileges{{ForRole: roleSQLAdmin,
						Tables: &epco.TablePermissions{Select: grants}}}}}
			})
			before := recoverDatabaseReady(ctx, fixture.api,
				databaseRecoveryReconciler(fixture.api, pg.NewDatabaseFactory()), fixture.database)
			children := r4ChildACLs(before, fixture.Role)
			Expect(len(children)).To(BeNumerically(">=", 3), "schema, named-table and default cleanup history exists")
			for _, privilege := range []string{r3Create, r3Temporary} {
				Expect(permissionDatabaseGrant(ctx, fixture.Fixture,
					string(fixture.database.Spec.Name), fixture.Role, privilege)).To(BeTrue())
			}
			newOwner, err := randomName("r4_owner_")
			Expect(err).NotTo(HaveOccurred())
			outside, err := randomName("r4_outside_")
			Expect(err).NotTo(HaveOccurred())
			for _, name := range []string{newOwner, outside} {
				roleSQL(ctx, fixture.Fixture, "CREATE ROLE "+quoteIdentifier(name)+" NOLOGIN")
				fixture.roles = append(fixture.roles, &epco.DatabaseRole{Spec: epco.DatabaseRoleSpec{Name: epco.SQLName(name)}})
			}
			_, err = target.ExecContext(ctx, "GRANT USAGE ON SCHEMA app TO "+quoteIdentifier(outside)+
				"; GRANT SELECT ON app.managed_values TO "+quoteIdentifier(outside)+
				"; ALTER DEFAULT PRIVILEGES FOR ROLE postgres IN SCHEMA app GRANT SELECT ON TABLES TO "+
				quoteIdentifier(outside))
			Expect(err).NotTo(HaveOccurred())
			roleSQL(ctx, fixture.Fixture, "GRANT CREATE ON DATABASE "+
				quoteIdentifier(string(fixture.database.Spec.Name))+" TO "+quoteIdentifier(outside))
			password, err := randomName("r4_owner_rotation_")
			Expect(err).NotTo(HaveOccurred())
			fixture.rotate(ctx, fixture.source, []byte(password))
			recoverUntilReady(ctx, fixture.api, recoveryReconciler(fixture.api, pg.NewRoleFactory()), fixture.role)
			fixture.editDatabase(ctx, func(spec *epco.DatabaseSpec) { spec.Owner = epco.SQLName(newOwner) })
			var checkpoint *epco.Database
			fault := &permissionRecoveryClient{Client: fixture.api}
			fault.reject = func(_ context.Context, candidate *epco.Database) bool {
				for _, binding := range r4ACLBindings(candidate) {
					if binding.Scope == r3DatabaseScope && binding.DesiredOwner != nil &&
						binding.DesiredOwner.Name == newOwner {
						checkpoint = candidate.DeepCopy()
						return true
					}
				}
				return false
			}
			Eventually(func() bool {
				_, _ = databaseRecoveryReconciler(fault, pg.NewDatabaseFactory()).Reconcile(ctx,
					ctrl.Request{NamespacedName: client.ObjectKeyFromObject(fixture.database)})
				return fault.failed.Load()
			}).WithContext(ctx).WithTimeout(5*time.Second).Should(BeTrue(), "owner-transfer checkpoint is rejectable")
			Expect(checkpoint).NotTo(BeNil())
			var owner string
			Expect(fixture.Server.Admin.QueryRowContext(ctx,
				"SELECT pg_get_userbyid(datdba) FROM pg_database WHERE datname=$1",
				string(fixture.database.Spec.Name)).Scan(&owner)).To(Succeed())
			Expect(owner).To(Equal(roleSQLAdmin), "failed durable intent must leave the SQL owner unchanged")
			prepared := map[string]bool{}
			for _, binding := range r4ACLBindings(checkpoint) {
				if binding.Scope == r3DatabaseScope && binding.Grantee == fixture.Role &&
					(binding.Privilege == r3Create || binding.Privilege == r3Temporary) {
					Expect(binding.PreviousOwner).NotTo(BeNil())
					Expect(binding.DesiredOwner).NotTo(BeNil())
					Expect(binding.PreviousOwner.Name).To(Equal(roleSQLAdmin))
					Expect(binding.DesiredOwner.Name).To(Equal(newOwner))
					prepared[binding.Privilege] = true
				}
			}
			Expect(prepared[r3Create] && prepared[r3Temporary]).To(BeTrue(),
				"CONNECT preparation also checkpoints every non-CONNECT database owner rewrite")
			preserved := r4ChildACLs(checkpoint, fixture.Role)
			for key := range children {
				Expect(preserved[key]).To(BeTrue(), "child cleanup identity survives the early CONNECT phase")
			}
			fixture.refreshed(ctx, fixture.database, r4OutputName, []byte(password), fixture.api)
			recoverDatabaseReady(ctx, fixture.api,
				databaseRecoveryReconciler(fixture.api, pg.NewDatabaseFactory()), fixture.database)
			Expect(fixture.Server.Admin.QueryRowContext(ctx,
				"SELECT pg_get_userbyid(datdba) FROM pg_database WHERE datname=$1",
				string(fixture.database.Spec.Name)).Scan(&owner)).To(Succeed())
			Expect(owner).To(Equal(newOwner))
			fixture.editDatabase(ctx, func(spec *epco.DatabaseSpec) {
				spec.Permissions.Create = nil
				spec.Permissions.Temporary = nil
				spec.Schemas = nil
			})
			recoverDatabaseReady(ctx, fixture.api,
				databaseRecoveryReconciler(fixture.api, pg.NewDatabaseFactory()), fixture.database)
			for _, privilege := range []string{r3Create, r3Temporary} {
				Expect(permissionDatabaseGrant(ctx, fixture.Fixture,
					string(fixture.database.Spec.Name), fixture.Role, privilege)).To(BeFalse(),
					"omitted non-CONNECT grants are removed after grantor rewrite")
			}
			Expect(permissionDatabaseGrant(ctx, fixture.Fixture,
				string(fixture.database.Spec.Name), outside, r3Create)).To(BeTrue())
			for _, role := range []string{fixture.Role, outside} {
				var usage, selectTable, defaults bool
				Expect(target.QueryRowContext(ctx, "SELECT has_schema_privilege($1,'app','USAGE'), "+
					"has_table_privilege($1,'app.managed_values','SELECT')", role).Scan(&usage, &selectTable)).To(Succeed())
				Expect(target.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_default_acl d
				 CROSS JOIN LATERAL aclexplode(d.defaclacl) a
				 WHERE d.defaclrole=(SELECT oid FROM pg_roles WHERE rolname='postgres')
				 AND d.defaclnamespace=(SELECT oid FROM pg_namespace WHERE nspname='app')
				 AND d.defaclobjtype='r' AND a.grantee=(SELECT oid FROM pg_roles WHERE rolname=$1)
				 AND a.privilege_type='SELECT')`, role).Scan(&defaults)).To(Succeed())
				if role == fixture.Role {
					Expect(usage || selectTable || defaults).To(BeFalse(), "all omitted child responsibilities are cleaned")
				} else {
					Expect(usage && selectTable && defaults).To(BeTrue(), "unrelated grants remain intact")
				}
			}
			_, err = target.ExecContext(ctx, "CREATE TABLE app.future_after(value int)")
			Expect(err).NotTo(HaveOccurred())
			var currentAccess, outsideAccess bool
			Expect(target.QueryRowContext(ctx, "SELECT has_table_privilege($1,'app.future_after','SELECT'), "+
				"has_table_privilege($2,'app.future_after','SELECT')", fixture.Role, outside).
				Scan(&currentAccess, &outsideAccess)).To(Succeed())
			Expect(currentAccess).To(BeFalse())
			Expect(outsideAccess).To(BeTrue(), "future objects retain only the unrelated default grant")
			var value int
			Expect(target.QueryRowContext(ctx, "SELECT value FROM app.managed_values").Scan(&value)).To(Succeed())
			Expect(value).To(Equal(42), "ACL omission preserves schema, table and data")
			r4Password(fixture.output(ctx, r4OutputName), []byte(password))
			Expect(fixture.login(ctx, string(fixture.database.Spec.Name), fixture.Role, password)).To(BeTrue())
		}, NodeTimeout(60*time.Second))
})
