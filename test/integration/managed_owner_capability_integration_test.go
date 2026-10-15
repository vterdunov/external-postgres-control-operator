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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const managedOwnerDatabaseObserve = "database-observe"

func managedOwnerIntegrationFixture(ctx context.Context, environment *kubeenv.Environment) *r4ConnectionFixture {
	f := r4NewFixture(ctx, environment)
	// The reused connection fixture declares its limited login as an application role.
	// Here that login is the administrative identity, so it must not have a competing unready manager.
	ExpectWithOffset(1, f.api.Delete(ctx, f.role)).To(Succeed())
	roleSQL(ctx, f.Fixture, "ALTER ROLE "+quoteIdentifier(f.Role)+" CREATEDB CREATEROLE NOINHERIT")
	var version int
	ExpectWithOffset(1, f.Admin.QueryRowContext(ctx, "SHOW server_version_num").Scan(&version)).To(Succeed())
	if version >= 160000 {
		roleSQL(ctx, f.Fixture, "ALTER ROLE "+quoteIdentifier(f.Role)+" SET createrole_self_grant=''")
	}
	secret := &corev1.Secret{}
	ExpectWithOffset(1, f.api.Get(ctx, client.ObjectKey{
		Namespace: f.cluster.Namespace, Name: recoveryAdminSecret}, secret)).To(Succeed())
	secret.Data[corev1.BasicAuthUsernameKey], secret.Data[corev1.BasicAuthPasswordKey] = []byte(f.Role), []byte(f.Password)
	ExpectWithOffset(1, f.api.Update(ctx, secret)).To(Succeed())
	return f
}

func managedOwnerIntegrationRole(ctx context.Context, f *r4ConnectionFixture, name string) *epco.DatabaseRole {
	sqlName, err := randomName("mo_" + name + "_")
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	role, _ := f.addRole(ctx, name, sqlName, "isolated-managed-owner-password")
	return recoverUntilReady(ctx, f.api, recoveryReconciler(f.api, pg.NewRoleFactory()), role)
}

func managedOwnerIntegrationSet(ctx context.Context, f *r4ConnectionFixture, name string) bool {
	var version int
	ExpectWithOffset(1, f.Admin.QueryRowContext(ctx, "SHOW server_version_num").Scan(&version)).To(Succeed())
	capability := databaseMemberCapability
	if version >= 160000 {
		capability = databaseSetCapability
	}
	var access bool
	ExpectWithOffset(1, f.Admin.QueryRowContext(ctx, "SELECT pg_has_role($1,$2,$3)",
		f.Role, name, capability).Scan(&access)).To(Succeed())
	return access
}

func managedOwnerIntegrationReady(ctx context.Context, f *r4ConnectionFixture) *epco.Database {
	reconciler := databaseRecoveryReconciler(f.api, pg.NewDatabaseFactory())
	Eventually(func(g Gomega) {
		_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(f.database)})
		g.Expect(err).NotTo(HaveOccurred())
		current := recoveredDatabase(ctx, f.api, f.database)
		condition := meta.FindStatusCondition(current.Status.Conditions, "Ready")
		g.Expect(condition).NotTo(BeNil())
		g.Expect(condition.Status).To(Equal(metav1.ConditionTrue), "database condition: %+v", condition)
	}).WithContext(ctx).WithTimeout(10 * time.Second).Should(Succeed())
	return recoveredDatabase(ctx, f.api, f.database)
}

