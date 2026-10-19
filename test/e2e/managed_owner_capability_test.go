//go:build e2e

package e2e

import (
	"context"
	"crypto/rand"
	"os"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	epco "github.com/vterdunov/external-postgres-control-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type managedOwnerFixture struct {
	cluster                       *epco.ExternalPostgres
	prefix, admin, owner, grantor string
	role                          *epco.DatabaseRole
}

func newManagedOwnerFixture(ctx context.Context) *managedOwnerFixture {
	prefix := "mo_" + strings.ToLower(rand.Text()[:8])
	f := &managedOwnerFixture{prefix: prefix, admin: prefix + "_admin",
		owner: prefix + "_owner", grantor: prefix + "_grantor"}
	installedDatabaseSQL(ctx, "postgres", "CREATE ROLE "+f.admin+
		" LOGIN NOSUPERUSER CREATEDB CREATEROLE NOINHERIT PASSWORD '"+installedDatabasePassword+"'")
	if os.Getenv("EPCO_PG_VERSION") != "15" {
		installedDatabaseSQL(ctx, "postgres", "ALTER ROLE "+f.admin+" SET createrole_self_grant=''")
	}
	DeferCleanup(func() {
		cleanup, cancel := boundedContext()
		defer cancel()
		installedDatabaseSQL(cleanup, "postgres", "DROP ROLE IF EXISTS "+f.owner+", "+f.grantor+", "+f.admin)
	})
	f.cluster = installedRoleDescriptor(ctx, strings.ReplaceAll(prefix, "_", "-"), nil)
	before := f.cluster.DeepCopy()
	f.cluster.Spec.ReconcileInterval = "2s"
	ExpectWithOffset(1, apiClient.Patch(ctx, f.cluster, client.MergeFrom(before))).To(Succeed())
	f.useAdmin(ctx, f.admin, installedDatabasePassword)
	return f
}

func (f *managedOwnerFixture) useAdmin(ctx context.Context, username, password string) {
	secret := &corev1.Secret{}
	ExpectWithOffset(1, apiClient.Get(ctx, client.ObjectKey{Namespace: f.cluster.Namespace,
		Name: f.cluster.Spec.CredentialsSecret.Name}, secret)).To(Succeed())
	secret.Data[corev1.BasicAuthUsernameKey], secret.Data[corev1.BasicAuthPasswordKey] = []byte(username), []byte(password)
	ExpectWithOffset(1, apiClient.Update(ctx, secret)).To(Succeed())
	installedClusterStatus(ctx, client.ObjectKeyFromObject(f.cluster), metav1.ConditionTrue, "Ready")
}

func (f *managedOwnerFixture) createRole(ctx context.Context, login bool) *epco.DatabaseRole {
	role := &epco.DatabaseRole{Namespace: f.cluster.Namespace, Name: f.cluster.Name + "-owner",
		Spec: epco.DatabaseRoleSpec{Name: epco.SQLName(f.owner), Cluster: epco.ClusterReference{Name: f.cluster.Name},
			Login: login}}
	if login {
		role.Spec.PasswordGeneration = &epco.PasswordGeneration{SecretName: role.Name + "-source"}
		DeferCleanup(func() {
			deleteObject(&corev1.Secret{Namespace: role.Namespace, Name: role.Spec.PasswordGeneration.SecretName})
		})
	}
	ExpectWithOffset(1, apiClient.Create(ctx, role)).To(Succeed())
	DeferCleanup(func() { deleteObject(role) })
	f.role = installedRole(ctx, role, metav1.ConditionTrue, "Ready")
	ExpectWithOffset(1, f.role.Status.HasManagedSQL).To(BeTrue())
	return f.role
}

