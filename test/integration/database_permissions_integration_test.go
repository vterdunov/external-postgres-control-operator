//go:build integration

package integration

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	epco "github.com/vterdunov/external-postgres-control-operator/api/v1alpha1"
	"github.com/vterdunov/external-postgres-control-operator/internal/controller"
	pg "github.com/vterdunov/external-postgres-control-operator/internal/postgres"
	kubeenv "github.com/vterdunov/external-postgres-control-operator/test/envtest"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	r3Schema       = databaseRecoveryApplicationName
	r3AllowedTable = "r3_allowed"
	r3MissingTable = "r3_missing"
	r3Connect      = "CONNECT"
	r3Usage        = "USAGE"
	r3Create       = "CREATE"
)

type r3SQLFixture struct {
	*Fixture
	scope         *kubeenv.Scope
	database      *epco.Database
	target        *sql.DB
	reader, other string
	roles         []string
	version       int
}

func (f *r3SQLFixture) role(ctx context.Context) string {
	name, err := randomName("r3_role_")
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	f.roles = append(f.roles, name)
	roleSQL(ctx, f.Fixture, "CREATE ROLE "+quoteIdentifier(name)+
		" LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE PASSWORD "+quoteLiteral(f.Password))
	return name
}

func (f *r3SQLFixture) exec(ctx context.Context, query string) {
	_, err := f.target.ExecContext(ctx, query)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
}

func (f *r3SQLFixture) login(ctx context.Context, name string) *sql.DB {
	db, err := f.Server.Open(string(f.database.Spec.Name), name, f.Password)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	ExpectWithOffset(1, db.PingContext(ctx)).To(Succeed())
	DeferCleanup(func() { Expect(db.Close()).To(Succeed()) })
	return db
}

func (f *r3SQLFixture) update(ctx context.Context, edit func(*epco.DatabaseSpec)) {
	current := recoveredDatabase(ctx, f.scope.Client, f.database)
	edit(&current.Spec)
	ExpectWithOffset(1, f.scope.Client.Update(ctx, current)).To(Succeed())
}

func (f *r3SQLFixture) reconcile(ctx context.Context, reconciler *controller.DatabaseReconciler) *epco.Database {
	_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(f.database)})
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	return recoveredDatabase(ctx, f.scope.Client, f.database)
}

func (f *r3SQLFixture) ready(ctx context.Context) {
	recoverDatabaseReady(ctx, f.scope.Client,
		databaseRecoveryReconciler(f.scope.Client, pg.NewDatabaseFactory()), f.database)
}

func r3Permissions(name string, ensure epco.EnsureOption) []epco.Permission {
	return []epco.Permission{{Name: epco.SQLName(name), Ensure: ensure}}
}

// Catalog assertions distinguish direct grants from inherited/effective rights.
// Empty ACLs remain empty, while NULL ACLs expand PostgreSQL's built-in defaults.
func (f *r3SQLFixture) direct(ctx context.Context, kind, object, grantee, privilege string) bool {
	var query string
	switch kind {
	case "database":
		query = `SELECT EXISTS(SELECT 1 FROM pg_database o CROSS JOIN LATERAL
		 aclexplode(COALESCE(o.datacl,acldefault('d',o.datdba))) a
		 WHERE o.datname=$1 AND a.grantee=CASE WHEN $2='public' THEN 0
		 ELSE (SELECT oid FROM pg_roles WHERE rolname=$2) END AND a.privilege_type=$3)`
	case "schema":
		query = `SELECT EXISTS(SELECT 1 FROM pg_namespace o CROSS JOIN LATERAL
		 aclexplode(COALESCE(o.nspacl,acldefault('n',o.nspowner))) a
		 WHERE o.nspname=$1 AND a.grantee=CASE WHEN $2='public' THEN 0
		 ELSE (SELECT oid FROM pg_roles WHERE rolname=$2) END AND a.privilege_type=$3)`
	default:
		query = `SELECT EXISTS(SELECT 1 FROM pg_class o CROSS JOIN LATERAL
		 aclexplode(COALESCE(o.relacl,acldefault(CASE WHEN o.relkind='S' THEN 's'::"char" ELSE 'r'::"char" END,o.relowner))) a
		 WHERE o.oid=to_regclass($1) AND a.grantee=CASE WHEN $2='public' THEN 0
		 ELSE (SELECT oid FROM pg_roles WHERE rolname=$2) END AND a.privilege_type=$3)`
	}
	var present bool
	ExpectWithOffset(1, f.target.QueryRowContext(ctx, query, object, grantee, privilege).Scan(&present)).To(Succeed())
	return present
}

func (f *r3SQLFixture) table(ctx context.Context, name string) {
	f.exec(ctx, "SET ROLE "+quoteIdentifier(string(f.database.Spec.Owner))+
		"; CREATE TABLE app."+quoteIdentifier(name)+"(value int); INSERT INTO app."+
		quoteIdentifier(name)+" VALUES(42); RESET ROLE")
}

func (f *r3SQLFixture) defaultGrant(ctx context.Context, creator, schema, kind, grantee, privilege string) bool {
	var present bool
	ExpectWithOffset(1, f.target.QueryRowContext(ctx, `SELECT EXISTS(
	 SELECT 1 FROM pg_default_acl d CROSS JOIN LATERAL aclexplode(d.defaclacl) a
	 WHERE d.defaclrole=(SELECT oid FROM pg_roles WHERE rolname=$1)
	 AND d.defaclnamespace=CASE WHEN $2='' THEN 0 ELSE (SELECT oid FROM pg_namespace WHERE nspname=$2) END
	 AND d.defaclobjtype=$3::"char"
	 AND a.grantee=CASE WHEN $4='public' THEN 0 ELSE (SELECT oid FROM pg_roles WHERE rolname=$4) END
	 AND a.privilege_type=$5)`, creator, schema, kind, grantee, privilege).Scan(&present)).To(Succeed())
	return present
}