var _ = Describe("Managed owner capability API and PostgreSQL", Ordered, ContinueOnFailure, func() {
	var environment *kubeenv.Environment
	BeforeAll(func(ctx SpecContext) { environment = r4StartAPI(ctx) }, NodeTimeout(90*time.Second))

	It("repairs a default creator before ACL planning and historical cleanup "+
		"without adopting an ordinary grantee", func(ctx SpecContext) {
		f := managedOwnerIntegrationFixture(ctx, environment)
		creator := managedOwnerIntegrationRole(ctx, f, "creator")
		reader := managedOwnerIntegrationRole(ctx, f, "reader")
		f.editDatabase(ctx, func(spec *epco.DatabaseSpec) {
			spec.Owner = epco.SQLName(f.Role)
			spec.ConnectionSecrets = nil
			spec.Schemas = []epco.SchemaSpec{{Name: "app", Permissions: &epco.SchemaPermissions{
				Create: []epco.Permission{{Name: creator.Spec.Name}},
				Usage:  []epco.Permission{{Name: reader.Spec.Name}, {Name: creator.Spec.Name}}},
				DefaultPrivileges: []epco.DefaultPrivileges{{ForRole: creator.Spec.Name,
					Tables: &epco.TablePermissions{Select: []epco.Permission{{Name: reader.Spec.Name}}}}}}}
		})
		managedOwnerIntegrationReady(ctx, f)
		Expect(managedOwnerIntegrationSet(ctx, f, string(creator.Spec.Name))).To(BeTrue())
		Expect(managedOwnerIntegrationSet(ctx, f, string(reader.Spec.Name))).To(BeFalse(),
			"ordinary grantee must not become a service SET target")
		target := f.target()
		_, err := target.ExecContext(ctx, "SET ROLE "+quoteIdentifier(string(creator.Spec.Name))+
			"; CREATE TABLE app.probe(value int); INSERT INTO app.probe VALUES(42); RESET ROLE")
		Expect(err).NotTo(HaveOccurred())
		var canRead bool
		Expect(target.QueryRowContext(ctx, "SELECT has_table_privilege($1,'app.probe','SELECT')",
			string(reader.Spec.Name)).Scan(&canRead)).To(Succeed())
		Expect(canRead).To(BeTrue(), "creator defaults must affect a newly created application table")
		var version int
		Expect(f.Admin.QueryRowContext(ctx, "SHOW server_version_num").Scan(&version)).To(Succeed())
		if version >= 160000 {
			roleSQL(ctx, f.Fixture, "REVOKE "+quoteIdentifier(string(creator.Spec.Name))+" FROM "+quoteIdentifier(f.Role)+
				" GRANTED BY "+quoteIdentifier(f.Role))
		} else {
			roleSQL(ctx, f.Fixture, "REVOKE "+quoteIdentifier(string(creator.Spec.Name))+" FROM "+quoteIdentifier(f.Role))
		}
		Expect(managedOwnerIntegrationSet(ctx, f, string(creator.Spec.Name))).To(BeFalse())
		f.editDatabase(ctx, func(spec *epco.DatabaseSpec) { spec.Schemas[0].DefaultPrivileges = nil })
		managedOwnerIntegrationReady(ctx, f)
		Expect(managedOwnerIntegrationSet(ctx, f, string(creator.Spec.Name))).To(BeTrue(),
			"historical creator cleanup must repair authority after spec removal")
		var defaults int
		Expect(target.QueryRowContext(ctx, "SELECT count(*) FROM pg_default_acl "+
			"WHERE defaclrole=$1::regrole AND defaclnamespace='app'::regnamespace",
			string(creator.Spec.Name)).Scan(&defaults)).To(Succeed())
		Expect(defaults).To(BeZero())
	}, NodeTimeout(90*time.Second))

	It("retries committed SQL after a lost response using recorded managed ownership", func(ctx SpecContext) {
		f := managedOwnerIntegrationFixture(ctx, environment)
		owner := managedOwnerIntegrationRole(ctx, f, "owner")
		f.editDatabase(ctx, func(spec *epco.DatabaseSpec) { spec.Owner = owner.Spec.Name; spec.ConnectionSecrets = nil })
		lost := &databaseRecoveryFactory{delegate: pg.NewDatabaseFactory(), unknown: true}
		broken := databaseRecoveryReconciler(f.api, lost)
		Eventually(func() bool {
			_, _ = broken.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(f.database)})
			return recoveryDatabaseExists(ctx, f.Server, string(f.database.Spec.Name))
		}).WithContext(ctx).WithTimeout(10*time.Second).Should(BeTrue(),
			"SQL must commit before the intentionally lost response")
		Expect(managedOwnerIntegrationSet(ctx, f, string(owner.Spec.Name))).To(BeTrue())
		current := managedOwnerIntegrationReady(ctx, f)
		Expect(current.Status.HasManagedSQL).To(BeTrue())
		var actual string
		Expect(f.Admin.QueryRowContext(ctx, "SELECT pg_get_userbyid(datdba) FROM pg_database WHERE datname=$1",
			string(f.database.Spec.Name)).Scan(&actual)).To(Succeed())
		Expect(actual).To(Equal(string(owner.Spec.Name)))
	}, NodeTimeout(60*time.Second))
	DescribeTable("does not acquire service membership outside managed operation eligibility",
		func(ctx SpecContext, mode string) {
			f := managedOwnerIntegrationFixture(ctx, environment)
			owner := managedOwnerIntegrationRole(ctx, f, "owner")
			if mode == "external" {
				Expect(f.api.Delete(ctx, owner)).To(Succeed())
				_, err := recoveryReconciler(f.api, pg.NewRoleFactory()).Reconcile(ctx,
					ctrl.Request{NamespacedName: client.ObjectKeyFromObject(owner)})
				Expect(err).NotTo(HaveOccurred())
			}
			if mode == "role-observe" {
				owner = recoveryRole(ctx, f.api, owner)
				owner.Spec.ManagementPolicy = epco.ManagementObserve
				Expect(f.api.Update(ctx, owner)).To(Succeed())
				recoverUntilReady(ctx, f.api, recoveryReconciler(f.api, pg.NewRoleFactory()), owner)
			}
			f.editDatabase(ctx, func(spec *epco.DatabaseSpec) {
				spec.Owner = owner.Spec.Name
				spec.ConnectionSecrets = nil
				if mode == "protected" {
					spec.Owner = "postgres"
				}
				if mode == managedOwnerDatabaseObserve {
					spec.ManagementPolicy = epco.ManagementObserve
				}
			})
			if mode == managedOwnerDatabaseObserve {
				roleSQL(ctx, f.Fixture, "CREATE DATABASE "+quoteIdentifier(string(f.database.Spec.Name))+
					" OWNER "+quoteIdentifier(string(owner.Spec.Name)))
			}
			reconciler := databaseRecoveryReconciler(f.api, pg.NewDatabaseFactory())
			for range 4 {
				permissionRecoveryReconcile(ctx, reconciler, f.database)
			}
			Expect(managedOwnerIntegrationSet(ctx, f, string(owner.Spec.Name))).To(BeFalse())
			current := recoveredDatabase(ctx, f.api, f.database)
			if mode != managedOwnerDatabaseObserve {
				condition := meta.FindStatusCondition(current.Status.Conditions, "Ready")
				Expect(condition).NotTo(BeNil())
				Expect(condition.Status).To(Equal(metav1.ConditionFalse))
			}
		}, Entry("external SQL role", "external"), Entry("observed role manager", "role-observe"),
		Entry("observed database", managedOwnerDatabaseObserve), Entry("protected administrator", "protected"))

	It("does not acquire an owner membership only to restore CONNECT for extension work", func(ctx SpecContext) {
		f := managedOwnerIntegrationFixture(ctx, environment)
		owner := managedOwnerIntegrationRole(ctx, f, "owner")
		roleSQL(ctx, f.Fixture, "CREATE DATABASE "+quoteIdentifier(string(f.database.Spec.Name))+
			" OWNER "+quoteIdentifier(string(owner.Spec.Name)))
		target := f.target()
		_, err := target.ExecContext(ctx, "REVOKE CONNECT ON DATABASE "+
			quoteIdentifier(string(f.database.Spec.Name))+" FROM PUBLIC")
		Expect(err).NotTo(HaveOccurred())
		f.editDatabase(ctx, func(spec *epco.DatabaseSpec) {
			spec.Owner = owner.Spec.Name
			spec.ConnectionSecrets = nil
			spec.Extensions = []epco.ExtensionSpec{{Name: "hstore"}}
		})
		for range 4 {
			permissionRecoveryReconcile(ctx, databaseRecoveryReconciler(f.api, pg.NewDatabaseFactory()), f.database)
		}
		Expect(managedOwnerIntegrationSet(ctx, f, string(owner.Spec.Name))).To(BeFalse(),
			"extension-only CONNECT repair must preserve extension authority boundaries")
		var connected bool
		Expect(f.Admin.QueryRowContext(ctx, "SELECT has_database_privilege($1,$2,'CONNECT')",
			f.Role, string(f.database.Spec.Name)).Scan(&connected)).To(Succeed())
		Expect(connected).To(BeFalse())
		current := recoveredDatabase(ctx, f.api, f.database)
		condition := meta.FindStatusCondition(current.Status.Conditions, "Ready")
		Expect(condition).NotTo(BeNil())
		Expect(condition.Status).To(Equal(metav1.ConditionFalse))
	}, NodeTimeout(60*time.Second))

	It("repairs actual-owner deletion while both resources disappear and desired owner is unready", func(ctx SpecContext) {
		f := managedOwnerIntegrationFixture(ctx, environment)
		owner := managedOwnerIntegrationRole(ctx, f, "owner")
		f.editDatabase(ctx, func(spec *epco.DatabaseSpec) {
			spec.Owner = owner.Spec.Name
			spec.ConnectionSecrets = nil
			spec.ReclaimPolicy = epco.ReclaimDelete
		})
		current := managedOwnerIntegrationReady(ctx, f)
		var version int
		Expect(f.Admin.QueryRowContext(ctx, "SHOW server_version_num").Scan(&version)).To(Succeed())
		statement := "REVOKE " + quoteIdentifier(string(owner.Spec.Name)) + " FROM " + quoteIdentifier(f.Role)
		if version >= 160000 {
			statement += " GRANTED BY " + quoteIdentifier(f.Role)
		}
		roleSQL(ctx, f.Fixture, statement)
		Expect(managedOwnerIntegrationSet(ctx, f, string(owner.Spec.Name))).To(BeFalse())
		current.Spec.Owner = "unready_owner_must_not_gate_finalizer"
		Expect(f.api.Update(ctx, current)).To(Succeed())
		owner = recoveryRole(ctx, f.api, owner)
		owner.Spec.ReclaimPolicy = epco.ReclaimDelete
		Expect(f.api.Update(ctx, owner)).To(Succeed())
		Expect(f.api.Delete(ctx, owner)).To(Succeed())
		Expect(f.api.Delete(ctx, current)).To(Succeed())
		Eventually(func(g Gomega) {
			_, _ = recoveryReconciler(f.api, pg.NewRoleFactory()).Reconcile(ctx,
				ctrl.Request{NamespacedName: client.ObjectKeyFromObject(owner)})
			_, _ = databaseRecoveryReconciler(f.api, pg.NewDatabaseFactory()).Reconcile(ctx,
				ctrl.Request{NamespacedName: client.ObjectKeyFromObject(current)})
			g.Expect(apierrors.IsNotFound(f.api.Get(ctx, client.ObjectKeyFromObject(current), &epco.Database{}))).To(BeTrue())
			g.Expect(apierrors.IsNotFound(f.api.Get(ctx, client.ObjectKeyFromObject(owner), &epco.DatabaseRole{}))).To(BeTrue())
		}).WithContext(ctx).WithTimeout(15 * time.Second).Should(Succeed())
		Expect(recoveryDatabaseExists(ctx, f.Server, string(f.database.Spec.Name))).To(BeFalse())
		Expect(roleExists(ctx, f.Fixture, string(owner.Spec.Name))).To(BeFalse())
	}, NodeTimeout(60*time.Second))

	It("repairs an actual table owner before candidate planning and its historical grantor after omission",
		func(ctx SpecContext) {
			f := managedOwnerIntegrationFixture(ctx, environment)
			owner := managedOwnerIntegrationRole(ctx, f, "table-owner")
			reader := managedOwnerIntegrationRole(ctx, f, "reader")
			f.editDatabase(ctx, func(spec *epco.DatabaseSpec) {
				spec.Owner = epco.SQLName(f.Role)
				spec.ConnectionSecrets = nil
				spec.Schemas = []epco.SchemaSpec{{Name: "app", Permissions: &epco.SchemaPermissions{
					Usage: []epco.Permission{{Name: owner.Spec.Name}, {Name: reader.Spec.Name}}}}}
			})
			managedOwnerIntegrationReady(ctx, f)
			target := f.target()
			_, err := target.ExecContext(ctx, "CREATE TABLE app.probe(value int); ALTER TABLE app.probe OWNER TO "+
				quoteIdentifier(string(owner.Spec.Name)))
			Expect(err).NotTo(HaveOccurred())
			f.editDatabase(ctx, func(spec *epco.DatabaseSpec) {
				spec.Schemas[0].TablePermissions = []epco.NamedTablePermissions{{Name: "probe",
					Permissions: epco.TablePermissions{Select: []epco.Permission{{Name: reader.Spec.Name}}}}}
			})
			managedOwnerIntegrationReady(ctx, f)
			Expect(managedOwnerIntegrationSet(ctx, f, string(owner.Spec.Name))).To(BeTrue())
			Expect(managedOwnerIntegrationSet(ctx, f, string(reader.Spec.Name))).To(BeFalse())
			var granted bool
			Expect(target.QueryRowContext(ctx, "SELECT has_table_privilege($1,'app.probe','SELECT')",
				string(reader.Spec.Name)).Scan(&granted)).To(Succeed())
			Expect(granted).To(BeTrue())
			var version int
			Expect(f.Admin.QueryRowContext(ctx, "SHOW server_version_num").Scan(&version)).To(Succeed())
			statement := "REVOKE " + quoteIdentifier(string(owner.Spec.Name)) + " FROM " + quoteIdentifier(f.Role)
			if version >= 160000 {
				statement += " GRANTED BY " + quoteIdentifier(f.Role)
			}
			roleSQL(ctx, f.Fixture, statement)
			f.editDatabase(ctx, func(spec *epco.DatabaseSpec) { spec.Schemas[0].TablePermissions = nil })
			managedOwnerIntegrationReady(ctx, f)
			Expect(managedOwnerIntegrationSet(ctx, f, string(owner.Spec.Name))).To(BeTrue())
			Expect(target.QueryRowContext(ctx, "SELECT has_table_privilege($1,'app.probe','SELECT')",
				string(reader.Spec.Name)).Scan(&granted)).To(Succeed())
			Expect(granted).To(BeFalse(), "historical exact-grantor cleanup must remove the previously managed row")
		}, NodeTimeout(60*time.Second))

})