func (f *managedOwnerFixture) createDatabase(ctx context.Context, suffix string) *epco.Database {
	name := f.prefix + "_" + suffix
	installedDatabaseFixtureCleanup(name)
	database := &epco.Database{Namespace: f.cluster.Namespace, Name: f.cluster.Name + "-" + suffix,
		Spec: epco.DatabaseSpec{Name: epco.SQLName(name), Owner: epco.SQLName(f.owner),
			Cluster: epco.ClusterReference{Name: f.cluster.Name}, ReclaimPolicy: epco.ReclaimDelete,
			Schemas: []epco.SchemaSpec{{Name: installedDatabaseApplicationSchema, Owner: epco.SQLName(f.owner)}}}}
	ExpectWithOffset(1, apiClient.Create(ctx, database)).To(Succeed())
	DeferCleanup(func() { deleteObject(database) })
	return database
}

func (f *managedOwnerFixture) expectOwner(ctx context.Context, database *epco.Database) {
	installedDatabase(ctx, database, metav1.ConditionTrue, "Ready")
	ExpectWithOffset(1, installedDatabaseSQL(ctx, "postgres", "SELECT pg_get_userbyid(datdba) FROM pg_database "+
		"WHERE datname='"+
		string(database.Spec.Name)+"'")).To(Equal(f.owner))
	ExpectWithOffset(1, installedDatabaseSQL(ctx, string(database.Spec.Name),
		"SELECT pg_get_userbyid(nspowner) FROM pg_namespace WHERE nspname='app'")).To(Equal(f.owner))
	ExpectWithOffset(1, installedDatabaseSQL(ctx, "postgres",
		"SELECT rolsuper,rolcreatedb,rolcreaterole,rolreplication,rolbypassrls "+
			"FROM pg_roles WHERE rolname='"+
			f.owner+"'")).To(Equal("f|f|f|f|f"))
}

func (f *managedOwnerFixture) setCapability(ctx context.Context) string {
	capability := "SET"
	if os.Getenv("EPCO_PG_VERSION") == "15" {
		capability = "MEMBER"
	}
	return installedDatabaseSQL(ctx, "postgres", "SELECT pg_has_role('"+f.admin+"','"+f.owner+"','"+capability+"')")
}

func (f *managedOwnerFixture) memberships(ctx context.Context) string {
	return installedDatabaseSQL(ctx, "postgres", "SELECT pg_get_userbyid(grantor),admin_option,inherit_option,set_option "+
		"FROM pg_auth_members WHERE roleid='"+
		f.owner+"'::regrole AND member='"+f.admin+"'::regrole ORDER BY grantor")
}