func (f *r3SQLFixture) pending(ctx context.Context, reason string) *epco.Database {
	var current *epco.Database
	Eventually(func(g Gomega) {
		current = f.reconcile(ctx, databaseRecoveryReconciler(f.scope.Client, pg.NewDatabaseFactory()))
		condition := meta.FindStatusCondition(current.Status.Conditions, "Ready")
		g.Expect(condition).NotTo(BeNil())
		g.Expect(condition.Status).To(Equal(metav1.ConditionFalse))
		g.Expect(condition.Reason).To(Equal(reason))
		g.Expect(condition.ObservedGeneration).To(Equal(current.Generation))
	}).WithContext(ctx).WithTimeout(10 * time.Second).Should(Succeed())
	return current
}

type r3StatusClient struct {
	client.Client
	reject func(*epco.Database) bool
	failed atomic.Int32
}

type r3StatusWriter struct {
	client.SubResourceWriter
	owner *r3StatusClient
}

func (c *r3StatusClient) Status() client.SubResourceWriter {
	return &r3StatusWriter{SubResourceWriter: c.Client.Status(), owner: c}
}

func (w *r3StatusWriter) reject(object client.Object) error {
	database, ok := object.(*epco.Database)
	if ok && w.owner.reject(database) {
		w.owner.failed.Add(1)
		return errors.New("injected R3 ACL status persistence failure")
	}
	return nil
}

func (w *r3StatusWriter) Update(ctx context.Context, object client.Object,
	options ...client.SubResourceUpdateOption) error {
	if err := w.reject(object); err != nil {
		return err
	}
	return w.SubResourceWriter.Update(ctx, object, options...)
}

func (w *r3StatusWriter) Patch(ctx context.Context, object client.Object, patch client.Patch,
	options ...client.SubResourcePatchOption) error {
	if err := w.reject(object); err != nil {
		return err
	}
	return w.SubResourceWriter.Patch(ctx, object, patch, options...)
}

