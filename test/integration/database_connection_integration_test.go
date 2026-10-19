//go:build integration

package integration

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	epco "github.com/vterdunov/external-postgres-control-operator/api/v1alpha1"
	pg "github.com/vterdunov/external-postgres-control-operator/internal/postgres"
	kubeenv "github.com/vterdunov/external-postgres-control-operator/test/envtest"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var _ = Describe("R4 application connections with real API and PostgreSQL", Ordered, ContinueOnFailure, func() {
	var environment *kubeenv.Environment
	var fixture *r4ConnectionFixture
	BeforeAll(func(ctx SpecContext) { environment = r4StartAPI(ctx) }, NodeTimeout(60*time.Second))
	BeforeEach(func(ctx SpecContext) { fixture = r4NewFixture(ctx, environment) }, NodeTimeout(30*time.Second))
	It("exports credentials that authenticate as the application in the effective database", func(ctx SpecContext) {
		secret := fixture.bootstrap(ctx)
		recoverDatabaseReady(ctx, fixture.api,
			databaseRecoveryReconciler(fixture.api, pg.NewDatabaseFactory()), fixture.database)
		Expect(secret.Type).To(Equal(corev1.SecretTypeBasicAuth))
		Expect(bytes.Equal(secret.Data[corev1.BasicAuthUsernameKey], []byte(fixture.Role))).To(BeTrue())
		Expect(string(secret.Data["dbname"])).To(Equal(string(fixture.database.Spec.Name)))
		Expect(fixture.login(ctx, string(secret.Data["dbname"]), string(secret.Data["username"]),
			string(secret.Data["password"]))).To(BeTrue(), "a fresh isolated application session accepts exported credentials")
		r4SecretFree(recoveryRole(ctx, fixture.api, fixture.role).Status, []byte(fixture.Password))
		r4SecretFree(recoveredDatabase(ctx, fixture.api, fixture.database).Status, []byte(fixture.Password))
		r4SecretFree(secret.Annotations, []byte(fixture.Password))
	}, NodeTimeout(30*time.Second))
	It("preserves the old output when SQL fails and applies only the newest rotation on retry", func(ctx SpecContext) {
		before := fixture.bootstrap(ctx)
		first, err := randomName("rotation-")
		Expect(err).NotTo(HaveOccurred())
		latest, err := randomName("rotation-")
		Expect(err).NotTo(HaveOccurred())
		fixture.rotate(ctx, fixture.source, []byte(first))
		factory := &r4RoleFactory{RoleFactory: pg.NewRoleFactory()}
		factory.reject.Store(true)
		_, err = recoveryReconciler(fixture.api, factory).Reconcile(ctx,
			ctrl.Request{NamespacedName: client.ObjectKeyFromObject(fixture.role)})
		Expect(r4ReconcileSucceeded(err)).To(BeTrue())
		r4NotReady(fixture.reconcile(ctx, fixture.database, fixture.api))
		r4Unchanged(before, fixture.output(ctx, r4OutputName))
		Expect(fixture.login(ctx, string(fixture.database.Spec.Name), fixture.Role, fixture.Password)).To(BeTrue())
		Expect(fixture.login(ctx, string(fixture.database.Spec.Name), fixture.Role, first)).To(BeFalse())
		fixture.rotate(ctx, fixture.source, []byte(latest))
		factory.reject.Store(false)
		recoverUntilReady(ctx, fixture.api, recoveryReconciler(fixture.api, factory), fixture.role)
		fixture.refreshed(ctx, fixture.database, r4OutputName, []byte(latest), fixture.api)
		recoverDatabaseReady(ctx, fixture.api,
			databaseRecoveryReconciler(fixture.api, pg.NewDatabaseFactory()), fixture.database)
		r4Password(fixture.output(ctx, r4OutputName), []byte(latest))
		Expect(fixture.login(ctx, string(fixture.database.Spec.Name), fixture.Role, latest)).To(BeTrue())
		Expect(fixture.login(ctx, string(fixture.database.Spec.Name), fixture.Role, first)).To(BeFalse())
	}, NodeTimeout(40*time.Second))
	It("keeps output pending after SQL success until the application receipt persists", func(ctx SpecContext) {
		before := fixture.bootstrap(ctx)
		password, err := randomName("receipt-")
		Expect(err).NotTo(HaveOccurred())
		fixture.rotate(ctx, fixture.source, []byte(password))
		var applied atomic.Bool
		factory := &r4RoleFactory{RoleFactory: pg.NewRoleFactory(), after: func() { applied.Store(true) }}
		fault := &recoveryStatusClient{Client: fixture.api, fail: func(*epco.DatabaseRole) bool { return applied.Load() }}
		Eventually(func() bool {
			_, _ = recoveryReconciler(fault, factory).Reconcile(ctx,
				ctrl.Request{NamespacedName: client.ObjectKeyFromObject(fixture.role)})
			return applied.Load() && fault.failed.Load() > 0
		}).WithContext(ctx).WithTimeout(5 * time.Second).Should(BeTrue())
		Expect(fixture.login(ctx, string(fixture.database.Spec.Name), fixture.Role, password)).To(BeTrue())
		r4NotReady(fixture.reconcile(ctx, fixture.database, fixture.api))
		r4Unchanged(before, fixture.output(ctx, r4OutputName))
		recoverUntilReady(ctx, fixture.api, recoveryReconciler(fixture.api, pg.NewRoleFactory()), fixture.role)
		Expect(fixture.login(ctx, string(fixture.database.Spec.Name), fixture.Role, password)).To(BeTrue(),
			"the replacement source is applied to SQL despite advanced legacy status fields")
		fixture.refreshed(ctx, fixture.database, r4OutputName, []byte(password), fixture.api)
		recoverDatabaseReady(ctx, fixture.api,
			databaseRecoveryReconciler(fixture.api, pg.NewDatabaseFactory()), fixture.database)
		r4Password(fixture.output(ctx, r4OutputName), []byte(password))
	}, NodeTimeout(40*time.Second))
	It("records the source snapshot actually applied when another rotation arrives during SQL", func(ctx SpecContext) {
		before := fixture.bootstrap(ctx)
		first, err := randomName("snapshot-")
		Expect(err).NotTo(HaveOccurred())
		latest, err := randomName("snapshot-")
		Expect(err).NotTo(HaveOccurred())
		fixture.rotate(ctx, fixture.source, []byte(first))
		var rotated atomic.Bool
		factory := &r4RoleFactory{RoleFactory: pg.NewRoleFactory(), after: func() {
			if rotated.CompareAndSwap(false, true) {
				fixture.rotate(ctx, fixture.source, []byte(latest))
			}
		}}
		_, err = recoveryReconciler(fixture.api, factory).Reconcile(ctx,
			ctrl.Request{NamespacedName: client.ObjectKeyFromObject(fixture.role)})
		Expect(r4ReconcileSucceeded(err)).To(BeTrue())
		Expect(rotated.Load()).To(BeTrue())
		Expect(fixture.login(ctx, string(fixture.database.Spec.Name), fixture.Role, first)).To(BeTrue())
		r4NotReady(fixture.reconcile(ctx, fixture.database, fixture.api))
		r4Unchanged(before, fixture.output(ctx, r4OutputName))
		recoverUntilReady(ctx, fixture.api, recoveryReconciler(fixture.api, pg.NewRoleFactory()), fixture.role)
		fixture.refreshed(ctx, fixture.database, r4OutputName, []byte(latest), fixture.api)
		recoverDatabaseReady(ctx, fixture.api,
			databaseRecoveryReconciler(fixture.api, pg.NewDatabaseFactory()), fixture.database)
		r4Password(fixture.output(ctx, r4OutputName), []byte(latest))
	}, NodeTimeout(40*time.Second))
	It("retries a failed output write while independent outputs converge without falsely becoming Ready",
		func(ctx SpecContext) {
			second := fixture.addDatabase(ctx, "second", fixture.role, "second-connection")
			before := fixture.bootstrap(ctx)
			fixture.published(ctx, second, "second-connection")
			password, err := randomName("fanout-")
			Expect(err).NotTo(HaveOccurred())
			fixture.rotate(ctx, fixture.source, []byte(password))
			recoverUntilReady(ctx, fixture.api, recoveryReconciler(fixture.api, pg.NewRoleFactory()), fixture.role)
			fault := &r4OutputClient{Client: fixture.api, name: r4OutputName}
			fault.reject.Store(true)
			Eventually(func() bool {
				_, _ = databaseRecoveryReconciler(fault, pg.NewDatabaseFactory()).Reconcile(ctx,
					ctrl.Request{NamespacedName: client.ObjectKeyFromObject(fixture.database)})
				return fault.failed.Load() > 0
			}).WithContext(ctx).WithTimeout(5 * time.Second).Should(BeTrue())
			r4NotReady(recoveredDatabase(ctx, fixture.api, fixture.database))
			r4Unchanged(before, fixture.output(ctx, r4OutputName))
			fixture.refreshed(ctx, second, "second-connection", []byte(password), fixture.api)
			recoverDatabaseReady(ctx, fixture.api, databaseRecoveryReconciler(fixture.api, pg.NewDatabaseFactory()), second)
			r4Password(fixture.output(ctx, "second-connection"), []byte(password))
			fault.reject.Store(false)
			fixture.refreshed(ctx, fixture.database, r4OutputName, []byte(password), fault)
			recoverDatabaseReady(ctx, fixture.api, databaseRecoveryReconciler(fault, pg.NewDatabaseFactory()), fixture.database)
			r4Password(fixture.output(ctx, r4OutputName), []byte(password))
		}, NodeTimeout(45*time.Second))
	It("exports independent service and admin-UI credentials for the same database", func(ctx SpecContext) {
		name, err := randomName("r4_reader_")
		Expect(err).NotTo(HaveOccurred())
		password, err := randomName("reader-")
		Expect(err).NotTo(HaveOccurred())
		reader, _ := fixture.addRole(ctx, "admin-ui", name, password)
		fixture.editDatabase(ctx, func(spec *epco.DatabaseSpec) {
			spec.ConnectionSecrets = append(spec.ConnectionSecrets,
				epco.ConnectionSecret{Name: "admin-ui-connection", RoleRef: epco.LocalReference{Name: reader.Name}})
		})
		recoverUntilReady(ctx, fixture.api, recoveryReconciler(fixture.api, pg.NewRoleFactory()), reader)
		fixture.bootstrap(ctx)
		output := fixture.output(ctx, "admin-ui-connection")
		r4Password(output, []byte(password))
		Expect(bytes.Equal(output.Data["username"], []byte(name))).To(BeTrue())
		Expect(fixture.login(ctx, string(fixture.database.Spec.Name), name, password)).To(BeTrue())
		Expect(fixture.login(ctx, string(fixture.database.Spec.Name), name, fixture.Password)).To(BeFalse())
	}, NodeTimeout(35*time.Second))
	DescribeTable("requires a new successful receipt after credential source identity changes",
		func(ctx SpecContext, rename bool) {
			before := fixture.bootstrap(ctx)
			password, err := randomName("identity-")
			Expect(err).NotTo(HaveOccurred())
			source := &corev1.Secret{Name: fixture.source.Name, Namespace: fixture.source.Namespace,
				Type: corev1.SecretTypeBasicAuth, Data: map[string][]byte{
					corev1.BasicAuthUsernameKey: []byte(fixture.Role), corev1.BasicAuthPasswordKey: []byte(password)}}
			if rename {
				source.Name = "renamed-source"
				Expect(fixture.scope.Create(ctx, source)).To(Succeed())
				role := recoveryRole(ctx, fixture.api, fixture.role)
				role.Spec.PasswordSecret.Name = source.Name
				Expect(fixture.api.Update(ctx, role)).To(Succeed())
			} else {
				Expect(fixture.api.Delete(ctx, fixture.source)).To(Succeed())
				Expect(fixture.api.Create(ctx, source)).To(Succeed())
				Expect(source.UID).NotTo(Equal(fixture.source.UID))
				DeferCleanup(func(ctx SpecContext) {
					Expect(client.IgnoreNotFound(fixture.api.Delete(ctx, source,
						client.Preconditions{UID: &source.UID}))).To(Succeed())
				})
			}
			// A legacy Ready/global-version record cannot stand in for the exact
			// successful-application receipt. Keep any receipt unchanged while
			// advancing only fields already present in the R3 status API.
			legacy := recoveryRole(ctx, fixture.api, fixture.role)
			legacy.Status.SecretResourceVersion = source.ResourceVersion
			legacy.Status.AppliedGeneration = legacy.Generation
			for i := range legacy.Status.Conditions {
				legacy.Status.Conditions[i].ObservedGeneration = legacy.Generation
			}
			Expect(fixture.api.Status().Update(ctx, legacy)).To(Succeed())
			Expect(fixture.login(ctx, string(fixture.database.Spec.Name), fixture.Role, fixture.Password)).To(BeTrue())
			Expect(fixture.login(ctx, string(fixture.database.Spec.Name), fixture.Role, password)).To(BeFalse(),
				"legacy status changes have not applied the replacement source to SQL")
			r4NotReady(fixture.reconcile(ctx, fixture.database, fixture.api))
			r4Unchanged(before, fixture.output(ctx, r4OutputName))
			recoverUntilReady(ctx, fixture.api, recoveryReconciler(fixture.api, pg.NewRoleFactory()), fixture.role)
			fixture.refreshed(ctx, fixture.database, r4OutputName, []byte(password), fixture.api)
			recoverDatabaseReady(ctx, fixture.api,
				databaseRecoveryReconciler(fixture.api, pg.NewDatabaseFactory()), fixture.database)
			r4Password(fixture.output(ctx, r4OutputName), []byte(password))
		},
		Entry("source name and role generation", true, NodeTimeout(40*time.Second)),
		Entry("source UID after delete and recreation", false, NodeTimeout(40*time.Second)),
	)
	It("recreates a deleted derived output only after the current source is applied", func(ctx SpecContext) {
		before := fixture.bootstrap(ctx)
		password, err := randomName("recreation-")
		Expect(err).NotTo(HaveOccurred())
		fixture.rotate(ctx, fixture.source, []byte(password))
		Expect(fixture.api.Delete(ctx, before, client.Preconditions{UID: &before.UID})).To(Succeed())
		fixture.reconcile(ctx, fixture.database, fixture.api)
		key := client.ObjectKeyFromObject(before)
		Expect(apierrors.IsNotFound(fixture.api.Get(ctx, key, &corev1.Secret{}))).To(BeTrue(),
			"a previously published output cannot return through the early-publication path")
		recoverUntilReady(ctx, fixture.api, recoveryReconciler(fixture.api, pg.NewRoleFactory()), fixture.role)
		output := fixture.published(ctx, fixture.database, r4OutputName)
		Expect(output.UID).NotTo(Equal(before.UID))
		r4Password(output, []byte(password))
	}, NodeTimeout(35*time.Second))
	DescribeTable("uses effective CONNECT while making no implicit direct grants",
		func(ctx SpecContext, public bool) {
			fixture.bootstrap(ctx)
			database := quoteIdentifier(string(fixture.database.Spec.Name))
			roleSQL(ctx, fixture.Fixture, "REVOKE CONNECT ON DATABASE "+database+" FROM PUBLIC")
			if public {
				roleSQL(ctx, fixture.Fixture, "GRANT CONNECT ON DATABASE "+database+" TO PUBLIC")
			} else {
				parent, err := randomName("r4_parent_")
				Expect(err).NotTo(HaveOccurred())
				roleSQL(ctx, fixture.Fixture, "CREATE ROLE "+quoteIdentifier(parent)+" NOLOGIN")
				DeferCleanup(func() {
					cleanup, cancel := context.WithTimeout(context.Background(), 15*time.Second)
					defer cancel()
					Expect(fixture.Server.withCleanupConnection(cleanup, roleSQLAdmin, func(connection *sql.Conn) error {
						_, revokeErr := connection.ExecContext(cleanup,
							"REVOKE CONNECT ON DATABASE "+database+" FROM "+quoteIdentifier(parent))
						_, dropErr := connection.ExecContext(cleanup, "DROP ROLE "+quoteIdentifier(parent))
						return errors.Join(revokeErr, dropErr)
					})).To(Succeed())
				})
				roleSQL(ctx, fixture.Fixture, "GRANT CONNECT ON DATABASE "+database+" TO "+quoteIdentifier(parent))
				role := recoveryRole(ctx, fixture.api, fixture.role)
				role.Spec.InRoles = []epco.SQLName{epco.SQLName(parent)}
				Expect(fixture.api.Update(ctx, role)).To(Succeed())
			}
			password, err := randomName("effective-")
			Expect(err).NotTo(HaveOccurred())
			fixture.rotate(ctx, fixture.source, []byte(password))
			recoverUntilReady(ctx, fixture.api, recoveryReconciler(fixture.api, pg.NewRoleFactory()), fixture.role)
			fixture.refreshed(ctx, fixture.database, r4OutputName, []byte(password), fixture.api)
			recoverDatabaseReady(ctx, fixture.api,
				databaseRecoveryReconciler(fixture.api, pg.NewDatabaseFactory()), fixture.database)
			r4Password(fixture.output(ctx, r4OutputName), []byte(password))
			var direct bool
			Expect(fixture.Server.Admin.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_database d
			 CROSS JOIN LATERAL aclexplode(COALESCE(d.datacl,acldefault('d',d.datdba))) a
			 WHERE d.datname=$1 AND a.grantee=(SELECT oid FROM pg_roles WHERE rolname=$2)
			 AND a.privilege_type='CONNECT')`, string(fixture.database.Spec.Name), fixture.Role).Scan(&direct)).To(Succeed())
			Expect(direct).To(BeFalse(), "export does not acquire direct SQL access")
		},
		Entry("PUBLIC grants", true, NodeTimeout(35*time.Second)),
		Entry("inherited grants", false, NodeTimeout(35*time.Second)),
	)
	It("preserves existing output without effective CONNECT until a declared CONNECT converges", func(ctx SpecContext) {
		before := fixture.bootstrap(ctx)
		roleSQL(ctx, fixture.Fixture, "REVOKE CONNECT ON DATABASE "+
			quoteIdentifier(string(fixture.database.Spec.Name))+" FROM PUBLIC")
		password, err := randomName("connect-")
		Expect(err).NotTo(HaveOccurred())
		fixture.rotate(ctx, fixture.source, []byte(password))
		recoverUntilReady(ctx, fixture.api, recoveryReconciler(fixture.api, pg.NewRoleFactory()), fixture.role)
		r4NotReady(fixture.reconcile(ctx, fixture.database, fixture.api))
		r4Unchanged(before, fixture.output(ctx, r4OutputName))
		fixture.editDatabase(ctx, func(spec *epco.DatabaseSpec) {
			spec.Permissions = &epco.DatabasePermissions{Connect: r3Permissions(fixture.Role, epco.EnsurePresent)}
		})
		fixture.refreshed(ctx, fixture.database, r4OutputName, []byte(password), fixture.api)
		recoverDatabaseReady(ctx, fixture.api,
			databaseRecoveryReconciler(fixture.api, pg.NewDatabaseFactory()), fixture.database)
		r4Password(fixture.output(ctx, r4OutputName), []byte(password))
		Expect(fixture.login(ctx, string(fixture.database.Spec.Name), fixture.Role, password)).To(BeTrue())
	}, NodeTimeout(35*time.Second))
	DescribeTable("refreshes an eligible output despite an unrelated ACL dependency and keeps aggregate Ready false",
		func(ctx SpecContext, blocker string) {
			fixture.bootstrap(ctx)
			missing, err := randomName("r4_missing_")
			Expect(err).NotTo(HaveOccurred())
			fixture.editDatabase(ctx, func(spec *epco.DatabaseSpec) {
				spec.Permissions = &epco.DatabasePermissions{Connect: r3Permissions(fixture.Role, epco.EnsurePresent)}
				switch blocker {
				case "table":
					spec.Schemas = []epco.SchemaSpec{{Name: databaseRecoveryApplicationName,
						TablePermissions: []epco.NamedTablePermissions{{Name: "not-created",
							Permissions: epco.TablePermissions{Select: r3Permissions(fixture.Role, epco.EnsurePresent)}}}}}
				case "grantee":
					spec.Permissions.Connect = append(spec.Permissions.Connect,
						epco.Permission{Name: epco.SQLName(missing), Ensure: epco.EnsurePresent})
				case "defaults":
					spec.DefaultPrivileges = []epco.DefaultPrivileges{{ForRole: epco.SQLName(missing),
						Tables: &epco.TablePermissions{Select: r3Permissions(fixture.Role, epco.EnsurePresent)}}}
				}
			})
			password, err := randomName("independent-")
			Expect(err).NotTo(HaveOccurred())
			fixture.rotate(ctx, fixture.source, []byte(password))
			recoverUntilReady(ctx, fixture.api, recoveryReconciler(fixture.api, pg.NewRoleFactory()), fixture.role)
			Eventually(func() bool {
				fixture.reconcile(ctx, fixture.database, fixture.api)
				return bytes.Equal(fixture.output(ctx, r4OutputName).Data["password"], []byte(password))
			}).WithContext(ctx).WithTimeout(5*time.Second).Should(BeTrue(), "independent connection refresh completes")
			current := recoveredDatabase(ctx, fixture.api, fixture.database)
			r4NotReady(current)
			Expect(current.Status.HasManagedSQL).To(BeTrue())
			Expect(current.Status.AppliedGeneration).NotTo(Equal(current.Generation))
			Expect(fixture.login(ctx, string(fixture.database.Spec.Name), fixture.Role, password)).To(BeTrue())
		},
		Entry("a named migration table", "table", NodeTimeout(40*time.Second)),
		Entry("another privilege grantee", "grantee", NodeTimeout(40*time.Second)),
		Entry("another default creator", "defaults", NodeTimeout(40*time.Second)),
	)
	It("does not reuse a former target receipt after endpoint change or descriptor recreation", func(ctx SpecContext) {
		before := fixture.bootstrap(ctx)
		other, err := Start(ctx, Options{Version: os.Getenv("EPCO_PG_VERSION"), RunID: postgres.RunID})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			cleanup, cancel := context.WithTimeout(context.Background(), 25*time.Second)
			defer cancel()
			Expect(other.Close(cleanup)).To(Succeed())
		})
		otherFixture, err := other.NewFixture(ctx)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			cleanup, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			Expect(otherFixture.Close(cleanup)).To(Succeed())
		})
		wrong, err := randomName("old-target-")
		Expect(err).NotTo(HaveOccurred())
		roleSQL(ctx, otherFixture, "CREATE ROLE "+quoteIdentifier(fixture.Role)+" LOGIN PASSWORD "+quoteLiteral(wrong))
		latest := &epco.ExternalPostgres{}
		Expect(fixture.api.Get(ctx, client.ObjectKeyFromObject(fixture.cluster), latest)).To(Succeed())
		latest.Spec.Connection.Port = int32(other.port)
		latest.Spec.Connection.Database = epco.SQLName(otherFixture.Database)
		Expect(fixture.api.Update(ctx, latest)).To(Succeed())
		for name, value := range map[string][]byte{recoveryAdminSecret: []byte(other.password), "ca": nil} {
			secret := &corev1.Secret{}
			Expect(fixture.api.Get(ctx,
				client.ObjectKey{Name: name, Namespace: fixture.database.Namespace}, secret)).To(Succeed())
			key := corev1.BasicAuthPasswordKey
			if name == "ca" {
				value, err = os.ReadFile(other.TLS.CA)
				Expect(err).NotTo(HaveOccurred())
				key = roleCAKey
			}
			secret.Data[key] = value
			Expect(fixture.api.Update(ctx, secret)).To(Succeed())
		}
		r4NotReady(fixture.reconcile(ctx, fixture.database, fixture.api))
		r4Unchanged(before, fixture.output(ctx, r4OutputName))
		recoverUntilReady(ctx, fixture.api, recoveryReconciler(fixture.api, pg.NewRoleFactory()), fixture.role)
		Eventually(func() bool {
			fixture.reconcile(ctx, fixture.database, fixture.api)
			return string(fixture.output(ctx, r4OutputName).Data["port"]) == strconv.Itoa(int(other.port))
		}).WithContext(ctx).WithTimeout(3*time.Second).Should(
			BeTrue(), "the output selects the SQL-confirmed current endpoint")
		recoverDatabaseReady(ctx, fixture.api,
			databaseRecoveryReconciler(fixture.api, pg.NewDatabaseFactory()), fixture.database)
		Expect(string(fixture.output(ctx, r4OutputName).Data["port"])).To(Equal(strconv.Itoa(int(other.port))))
		Expect(recoveryDatabaseExists(ctx, fixture.Server, string(fixture.database.Spec.Name))).To(BeTrue())
		before = fixture.output(ctx, r4OutputName)
		Expect(fixture.api.Get(ctx, client.ObjectKeyFromObject(fixture.cluster), latest)).To(Succeed())
		Expect(fixture.api.Delete(ctx, latest)).To(Succeed())
		recreated := &epco.ExternalPostgres{Name: latest.Name, Namespace: latest.Namespace, Spec: latest.Spec}
		Expect(fixture.api.Create(ctx, recreated)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) { Expect(fixture.api.Delete(ctx, recreated)).To(Succeed()) })
		fixture.editDatabase(ctx, func(spec *epco.DatabaseSpec) {
			spec.ConnectionSecrets[0].SSLRootCertPath = "/changed/ca.crt"
		})
		r4NotReady(fixture.reconcile(ctx, fixture.database, fixture.api))
		r4Unchanged(before, fixture.output(ctx, r4OutputName))
		recoverUntilReady(ctx, fixture.api, recoveryReconciler(fixture.api, pg.NewRoleFactory()), fixture.role)
		Eventually(func() bool {
			fixture.reconcile(ctx, fixture.database, fixture.api)
			return string(fixture.output(ctx, r4OutputName).Data["sslrootcert"]) == "/changed/ca.crt"
		}).WithContext(ctx).WithTimeout(3*time.Second).Should(BeTrue(), "the recreated descriptor gains its own SQL receipt")
		recoverDatabaseReady(ctx, fixture.api,
			databaseRecoveryReconciler(fixture.api, pg.NewDatabaseFactory()), fixture.database)
		Expect(string(fixture.output(ctx, r4OutputName).Data["sslrootcert"])).To(Equal("/changed/ca.crt"))
	}, NodeTimeout(120*time.Second))
	It("refreshes through admin catalogs when HBA denies operator-side application authentication", func(ctx SpecContext) {
		fixture.bootstrap(ctx)
		// Rewrite only this suite-owned disposable server's HBA and restore it
		// before cleanup. The administrator continues using password auth.
		query := "host all " + quoteIdentifier(fixture.Role) + " all reject\n"
		command := "cp \"$PGDATA/pg_hba.conf\" /tmp/r4-hba.backup; " +
			"{ cat; cat /tmp/r4-hba.backup; } > \"$PGDATA/pg_hba.conf\""
		Expect(r4DockerInput(ctx, fixture.Server, command, []byte(query))).To(BeTrue())
		DeferCleanup(func(ctx SpecContext) {
			Expect(r4DockerInput(ctx, fixture.Server, "cp /tmp/r4-hba.backup \"$PGDATA/pg_hba.conf\"", nil)).To(BeTrue())
			_, restoreErr := fixture.Server.Admin.ExecContext(ctx, "SELECT pg_reload_conf()")
			Expect(restoreErr).NotTo(HaveOccurred())
		})
		_, err := fixture.Server.Admin.ExecContext(ctx, "SELECT pg_reload_conf()")
		Expect(err).NotTo(HaveOccurred())
		Eventually(func() bool {
			return fixture.login(ctx, string(fixture.database.Spec.Name), fixture.Role, fixture.Password)
		}).WithContext(ctx).WithTimeout(5*time.Second).Should(
			BeFalse(), "HBA blocks the application from the operator network")
		password, err := randomName("catalog-only-")
		Expect(err).NotTo(HaveOccurred())
		fixture.rotate(ctx, fixture.source, []byte(password))
		recoverUntilReady(ctx, fixture.api, recoveryReconciler(fixture.api, pg.NewRoleFactory()), fixture.role)
		fixture.refreshed(ctx, fixture.database, r4OutputName, []byte(password), fixture.api)
		recoverDatabaseReady(ctx, fixture.api,
			databaseRecoveryReconciler(fixture.api, pg.NewDatabaseFactory()), fixture.database)
		r4Password(fixture.output(ctx, r4OutputName), []byte(password))
		Expect(fixture.login(ctx, string(fixture.database.Spec.Name), fixture.Role, password)).To(BeFalse())
	}, NodeTimeout(40*time.Second))
})

func r4DockerInput(ctx context.Context, server *Server, script string, input []byte) bool {
	command := exec.CommandContext(ctx, "docker", "exec", "-i", server.Container, "sh", "-ec", script)
	command.Stdin = strings.NewReader(string(input))
	return command.Run() == nil
}