var _ = Describe("Installed managed owner capability", func() {
	It("bootstraps a fresh restricted administrator without selfgrant or fixture membership "+
		"and supports a native application", func(ctx SpecContext) {
		f := newManagedOwnerFixture(ctx)
		f.createRole(ctx, true)
		Expect(installedDatabaseSQL(ctx, "postgres", "SELECT rolsuper,rolcreatedb,rolcreaterole FROM pg_roles "+
			"WHERE rolname='"+f.admin+"'")).To(Equal("f|t|t"))
		if os.Getenv("EPCO_PG_VERSION") != "15" {
			Expect(f.setCapability(ctx)).To(Equal("f"), "fresh role creator ADMIN must not silently provide SET")
			Expect(f.memberships(ctx)).To(Equal("postgres|t|f|f"))
		}
		database := f.createDatabase(ctx, "fresh")
		database.Spec.ConnectionSecrets = []epco.ConnectionSecret{{Name: f.cluster.Name + "-connection",
			RoleRef: epco.LocalReference{Name: f.role.Name}}}
		Expect(apiClient.Update(ctx, database)).To(Succeed())
		f.expectOwner(ctx, database)
		Expect(f.setCapability(ctx)).To(Equal("t"))
		output := installedConnectionOutput(ctx, database.Namespace, database.Spec.ConnectionSecrets[0].Name)
		application := &r6Application{port: r6ForwardPostgres(ctx)}
		Expect(application.query(ctx, output,
			"CREATE TABLE app.probe(value int); INSERT INTO app.probe VALUES(42)", "")).To(BeEmpty())
		Expect(application.query(ctx, output, "SELECT current_user,value FROM app.probe", "")).To(Equal(f.owner + "|42"))
		Expect(application.query(ctx, output, "CREATE ROLE forbidden_escalation", "42501")).To(BeEmpty())
		if os.Getenv("EPCO_PG_VERSION") != "15" {
			Expect(f.memberships(ctx)).To(Equal("postgres|t|f|f\n" + f.admin + "|f|f|t"))
		}
		second := f.createDatabase(ctx, "shared")
		f.expectOwner(ctx, second)
		deleteObject(second)
		Expect(f.setCapability(ctx)).To(Equal("t"), "one database must not revoke a shared service membership")
		// Drift repair uses current SQL and retained role management, without a new CR.
		if os.Getenv("EPCO_PG_VERSION") != "15" {
			installedDatabaseSQL(ctx, "postgres", "GRANT "+f.owner+" TO "+f.admin+" WITH SET FALSE GRANTED BY "+f.admin)
			third := f.createDatabase(ctx, "repair")
			f.expectOwner(ctx, third)
			Expect(f.memberships(ctx)).To(Equal("postgres|t|f|f\n" + f.admin + "|f|f|t"))
		}
		// Transfer keeps the accepted old-owner INHERIT prerequisite. Only the new owner's SET is automatic.
		other := &epco.DatabaseRole{Namespace: f.cluster.Namespace, Name: f.cluster.Name + "-other",
			Spec: epco.DatabaseRoleSpec{Name: epco.SQLName(f.grantor), Cluster: epco.ClusterReference{Name: f.cluster.Name}}}
		Expect(apiClient.Create(ctx, other)).To(Succeed())
		DeferCleanup(func() { deleteObject(other) })
		installedRole(ctx, other, metav1.ConditionTrue, "Ready")
		if os.Getenv("EPCO_PG_VERSION") != "15" {
			installedDatabaseSQL(ctx, "postgres", "GRANT "+f.owner+" TO "+f.admin+" WITH INHERIT TRUE GRANTED BY "+f.admin)
		} else {
			installedDatabaseSQL(ctx, "postgres", "ALTER ROLE "+f.admin+" INHERIT")
		}
		Expect(apiClient.Get(ctx, client.ObjectKeyFromObject(database), database)).To(Succeed())
		database.Spec.Owner = epco.SQLName(f.grantor)
		Expect(apiClient.Update(ctx, database)).To(Succeed())
		installedDatabase(ctx, database, metav1.ConditionTrue, "Ready")
		Expect(installedDatabaseSQL(ctx, "postgres", "SELECT pg_get_userbyid(datdba) FROM pg_database "+
			"WHERE datname='"+string(database.Spec.Name)+"'")).To(Equal(f.grantor))
		Expect(f.setCapability(ctx)).To(Equal("t"), "owner transfer must retain old shared service access")
	}, NodeTimeout(5*time.Minute))

	DescribeTable("repairs PostgreSQL 16 grantor-specific authority without changing independent options",
		func(ctx SpecContext, authority string) {
			if os.Getenv("EPCO_PG_VERSION") == "15" {
				Skip("independent membership options require PostgreSQL 16")
			}
			f := newManagedOwnerFixture(ctx)
			f.createRole(ctx, false)
			// Preserve the creator ADMIN binding and change only the needed SET option.
			if authority == "direct" {
				installedDatabaseSQL(ctx, "postgres", "GRANT "+f.owner+" TO "+f.admin+
					" WITH ADMIN FALSE, INHERIT TRUE, SET FALSE GRANTED BY "+f.admin)
			} else {
				installedDatabaseSQL(ctx, "postgres", "REVOKE "+f.owner+" FROM "+f.admin+"; CREATE ROLE "+f.grantor+" NOLOGIN; "+
					"GRANT "+f.owner+" TO "+f.grantor+" WITH ADMIN TRUE, INHERIT FALSE, SET FALSE; "+
					"GRANT "+f.owner+" TO "+f.admin+" WITH ADMIN FALSE, INHERIT TRUE, SET FALSE")
				options := "INHERIT TRUE, SET FALSE"
				if authority == "set-reachable" {
					options = "INHERIT FALSE, SET TRUE"
				}
				installedDatabaseSQL(ctx, "postgres", "GRANT "+f.grantor+" TO "+f.admin+" WITH "+options)
			}
			before := f.memberships(ctx)
			database := f.createDatabase(ctx, "authority")
			f.expectOwner(ctx, database)
			Expect(f.setCapability(ctx)).To(Equal("t"))
			if authority == "direct" {
				Expect(f.memberships(ctx)).To(Equal("postgres|t|f|f\n" + f.admin + "|f|t|t"))
			} else {
				Expect(f.memberships(ctx)).To(ContainSubstring(before), "independent creator row must be unchanged")
				Expect(f.memberships(ctx)).To(ContainSubstring(f.grantor + "|f|f|t"))
			}
		}, Entry("direct ADMIN", "direct"), Entry("inherited ADMIN", "inherited"),
		Entry("SET-reachable ADMIN", "set-reachable"))

	It("uses existing independent SET without ADMIN and recovers after all repair authority is restored",
		func(ctx SpecContext) {
			if os.Getenv("EPCO_PG_VERSION") == "15" {
				Skip("independent membership options require PostgreSQL 16")
			}
			f := newManagedOwnerFixture(ctx)
			f.createRole(ctx, false)
			installedDatabaseSQL(ctx, "postgres", "REVOKE "+f.owner+" FROM "+f.admin+"; GRANT "+f.owner+" TO "+f.admin+
				" WITH ADMIN FALSE, INHERIT FALSE, SET TRUE")
			before := f.memberships(ctx)
			ready := f.createDatabase(ctx, "sufficient")
			f.expectOwner(ctx, ready)
			Expect(f.memberships(ctx)).To(Equal(before), "sufficient SET must avoid a redundant, unauthorized GRANT")
			installedDatabaseSQL(ctx, "postgres", "GRANT "+f.owner+" TO "+f.admin+" WITH SET FALSE")
			pending := f.createDatabase(ctx, "noauthority")
			installedDatabase(ctx, pending, metav1.ConditionFalse, "InsufficientPrivileges")
			Expect(f.setCapability(ctx)).To(Equal("f"))
			Expect(installedDatabaseSQL(ctx, "postgres", "SELECT count(*) FROM pg_database "+
				"WHERE datname='"+string(pending.Spec.Name)+"'")).To(Equal("0"))
			installedDatabaseSQL(ctx, "postgres", "GRANT "+f.owner+" TO "+f.admin+" WITH ADMIN TRUE, INHERIT FALSE, SET FALSE")
			f.expectOwner(ctx, pending)
			Expect(f.setCapability(ctx)).To(Equal("t"))
		}, NodeTimeout(4*time.Minute))
	It("adopts existing managed SQL and retains service access across Role CR recreation", func(ctx SpecContext) {
		f := newManagedOwnerFixture(ctx)
		installedDatabaseSQL(ctx, "postgres", "SET ROLE "+f.admin+"; CREATE ROLE "+f.owner+" NOLOGIN; RESET ROLE")
		owner := f.createRole(ctx, false)
		database := f.createDatabase(ctx, "existing")
		f.expectOwner(ctx, database)
		beforeUID := owner.UID
		deleteObject(owner)
		owner = f.createRole(ctx, false)
		Expect(owner.UID).NotTo(Equal(beforeUID))
		Expect(f.setCapability(ctx)).To(Equal("t"), "retained SQL memberships survive CR recreation")
		f.expectOwner(ctx, f.createDatabase(ctx, "recreated"))
	}, NodeTimeout(3*time.Minute))

})