var _ = Describe("R3 declarative permissions with real API and PostgreSQL", Ordered, ContinueOnFailure, func() {
	var environment *kubeenv.Environment
	var fixture *r3SQLFixture
	BeforeAll(func(ctx SpecContext) {
		_, source, _, ok := runtime.Caller(0)
		Expect(ok).To(BeTrue())
		var err error
		environment, err = kubeenv.Start(ctx, kubeenv.Options{
			CRDDirectory:          filepath.Join(filepath.Dir(source), "..", "..", "config", "crd", "bases"),
			BinaryAssetsDirectory: os.Getenv("KUBEBUILDER_ASSETS"), Timeout: 30 * time.Second})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			Expect(environment.Stop(ctx)).To(Succeed())
		})
	}, NodeTimeout(60*time.Second))
	BeforeEach(func(ctx SpecContext) {
		base, err := postgres.NewFixture(ctx)
		Expect(err).NotTo(HaveOccurred())
		scope, _, err := environment.NewScope(ctx, "r3-acl-")
		Expect(err).NotTo(HaveOccurred())
		fixture = &r3SQLFixture{Fixture: base, scope: scope}
		DeferCleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
			defer cancel()
			Expect(scope.Cleanup(ctx)).To(Succeed())
			if fixture.target != nil {
				Expect(fixture.target.Close()).To(Succeed())
			}
			if fixture.database != nil {
				_, err := fixture.Server.Admin.ExecContext(ctx,
					"DROP DATABASE IF EXISTS "+quoteIdentifier(string(fixture.database.Spec.Name))+" WITH (FORCE)")
				Expect(err).NotTo(HaveOccurred())
			}
			for _, role := range fixture.roles {
				roleSQL(ctx, base, "DROP ROLE IF EXISTS "+quoteIdentifier(role))
			}
			Expect(base.Close(ctx)).To(Succeed())
		})
		Expect(base.Admin.QueryRowContext(ctx, "SHOW server_version_num").Scan(&fixture.version)).To(Succeed())
		roleSQL(ctx, base, "ALTER ROLE "+quoteIdentifier(base.Role)+" CREATEDB")
		fixture.reader, fixture.other = fixture.role(ctx), fixture.role(ctx)
		cluster := recoveryCluster(ctx, scope, base)
		secret := &corev1.Secret{}
		secretKey := client.ObjectKey{Namespace: scope.Namespace.Name, Name: recoveryAdminSecret}
		Expect(environment.Client.Get(ctx, secretKey, secret)).To(Succeed())
		secret.Data[corev1.BasicAuthUsernameKey] = []byte(base.Role)
		secret.Data[corev1.BasicAuthPasswordKey] = []byte(base.Password)
		Expect(environment.Client.Update(ctx, secret)).To(Succeed())
		name, err := randomName("r3_db_")
		Expect(err).NotTo(HaveOccurred())
		fixture.database = &epco.Database{Name: "permissions", Namespace: scope.Namespace.Name,
			Spec: epco.DatabaseSpec{Name: epco.SQLName(name), Owner: epco.SQLName(base.Role),
				Cluster: epco.ClusterReference{Name: cluster.Name}, ReclaimPolicy: epco.ReclaimRetain,
				Schemas: []epco.SchemaSpec{{Name: r3Schema}}}}
		roleSQL(ctx, base, "CREATE DATABASE "+quoteIdentifier(name)+" OWNER "+quoteIdentifier(base.Role))
		fixture.target, err = base.Server.Open(name, roleSQLAdmin, base.Server.password)
		Expect(err).NotTo(HaveOccurred())
		fixture.exec(ctx, "CREATE SCHEMA app AUTHORIZATION "+quoteIdentifier(base.Role))
		Expect(scope.Create(ctx, fixture.database)).To(Succeed())
	}, NodeTimeout(30*time.Second))

	It("grants exactly declared database and schema privileges and repairs drift", func(ctx SpecContext) {
		fixture.update(ctx, func(spec *epco.DatabaseSpec) {
			p := r3Permissions(fixture.reader, epco.EnsurePresent)
			spec.Permissions = &epco.DatabasePermissions{Connect: p, Create: p, Temporary: p}
			spec.Schemas[0].Permissions = &epco.SchemaPermissions{Usage: p, Create: p}
		})
		current := fixture.reconcile(ctx, databaseRecoveryReconciler(environment.Client, pg.NewDatabaseFactory()))
		condition := meta.FindStatusCondition(current.Status.Conditions, "Ready")
		Expect(condition).NotTo(BeNil())
		Expect(condition.Reason).NotTo(Equal("UnsupportedFeature"),
			"R3 requires database/schema ACL support on every supported PostgreSQL major")
		fixture.ready(ctx)
		for _, privilege := range []string{r3Connect, r3Create, "TEMPORARY"} {
			Expect(fixture.direct(ctx, "database", string(fixture.database.Spec.Name), fixture.reader, privilege)).To(BeTrue())
		}
		for _, privilege := range []string{r3Usage, r3Create} {
			Expect(fixture.direct(ctx, "schema", r3Schema, fixture.reader, privilege)).To(BeTrue())
		}
		Expect(fixture.direct(ctx, "schema", r3Schema, fixture.other, r3Usage)).To(BeFalse())
		fixture.exec(ctx, "REVOKE USAGE ON SCHEMA app FROM "+quoteIdentifier(fixture.reader))
		fixture.ready(ctx)
		Expect(fixture.direct(ctx, "schema", r3Schema, fixture.reader, r3Usage)).To(BeTrue())
		reader := fixture.login(ctx, fixture.reader)
		_, err := reader.ExecContext(ctx, "CREATE TEMP TABLE session_value(value int); "+
			"CREATE SCHEMA reader_owned; CREATE TABLE app.reader_owned(value int)")
		Expect(err).NotTo(HaveOccurred())
	}, NodeTimeout(30*time.Second))

	DescribeTable("adopts existing grants then cleans up only live-UID managed declarations",
		func(ctx SpecContext, empty bool) {
			fixture.exec(ctx, "GRANT USAGE, CREATE ON SCHEMA app TO "+quoteIdentifier(fixture.reader)+
				"; GRANT USAGE ON SCHEMA app TO "+quoteIdentifier(fixture.other))
			fixture.update(ctx, func(spec *epco.DatabaseSpec) {
				spec.Schemas[0].Permissions = &epco.SchemaPermissions{Usage: r3Permissions(fixture.reader, epco.EnsurePresent)}
			})
			fixture.ready(ctx)
			fixture.update(ctx, func(spec *epco.DatabaseSpec) {
				if empty {
					spec.Schemas[0].Permissions = &epco.SchemaPermissions{Usage: []epco.Permission{}}
				} else {
					spec.Schemas[0].Permissions = nil
				}
			})
			fixture.ready(ctx)
			Expect(fixture.direct(ctx, "schema", r3Schema, fixture.reader, r3Usage)).To(BeFalse())
			Expect(fixture.direct(ctx, "schema", r3Schema, fixture.reader, r3Create)).To(BeTrue(),
				"initially omitted privileges remain unmanaged")
			Expect(fixture.direct(ctx, "schema", r3Schema, fixture.other, r3Usage)).To(BeTrue())
		}, Entry("empty list", true), Entry("omitted block", false))

	It("explicit absent removes existing access while withdrawal stops enforcement", func(ctx SpecContext) {
		fixture.exec(ctx, "GRANT USAGE ON SCHEMA app TO "+quoteIdentifier(fixture.reader))
		fixture.update(ctx, func(spec *epco.DatabaseSpec) {
			spec.Schemas[0].Permissions = &epco.SchemaPermissions{Usage: r3Permissions(fixture.reader, epco.EnsureAbsent)}
		})
		fixture.ready(ctx)
		Expect(fixture.direct(ctx, "schema", r3Schema, fixture.reader, r3Usage)).To(BeFalse())
		fixture.update(ctx, func(spec *epco.DatabaseSpec) { spec.Schemas[0].Permissions = nil })
		fixture.ready(ctx)
		Expect(fixture.direct(ctx, "schema", r3Schema, fixture.reader, r3Usage)).To(BeFalse(),
			"omission does not restore removed rights")
		fixture.exec(ctx, "GRANT USAGE ON SCHEMA app TO "+quoteIdentifier(fixture.reader))
		fixture.ready(ctx)
		Expect(fixture.direct(ctx, "schema", r3Schema, fixture.reader, r3Usage)).To(BeTrue(),
			"withdrawn absent declaration stops enforcement")
	})

	It("normalizes NULL PUBLIC ACL and preserves fresh restricted administrator CONNECT", func(ctx SpecContext) {
		owner := fixture.role(ctx)
		roleSQL(ctx, fixture.Fixture, "ALTER ROLE "+quoteIdentifier(fixture.Role)+
			" NOINHERIT; GRANT "+quoteIdentifier(owner)+" TO "+quoteIdentifier(fixture.Role))
		if fixture.version >= 160000 {
			roleSQL(ctx, fixture.Fixture, "GRANT "+quoteIdentifier(owner)+" TO "+quoteIdentifier(fixture.Role)+
				" WITH INHERIT FALSE, SET TRUE")
		}
		fixture.exec(ctx, "ALTER DATABASE "+quoteIdentifier(string(fixture.database.Spec.Name))+
			" OWNER TO "+quoteIdentifier(owner)+"; ALTER SCHEMA app OWNER TO "+quoteIdentifier(owner))
		var nullACL bool
		Expect(fixture.target.QueryRowContext(ctx, "SELECT datacl IS NULL FROM pg_database WHERE datname=$1",
			string(fixture.database.Spec.Name)).Scan(&nullACL)).To(Succeed())
		Expect(nullACL).To(BeTrue())
		fixture.update(ctx, func(spec *epco.DatabaseSpec) {
			spec.Owner = epco.SQLName(owner)
			spec.Permissions = &epco.DatabasePermissions{Connect: r3Permissions("public", epco.EnsureAbsent),
				Temporary: r3Permissions("public", epco.EnsureAbsent)}
			spec.Schemas[0].Permissions = &epco.SchemaPermissions{Usage: r3Permissions(fixture.reader, epco.EnsurePresent)}
		})
		fixture.ready(ctx)
		Expect(fixture.direct(ctx, "database", string(fixture.database.Spec.Name), "public", r3Connect)).To(BeFalse())
		Expect(fixture.direct(ctx, "database", string(fixture.database.Spec.Name), "public", "TEMPORARY")).To(BeFalse())
		admin := fixture.login(ctx, fixture.Role)
		var actual string
		Expect(admin.QueryRowContext(ctx, "SELECT current_user").Scan(&actual)).To(Succeed())
		Expect(actual).To(Equal(fixture.Role))
		fixture.ready(ctx)
		Expect(fixture.direct(ctx, "schema", r3Schema, fixture.reader, r3Usage)).To(BeTrue())
	}, NodeTimeout(30*time.Second))

	It("confirms database ACLs during disabled admission while retaining target cleanup", func(ctx SpecContext) {
		fixture.update(ctx, func(spec *epco.DatabaseSpec) {
			spec.Schemas[0].Permissions = &epco.SchemaPermissions{
				Usage: r3Permissions(fixture.reader, epco.EnsurePresent)}
		})
		fixture.ready(ctx)
		Expect(fixture.direct(ctx, "schema", r3Schema, fixture.reader, r3Usage)).To(BeTrue())
		fixture.update(ctx, func(spec *epco.DatabaseSpec) {
			spec.AllowConnections = new(false)
			spec.Schemas = nil
			p := r3Permissions(fixture.other, epco.EnsurePresent)
			spec.Permissions = &epco.DatabasePermissions{Create: p, Connect: p}
		})
		current := fixture.pending(ctx, "DependenciesNotReady")
		var admission bool
		Expect(fixture.Server.Admin.QueryRowContext(ctx, "SELECT datallowconn FROM pg_database WHERE datname=$1",
			string(fixture.database.Spec.Name)).Scan(&admission)).To(Succeed())
		Expect(admission).To(BeFalse(), "target admission remains deliberately disabled")
		for _, privilege := range []string{r3Create, r3Connect} {
			Expect(fixture.direct(ctx, "database", string(fixture.database.Spec.Name), fixture.other, privilege)).
				To(BeTrue(), "database ACLs use the administrative connection despite blocked target cleanup")
		}
		Expect(fixture.direct(ctx, "schema", r3Schema, fixture.reader, r3Usage)).To(BeTrue(),
			"historical target cleanup waits for explicitly restored admission")
		retained := false
		for _, target := range current.Status.ManagedACLTargets {
			for _, binding := range target.Bindings {
				if binding.Schema == r3Schema && binding.Grantee == fixture.reader && binding.Privilege == r3Usage {
					retained = true
				}
			}
		}
		Expect(retained).To(BeTrue(), "blocked cleanup responsibility remains persisted")
		fresh, err := fixture.Server.Open(string(fixture.database.Spec.Name), fixture.Role, fixture.Password)
		Expect(err).NotTo(HaveOccurred())
		Expect(SQLState(fresh.PingContext(ctx))).To(Equal("55000"),
			"an existing catalog connection cannot stand in for fresh target admission")
		Expect(fresh.Close()).To(Succeed())
		fixture.update(ctx, func(spec *epco.DatabaseSpec) { spec.AllowConnections = new(true) })
		fixture.ready(ctx)
		Expect(fixture.direct(ctx, "schema", r3Schema, fixture.reader, r3Usage)).To(BeFalse())
		for _, privilege := range []string{r3Create, r3Connect} {
			Expect(fixture.direct(ctx, "database", string(fixture.database.Spec.Name), fixture.other, privilege)).To(BeTrue())
		}
		Expect(fixture.login(ctx, fixture.Role).PingContext(ctx)).To(Succeed())
	}, NodeTimeout(35*time.Second))

	It("executes no ACL mutation after failed persistence and recovers committed grants on a newer spec",
		func(ctx SpecContext) {
			fixture.ready(ctx)
			fixture.update(ctx, func(spec *epco.DatabaseSpec) {
				spec.Schemas[0].Permissions = &epco.SchemaPermissions{Usage: r3Permissions(fixture.reader, epco.EnsurePresent)}
			})
			blocked := &r3StatusClient{Client: environment.Client, reject: func(*epco.Database) bool { return true }}
			reconciler := databaseRecoveryReconciler(blocked, pg.NewDatabaseFactory())
			Eventually(func() bool {
				_, _ = reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(fixture.database)})
				return blocked.failed.Load() > 0
			}).WithContext(ctx).WithTimeout(5 * time.Second).Should(BeTrue())
			Expect(fixture.direct(ctx, "schema", r3Schema, fixture.reader, r3Usage)).To(BeFalse(),
				"failed checkpoint must prevent the corresponding SQL grant")
			completed := &r3StatusClient{Client: environment.Client, reject: func(*epco.Database) bool {
				return fixture.direct(ctx, "schema", r3Schema, fixture.reader, r3Usage)
			}}
			reconciler = databaseRecoveryReconciler(completed, pg.NewDatabaseFactory())
			Eventually(func() bool {
				_, _ = reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(fixture.database)})
				return completed.failed.Load() > 0
			}).WithContext(ctx).WithTimeout(5 * time.Second).Should(BeTrue())
			Expect(fixture.direct(ctx, "schema", r3Schema, fixture.reader, r3Usage)).To(BeTrue())
			fixture.update(ctx, func(spec *epco.DatabaseSpec) { spec.Schemas[0].Permissions = nil })
			fixture.ready(ctx)
			Expect(fixture.direct(ctx, "schema", r3Schema, fixture.reader, r3Usage)).To(BeFalse(),
				"new reconciler must recover persisted intent without replaying the withdrawn grant")
		}, NodeTimeout(30*time.Second))

	It("reconciles every existing table and sequence privilege and new relations on the next pass", func(ctx SpecContext) {
		fixture.table(ctx, "first")
		fixture.exec(ctx, "SET ROLE "+quoteIdentifier(fixture.Role)+"; CREATE SEQUENCE app.first_seq; "+
			"CREATE VIEW app.first_view AS SELECT * FROM app.first; "+
			"CREATE MATERIALIZED VIEW app.first_mat AS SELECT * FROM app.first; "+
			"CREATE TABLE app.partitioned(value int) PARTITION BY RANGE(value); RESET ROLE")
		fixture.update(ctx, func(spec *epco.DatabaseSpec) {
			p := r3Permissions(fixture.reader, epco.EnsurePresent)
			spec.Schemas[0].Permissions = &epco.SchemaPermissions{Usage: p,
				Tables: &epco.TablePermissions{Select: p, Insert: p, Update: p, Delete: p,
					Truncate: p, References: p, Trigger: p},
				Sequences: &epco.SequencePermissions{Usage: p, Select: p, Update: p}}
		})
		fixture.ready(ctx)
		for _, privilege := range []string{"SELECT", "INSERT", "UPDATE", "DELETE", "TRUNCATE", "REFERENCES", "TRIGGER"} {
			Expect(fixture.direct(ctx, "table", "app.first", fixture.reader, privilege)).To(BeTrue())
		}
		for _, name := range []string{"app.first_view", "app.first_mat", "app.partitioned"} {
			Expect(fixture.direct(ctx, "table", name, fixture.reader, "SELECT")).To(BeTrue())
		}
		for _, privilege := range []string{r3Usage, "SELECT", "UPDATE"} {
			Expect(fixture.direct(ctx, "table", "app.first_seq", fixture.reader, privilege)).To(BeTrue())
		}
		fixture.table(ctx, "later")
		Expect(fixture.direct(ctx, "table", "app.later", fixture.reader, "SELECT")).To(BeFalse(),
			"schema-wide grants do not claim immediate defaults")
		fixture.ready(ctx)
		Expect(fixture.direct(ctx, "table", "app.later", fixture.reader, "SELECT")).To(BeTrue())
		fixture.exec(ctx, "GRANT SELECT ON app.first TO "+quoteIdentifier(fixture.other))
		fixture.update(ctx, func(spec *epco.DatabaseSpec) { spec.Schemas = nil })
		fixture.ready(ctx)
		Expect(fixture.direct(ctx, "table", "app.first", fixture.reader, "SELECT")).To(BeFalse())
		Expect(fixture.direct(ctx, "table", "app.later", fixture.reader, "SELECT")).To(BeFalse())
		Expect(fixture.direct(ctx, "table", "app.first", fixture.other, "SELECT")).To(BeTrue())
		var value int
		Expect(fixture.target.QueryRowContext(ctx, "SELECT value FROM app.first").Scan(&value)).To(Succeed())
		Expect(value).To(Equal(42))
	}, NodeTimeout(40*time.Second))

	It("applies defaults for actual owner and migrator creators across global and schema layers", func(ctx SpecContext) {
		migrator := fixture.role(ctx)
		roleSQL(ctx, fixture.Fixture, "GRANT "+quoteIdentifier(migrator)+" TO "+quoteIdentifier(fixture.Role))
		fixture.exec(ctx, "CREATE SCHEMA second AUTHORIZATION "+quoteIdentifier(fixture.Role)+
			"; GRANT USAGE,CREATE ON SCHEMA app,second TO "+quoteIdentifier(migrator)+", "+
			quoteIdentifier(fixture.other)+"; GRANT USAGE ON SCHEMA app,second TO "+quoteIdentifier(fixture.reader))
		fixture.table(ctx, "before_defaults")
		fixture.update(ctx, func(spec *epco.DatabaseSpec) {
			p := r3Permissions(fixture.reader, epco.EnsurePresent)
			for _, creator := range []string{fixture.Role, migrator} {
				spec.DefaultPrivileges = append(spec.DefaultPrivileges, epco.DefaultPrivileges{
					ForRole: epco.SQLName(creator), Tables: &epco.TablePermissions{Select: p},
					Functions: &epco.FunctionPermissions{Execute: r3Permissions("public", epco.EnsureAbsent)},
					Types:     &epco.TypePermissions{Usage: r3Permissions("public", epco.EnsureAbsent)}})
				spec.Schemas[0].DefaultPrivileges = append(spec.Schemas[0].DefaultPrivileges,
					epco.DefaultPrivileges{ForRole: epco.SQLName(creator), Sequences: &epco.SequencePermissions{Usage: p}})
			}
		})
		fixture.ready(ctx)
		Expect(fixture.defaultGrant(ctx, fixture.Role, "", "r", fixture.reader, "SELECT")).To(BeTrue())
		Expect(fixture.defaultGrant(ctx, migrator, r3Schema, "S", fixture.reader, r3Usage)).To(BeTrue())
		Expect(fixture.direct(ctx, "table", "app.before_defaults", fixture.reader, "SELECT")).To(BeFalse())
		fixture.table(ctx, "owner_created")
		migration := fixture.login(ctx, migrator)
		_, err := migration.ExecContext(ctx, "CREATE TABLE app.migrator_created(value int); "+
			"CREATE TABLE second.migrator_created(value int); CREATE SEQUENCE app.migrator_seq; "+
			"CREATE SEQUENCE second.migrator_seq; "+
			"CREATE FUNCTION app.future_function() RETURNS int LANGUAGE sql AS 'SELECT 42'; "+
			"CREATE TYPE app.future_type AS ENUM('value')")
		Expect(err).NotTo(HaveOccurred())
		for _, table := range []string{"app.owner_created", "app.migrator_created", "second.migrator_created"} {
			Expect(fixture.direct(ctx, "table", table, fixture.reader, "SELECT")).To(BeTrue(),
				"defaults must be present immediately without a reconcile")
		}
		Expect(fixture.direct(ctx, "table", "app.migrator_seq", fixture.reader, r3Usage)).To(BeTrue())
		Expect(fixture.direct(ctx, "table", "second.migrator_seq", fixture.reader, r3Usage)).To(BeFalse(),
			"schema defaults remain local")
		third := fixture.login(ctx, fixture.other)
		_, err = third.ExecContext(ctx, "CREATE TABLE app.third_created(value int)")
		Expect(err).NotTo(HaveOccurred())
		Expect(fixture.direct(ctx, "table", "app.third_created", fixture.reader, "SELECT")).To(BeFalse())
		var allowed bool
		Expect(fixture.target.QueryRowContext(ctx, "SELECT has_function_privilege($1,'app.future_function()','EXECUTE')",
			fixture.reader).Scan(&allowed)).To(Succeed())
		Expect(allowed).To(BeFalse())
		Expect(fixture.target.QueryRowContext(ctx, "SELECT has_type_privilege($1,'app.future_type','USAGE')",
			fixture.reader).Scan(&allowed)).To(Succeed())
		Expect(allowed).To(BeFalse())
		fixture.update(ctx, func(spec *epco.DatabaseSpec) {
			spec.DefaultPrivileges = nil
			spec.Schemas[0].DefaultPrivileges = nil
		})
		fixture.ready(ctx)
		Expect(fixture.direct(ctx, "table", "app.migrator_created", fixture.reader, "SELECT")).To(BeTrue(),
			"default cleanup never revokes current-table ACLs")
		_, err = migration.ExecContext(ctx,
			"CREATE TABLE app.after_removal(value int); CREATE SEQUENCE app.after_removal_seq")
		Expect(err).NotTo(HaveOccurred())
		Expect(fixture.direct(ctx, "table", "app.after_removal", fixture.reader, "SELECT")).To(BeFalse())
		Expect(fixture.direct(ctx, "table", "app.after_removal_seq", fixture.reader, r3Usage)).To(BeFalse())
	}, NodeTimeout(40*time.Second))

	It("keeps additive schema default absence separate from global PUBLIC access", func(ctx SpecContext) {
		fixture.update(ctx, func(spec *epco.DatabaseSpec) {
			spec.Schemas[0].DefaultPrivileges = []epco.DefaultPrivileges{{ForRole: epco.SQLName(fixture.Role),
				Functions: &epco.FunctionPermissions{Execute: r3Permissions("public", epco.EnsureAbsent)},
				Types:     &epco.TypePermissions{Usage: r3Permissions("public", epco.EnsureAbsent)}}}
		})
		fixture.ready(ctx)
		Expect(fixture.defaultGrant(ctx, fixture.Role, r3Schema, "f", "public", "EXECUTE")).To(BeFalse())
		fixture.exec(ctx, "SET ROLE "+quoteIdentifier(fixture.Role)+
			"; CREATE FUNCTION app.globally_callable() RETURNS int LANGUAGE sql AS 'SELECT 42'; "+
			"CREATE TYPE app.globally_usable AS ENUM('value'); RESET ROLE")
		var allowed bool
		Expect(fixture.target.QueryRowContext(ctx, "SELECT has_function_privilege($1,'app.globally_callable()','EXECUTE')",
			fixture.reader).Scan(&allowed)).To(Succeed())
		Expect(allowed).To(BeTrue(), "schema-local absence cannot remove the global built-in PUBLIC default")
		Expect(fixture.target.QueryRowContext(ctx, "SELECT has_type_privilege($1,'app.globally_usable','USAGE')",
			fixture.reader).Scan(&allowed)).To(Succeed())
		Expect(allowed).To(BeTrue())
	})

	It("restricts named SELECT to its allowlist and withdraws only the removed table", func(ctx SpecContext) {
		fixture.table(ctx, r3AllowedTable)
		fixture.table(ctx, "also_allowed")
		fixture.table(ctx, "unlisted")
		fixture.update(ctx, func(spec *epco.DatabaseSpec) {
			spec.Schemas[0].Permissions = &epco.SchemaPermissions{Usage: r3Permissions(fixture.reader, epco.EnsurePresent)}
			for _, name := range []string{r3AllowedTable, "also_allowed"} {
				spec.Schemas[0].TablePermissions = append(spec.Schemas[0].TablePermissions,
					epco.NamedTablePermissions{Name: epco.SQLName(name),
						Permissions: epco.TablePermissions{Select: r3Permissions(fixture.reader, epco.EnsurePresent)}})
			}
		})
		fixture.ready(ctx)
		reader := fixture.login(ctx, fixture.reader)
		var value int
		Expect(reader.QueryRowContext(ctx, "SELECT value FROM app.r3_allowed").Scan(&value)).To(Succeed())
		Expect(value).To(Equal(42))
		for _, query := range []string{"SELECT * FROM app.unlisted", "INSERT INTO app.r3_allowed VALUES(7)",
			"UPDATE app.r3_allowed SET value=7", "DELETE FROM app.r3_allowed", "CREATE TABLE app.not_allowed(value int)"} {
			_, err := reader.ExecContext(ctx, query)
			Expect(SQLState(err)).To(Equal("42501"))
		}
		fixture.table(ctx, "new_unlisted")
		fixture.ready(ctx)
		Expect(fixture.direct(ctx, "table", "app.new_unlisted", fixture.reader, "SELECT")).To(BeFalse())
		fixture.update(ctx, func(spec *epco.DatabaseSpec) {
			spec.Schemas[0].TablePermissions = spec.Schemas[0].TablePermissions[1:]
		})
		fixture.ready(ctx)
		Expect(fixture.direct(ctx, "table", "app.r3_allowed", fixture.reader, "SELECT")).To(BeFalse())
		Expect(fixture.direct(ctx, "table", "app.also_allowed", fixture.reader, "SELECT")).To(BeTrue())
		Expect(fixture.target.QueryRowContext(ctx, "SELECT value FROM app.r3_allowed").Scan(&value)).To(Succeed())
		Expect(value).To(Equal(42))
	}, NodeTimeout(40*time.Second))

	It("keeps lifecycle ownership while a named table is missing and converges when it appears", func(ctx SpecContext) {
		fixture.update(ctx, func(spec *epco.DatabaseSpec) {
			spec.Schemas[0].TablePermissions = []epco.NamedTablePermissions{{Name: r3MissingTable,
				Permissions: epco.TablePermissions{Select: r3Permissions(fixture.reader, epco.EnsurePresent)}}}
		})
		current := fixture.pending(ctx, "DependenciesNotReady")
		Expect(current.Status.HasManagedSQL).To(BeTrue(),
			"confirmed database/schema phase reserves the name even when table ACL is pending")
		var missing bool
		Expect(fixture.target.QueryRowContext(ctx,
			"SELECT to_regclass('app.r3_missing') IS NULL").Scan(&missing)).To(Succeed())
		Expect(missing).To(BeTrue(), "permission reconciliation must never create a table")
		competitor := &epco.Database{Name: "contender", Namespace: fixture.scope.Namespace.Name, Spec: current.Spec}
		Expect(fixture.scope.Create(ctx, competitor)).To(Succeed())
		reconciler := databaseRecoveryReconciler(environment.Client, pg.NewDatabaseFactory())
		_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(competitor)})
		Expect(err).NotTo(HaveOccurred())
		other := recoveredDatabase(ctx, environment.Client, competitor)
		Expect(meta.FindStatusCondition(other.Status.Conditions, "Ready").Reason).To(Equal("OwnershipConflict"))
		fixture.table(ctx, r3MissingTable)
		fixture.ready(ctx)
		Expect(fixture.direct(ctx, "table", "app.r3_missing", fixture.reader, "SELECT")).To(BeTrue())
		fixture.update(ctx, func(spec *epco.DatabaseSpec) {
			spec.Schemas[0].TablePermissions = []epco.NamedTablePermissions{{Name: "only_absent",
				Permissions: epco.TablePermissions{Select: r3Permissions(fixture.reader, epco.EnsureAbsent)}}}
		})
		fixture.ready(ctx)
		Expect(fixture.direct(ctx, "table", "app.r3_missing", fixture.reader, "SELECT")).To(BeFalse())
	}, NodeTimeout(40*time.Second))

	It("follows renamed managed bindings for cleanup and treats DROP/recreate as a new table", func(ctx SpecContext) {
		fixture.table(ctx, r3AllowedTable)
		fixture.update(ctx, func(spec *epco.DatabaseSpec) {
			spec.Schemas[0].TablePermissions = []epco.NamedTablePermissions{{Name: r3AllowedTable,
				Permissions: epco.TablePermissions{Select: r3Permissions(fixture.reader, epco.EnsurePresent)}}}
		})
		fixture.ready(ctx)
		fixture.exec(ctx, "ALTER TABLE app.r3_allowed RENAME TO renamed")
		fixture.pending(ctx, "DependenciesNotReady")
		Expect(fixture.direct(ctx, "table", "app.renamed", fixture.reader, "SELECT")).To(BeFalse(),
			"old managed binding is cleaned despite the missing declared name")
		fixture.table(ctx, r3AllowedTable)
		fixture.ready(ctx)
		Expect(fixture.direct(ctx, "table", "app.r3_allowed", fixture.reader, "SELECT")).To(BeTrue())
		var before, after int64
		Expect(fixture.target.QueryRowContext(ctx, "SELECT 'app.r3_allowed'::regclass::oid").Scan(&before)).To(Succeed())
		fixture.exec(ctx, "DROP TABLE app.r3_allowed")
		fixture.table(ctx, r3AllowedTable)
		Expect(fixture.target.QueryRowContext(ctx, "SELECT 'app.r3_allowed'::regclass::oid").Scan(&after)).To(Succeed())
		Expect(after).NotTo(Equal(before))
		fixture.ready(ctx)
		Expect(fixture.direct(ctx, "table", "app.r3_allowed", fixture.reader, "SELECT")).To(BeTrue())
		Expect(fixture.direct(ctx, "table", "app.renamed", fixture.reader, "SELECT")).To(BeFalse())
	}, NodeTimeout(40*time.Second))

	DescribeTable("version-gates table and default MAINTAIN before any requested ACL mutation",
		func(ctx SpecContext, defaults bool) {
			fixture.table(ctx, "maintenance")
			fixture.update(ctx, func(spec *epco.DatabaseSpec) {
				p := r3Permissions(fixture.reader, epco.EnsurePresent)
				if defaults {
					spec.DefaultPrivileges = []epco.DefaultPrivileges{{ForRole: epco.SQLName(fixture.Role),
						Tables: &epco.TablePermissions{Select: p, Maintain: p}}}
				} else {
					spec.Schemas[0].TablePermissions = []epco.NamedTablePermissions{{Name: "maintenance",
						Permissions: epco.TablePermissions{Select: p, Maintain: p}}}
				}
			})
			if fixture.version < 170000 {
				fixture.pending(ctx, "UnsupportedFeature")
				Expect(fixture.direct(ctx, "table", "app.maintenance", fixture.reader, "SELECT")).To(BeFalse())
				Expect(fixture.defaultGrant(ctx, fixture.Role, "", "r", fixture.reader, "SELECT")).To(BeFalse())
				return
			}
			fixture.ready(ctx)
			if defaults {
				fixture.table(ctx, "future_maintenance")
				Expect(fixture.direct(ctx, "table", "app.future_maintenance", fixture.reader, "MAINTAIN")).To(BeTrue())
				Expect(fixture.defaultGrant(ctx, fixture.Role, "", "r", fixture.reader, "MAINTAIN")).To(BeTrue())
			} else {
				Expect(fixture.direct(ctx, "table", "app.maintenance", fixture.reader, "MAINTAIN")).To(BeTrue())
			}
			fixture.update(ctx, func(spec *epco.DatabaseSpec) {
				if defaults {
					spec.DefaultPrivileges = nil
				} else {
					spec.Schemas[0].TablePermissions = nil
				}
			})
			fixture.ready(ctx)
			if defaults {
				Expect(fixture.defaultGrant(ctx, fixture.Role, "", "r", fixture.reader, "MAINTAIN")).To(BeFalse())
			} else {
				Expect(fixture.direct(ctx, "table", "app.maintenance", fixture.reader, "MAINTAIN")).To(BeFalse())
			}
		}, Entry("named table", false), Entry("table defaults", true))

	It("confirms restricted grant-option results instead of treating a partial GRANT warning as success",
		func(ctx SpecContext) {
			fixture.exec(ctx, "SET ROLE "+quoteIdentifier(fixture.other)+
				"; RESET ROLE; CREATE TABLE app.partial(value int); ALTER TABLE app.partial OWNER TO "+
				quoteIdentifier(fixture.other)+"; GRANT SELECT ON app.partial TO "+quoteIdentifier(fixture.Role)+
				" WITH GRANT OPTION")
			fixture.update(ctx, func(spec *epco.DatabaseSpec) {
				p := r3Permissions(fixture.reader, epco.EnsurePresent)
				spec.Schemas[0].TablePermissions = []epco.NamedTablePermissions{{Name: "partial",
					Permissions: epco.TablePermissions{Select: p, Insert: p}}}
			})
			fixture.pending(ctx, "InsufficientPrivileges")
			Expect(fixture.direct(ctx, "table", "app.partial", fixture.reader, "INSERT")).To(BeFalse())
			var superuser bool
			Expect(fixture.target.QueryRowContext(ctx, "SELECT rolsuper FROM pg_roles WHERE rolname=$1",
				fixture.Role).Scan(&superuser)).To(Succeed())
			Expect(superuser).To(BeFalse())
			Expect(roleMemberships(ctx, fixture.Fixture, fixture.Role)).NotTo(ContainElement(fixture.other))
			fixture.exec(ctx, "GRANT INSERT ON app.partial TO "+quoteIdentifier(fixture.Role)+" WITH GRANT OPTION")
			fixture.ready(ctx)
			Expect(fixture.direct(ctx, "table", "app.partial", fixture.reader, "SELECT")).To(BeTrue())
			Expect(fixture.direct(ctx, "table", "app.partial", fixture.reader, "INSERT")).To(BeTrue())
			fixture.update(ctx, func(spec *epco.DatabaseSpec) { spec.Schemas[0].TablePermissions = nil })
			fixture.ready(ctx)
			Expect(fixture.direct(ctx, "table", "app.partial", fixture.reader, "SELECT")).To(BeFalse())
			Expect(fixture.direct(ctx, "table", "app.partial", fixture.Role, "SELECT")).To(BeTrue(),
				"independent grant option remains")
		}, NodeTimeout(40*time.Second))

	It("preserves independent grantor rows when withdrawing a selected preexisting table grant", func(ctx SpecContext) {
		fixture.table(ctx, "multiple_grantors")
		fixture.exec(ctx, "GRANT USAGE ON SCHEMA app TO "+quoteIdentifier(fixture.other))
		fixture.exec(ctx, "SET ROLE "+quoteIdentifier(fixture.Role)+
			"; GRANT SELECT ON app.multiple_grantors TO "+quoteIdentifier(fixture.reader)+
			"; GRANT SELECT ON app.multiple_grantors TO "+quoteIdentifier(fixture.other)+
			" WITH GRANT OPTION; RESET ROLE; SET ROLE "+quoteIdentifier(fixture.other)+
			"; GRANT SELECT ON app.multiple_grantors TO "+quoteIdentifier(fixture.reader)+"; RESET ROLE")
		count := func() int {
			var n int
			Expect(fixture.target.QueryRowContext(ctx, `SELECT count(*) FROM pg_class c
			 CROSS JOIN LATERAL aclexplode(c.relacl) a WHERE c.oid='app.multiple_grantors'::regclass
			 AND a.grantee=(SELECT oid FROM pg_roles WHERE rolname=$1)
			 AND a.privilege_type='SELECT'`, fixture.reader).Scan(&n)).To(Succeed())
			return n
		}
		Expect(count()).To(Equal(2))
		fixture.update(ctx, func(spec *epco.DatabaseSpec) {
			spec.Schemas[0].TablePermissions = []epco.NamedTablePermissions{{Name: "multiple_grantors",
				Permissions: epco.TablePermissions{Select: r3Permissions(fixture.reader, epco.EnsurePresent)}}}
		})
		fixture.ready(ctx)
		fixture.update(ctx, func(spec *epco.DatabaseSpec) { spec.Schemas[0].TablePermissions = nil })
		fixture.ready(ctx)
		Expect(count()).To(Equal(1), "withdrawal removes the one adopted binding and preserves independent grantor access")
		var allowed bool
		Expect(fixture.target.QueryRowContext(ctx, "SELECT has_table_privilege($1,'app.multiple_grantors','SELECT')",
			fixture.reader).Scan(&allowed)).To(Succeed())
		Expect(allowed).To(BeTrue(), "direct managed cleanup does not promise effective denial")
	}, NodeTimeout(30*time.Second))

	It("keeps RESTRICT cleanup pending when an adopted grant option has downstream dependents", func(ctx SpecContext) {
		fixture.table(ctx, "dependent")
		fixture.exec(ctx, "GRANT USAGE ON SCHEMA app TO "+quoteIdentifier(fixture.reader))
		fixture.exec(ctx, "SET ROLE "+quoteIdentifier(fixture.Role)+
			"; GRANT SELECT ON app.dependent TO "+quoteIdentifier(fixture.reader)+
			" WITH GRANT OPTION; RESET ROLE; SET ROLE "+quoteIdentifier(fixture.reader)+
			"; GRANT SELECT ON app.dependent TO "+quoteIdentifier(fixture.other)+"; RESET ROLE")
		fixture.update(ctx, func(spec *epco.DatabaseSpec) {
			spec.Schemas[0].TablePermissions = []epco.NamedTablePermissions{{Name: "dependent",
				Permissions: epco.TablePermissions{Select: r3Permissions(fixture.reader, epco.EnsurePresent)}}}
		})
		fixture.ready(ctx)
		fixture.update(ctx, func(spec *epco.DatabaseSpec) { spec.Schemas[0].TablePermissions = nil })
		fixture.pending(ctx, "DeletionBlocked")
		Expect(fixture.direct(ctx, "table", "app.dependent", fixture.reader, "SELECT")).To(BeTrue())
		Expect(fixture.direct(ctx, "table", "app.dependent", fixture.other, "SELECT")).To(BeTrue(),
			"cleanup must not fall back to CASCADE")
		fixture.exec(ctx, "SET ROLE "+quoteIdentifier(fixture.reader)+
			"; REVOKE SELECT ON app.dependent FROM "+quoteIdentifier(fixture.other)+" RESTRICT; RESET ROLE")
		fixture.ready(ctx)
		Expect(fixture.direct(ctx, "table", "app.dependent", fixture.reader, "SELECT")).To(BeFalse())
	}, NodeTimeout(40*time.Second))
})
