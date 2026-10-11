//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"maps"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	epco "github.com/vterdunov/external-postgres-control-operator/api/v1alpha1"
	"github.com/vterdunov/external-postgres-control-operator/internal/controller"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var _ = Describe("R6 Installed application scenarios", func() {
	It("runs same-wave owner migrations and non-owner CRUD with creation-time read-only defaults and sequence repair",
		func(ctx SpecContext) {
			cluster := r6Platform(ctx)
			app := r6NewApplication(ctx, cluster, r6ForwardPostgres(ctx))
			app.migrate(ctx)
			owner := string(app.roles[r6Owner].Spec.Name)
			runtimeRole := string(app.roles[r6Runtime].Spec.Name)
			migrator := app.output(ctx, r6Migrator)
			runtimeOutput, reader := app.output(ctx, r6Runtime), app.output(ctx, r6Reader)
			Expect(app.query(ctx, runtimeOutput, "SELECT session_user,current_user,current_database()", "")).
				To(Equal(runtimeRole + "|" + runtimeRole + "|" + string(app.database.Spec.Name)))
			Expect(installedDatabaseSQL(ctx, "postgres", "SELECT rolcanlogin,rolsuper FROM pg_roles WHERE rolname='"+
				owner+"'")).To(Equal("f|f"))
			Expect(installedDatabaseSQL(ctx, "postgres", "SELECT pg_get_userbyid(datdba) FROM pg_database WHERE datname='"+
				string(app.database.Spec.Name)+"'")).To(Equal(owner))
			Expect(installedDatabaseSQL(ctx, string(app.database.Spec.Name),
				"SELECT pg_get_userbyid(nspowner) FROM pg_namespace WHERE nspname='app'")).To(Equal(owner))
			Expect(app.query(ctx, migrator, "SELECT session_user,current_user,pg_has_role(current_user,'"+
				owner+"','MEMBER')", "")).To(Equal(string(app.roles[r6Migrator].Spec.Name) + "|" +
				string(app.roles[r6Migrator].Spec.Name) + "|true"))
			Expect(installedDatabaseSQL(ctx, string(app.database.Spec.Name),
				"SELECT pg_get_userbyid(relowner) FROM pg_class "+
					"WHERE oid IN ('app.orders'::regclass,'app.orders_id_seq'::regclass) ORDER BY relname")).
				To(Equal(owner + "\n" + owner))
			Expect(app.query(ctx, runtimeOutput,
				"SELECT pg_has_role(current_user,'"+owner+"','MEMBER'), "+
					"has_table_privilege(current_user,'app.orders','INSERT'), "+
					"has_sequence_privilege(current_user,'app.orders_id_seq','UPDATE'), "+
					"(SELECT rolsuper FROM pg_roles WHERE rolname=current_user)", "")).To(Equal("false|true|false|false"))
			app.query(ctx, runtimeOutput, "SET ROLE "+owner, "42501")
			app.query(ctx, runtimeOutput, "CREATE TABLE app.denied(value integer)", "42501")
			Expect(app.query(ctx, runtimeOutput, "INSERT INTO app.orders(value) VALUES(43) RETURNING id", "")).To(Equal("2"))
			Expect(app.query(ctx, runtimeOutput,
				"UPDATE app.orders SET value=44 WHERE value=43 RETURNING value", "")).To(Equal("44"))
			Expect(app.query(ctx, runtimeOutput, "SELECT value FROM app.orders ORDER BY id", "")).To(Equal("42\n44"))
			Expect(app.query(ctx, runtimeOutput, "DELETE FROM app.orders WHERE value=44 RETURNING value", "")).To(Equal("44"))
			for _, sql := range []string{"INSERT INTO app.orders(value) VALUES(99)",
				"UPDATE app.orders SET value=99", "DELETE FROM app.orders", "CREATE TABLE app.denied(value integer)"} {
				app.query(ctx, reader, sql, "42501")
			}
			Expect(app.query(ctx, reader, "SELECT id,value FROM app.orders", "")).To(Equal("1|42"))
			second := app.migration(ctx, "second-migration", "SET ROLE "+owner+
				"; CREATE TABLE app.future(id SERIAL PRIMARY KEY,value integer); "+
				"INSERT INTO app.future(value) VALUES(23);", false)
			r6MigrationComplete(ctx, second)
			// No broad/table SELECT for this login can repair a missing default.
			Expect(app.query(ctx, reader, "SELECT value FROM app.future", "")).To(Equal("23"))
			Expect(app.query(ctx, runtimeOutput, "INSERT INTO app.future(value) VALUES(24) RETURNING id", "")).To(Equal("2"))
			Expect(apiClient.Get(ctx, client.ObjectKeyFromObject(app.database), app.database)).To(Succeed())
			before := app.database.DeepCopy()
			app.database.Spec.Schemas[0].DefaultPrivileges[0].Sequences = &epco.SequencePermissions{
				Usage: []epco.Permission{{Name: app.roles[r6Runtime].Spec.Name, Ensure: epco.EnsureAbsent}}}
			app.database.Spec.Schemas[0].Permissions.Sequences = &epco.SequencePermissions{
				Usage: []epco.Permission{{Name: app.roles[r6Runtime].Spec.Name, Ensure: epco.EnsureAbsent}}}
			Expect(apiClient.Patch(ctx, app.database, client.MergeFrom(before))).To(Succeed())
			app.ready(ctx)
			Expect(app.query(ctx, runtimeOutput,
				"SELECT has_table_privilege(current_user,'app.orders','INSERT'), "+
					"has_sequence_privilege(current_user,'app.orders_id_seq','USAGE'), "+
					"has_sequence_privilege(current_user,'app.orders_id_seq','UPDATE')", "")).To(Equal("true|false|false"))
			app.query(ctx, runtimeOutput, "INSERT INTO app.orders(value) VALUES(90)", "42501")
			Expect(app.query(ctx, reader, "SELECT id,value FROM app.orders", "")).To(Equal("1|42"))
			before = app.database.DeepCopy()
			app.database.Spec.Schemas[0].Permissions.Sequences.Usage = r6Permissions(app.roles[r6Runtime].Spec.Name)
			app.database.Spec.Schemas[0].DefaultPrivileges[0].Sequences.Usage =
				r6Permissions(app.roles[r6Runtime].Spec.Name)
			Expect(apiClient.Patch(ctx, app.database, client.MergeFrom(before))).To(Succeed())
			app.ready(ctx)
			Expect(app.query(ctx, runtimeOutput, "INSERT INTO app.orders(value) VALUES(91) RETURNING id", "")).To(Equal("3"))
		}, NodeTimeout(7*time.Minute))

	It("limits a named-table login across existing and later objects and removes access without deleting data",
		func(ctx SpecContext) {
			app := r6NewApplication(ctx, r6Platform(ctx), r6ForwardPostgres(ctx))
			app.migrate(ctx)
			named := app.output(ctx, r6Named)
			Expect(app.query(ctx, named, "SELECT value FROM app.orders", "")).To(Equal("42"))
			Expect(app.query(ctx, named, "SELECT value FROM app.invoices", "")).To(Equal("7"))
			for _, sql := range []string{"SELECT * FROM app.hidden", "INSERT INTO app.orders(value) VALUES(99)",
				"UPDATE app.orders SET value=99", "DELETE FROM app.orders", "CREATE TABLE app.denied(value integer)"} {
				app.query(ctx, named, sql, "42501")
			}
			job := app.migration(ctx, "new-hidden-table", "SET ROLE "+string(app.roles[r6Owner].Spec.Name)+
				"; CREATE TABLE app.future(value integer); INSERT INTO app.future VALUES(23);", false)
			r6MigrationComplete(ctx, job)
			app.query(ctx, named, "SELECT * FROM app.future", "42501")
			Expect(apiClient.Get(ctx, client.ObjectKeyFromObject(app.database), app.database)).To(Succeed())
			before := app.database.DeepCopy()
			app.database.Spec.Schemas[0].TablePermissions = app.database.Spec.Schemas[0].TablePermissions[1:]
			Expect(apiClient.Patch(ctx, app.database, client.MergeFrom(before))).To(Succeed())
			app.ready(ctx)
			app.query(ctx, named, "SELECT * FROM app.orders", "42501")
			Expect(app.query(ctx, named, "SELECT value FROM app.invoices", "")).To(Equal("7"))
			Expect(app.query(ctx, app.output(ctx, r6Reader), "SELECT value FROM app.orders", "")).To(Equal("42"))
		}, NodeTimeout(6*time.Minute))

	It("isolates equal Kubernetes names and pauses latest-input work and a different pending finalizer in one namespace",
		func(ctx SpecContext) {
			cluster, port := r6Platform(ctx), r6ForwardPostgres(ctx)
			first, other := r6NewApplication(ctx, cluster, port), r6NewApplication(ctx, cluster, port)
			first.migrate(ctx)
			other.migrate(ctx)
			firstRuntime, otherRuntime := first.output(ctx, r6Runtime), other.output(ctx, r6Runtime)
			Expect(first.database.Name).To(Equal(other.database.Name))
			Expect(first.roles[r6Runtime].Name).To(Equal(other.roles[r6Runtime].Name))
			Expect(firstRuntime.Name).To(Equal(otherRuntime.Name))
			Expect(first.database.Spec.Name).NotTo(Equal(other.database.Spec.Name))
			for _, pair := range []struct {
				app    *r6Application
				output *corev1.Secret
				target *r6Application
			}{{first, firstRuntime, other}, {other, otherRuntime, first}} {
				Expect(pair.app.query(ctx, pair.output, "INSERT INTO app.orders(value) VALUES(50) RETURNING id", "")).
					To(Equal("2"))
				denied := pair.output.DeepCopy()
				denied.Data[r6DatabaseKey] = []byte(pair.target.database.Spec.Name)
				pair.app.query(ctx, denied, "SELECT 1", "42501-connect")
			}
			Expect(first.database.Status.HasManagedSQL).To(BeTrue())
			duplicate := &epco.Database{Namespace: other.namespace.Name, Name: "conflicting-database",
				Spec: epco.DatabaseSpec{Cluster: first.database.Spec.Cluster, Name: first.database.Spec.Name,
					Owner: other.roles[r6Owner].Spec.Name, ReclaimPolicy: epco.ReclaimDelete,
					ConnectionSecrets: []epco.ConnectionSecret{{Name: "conflicting-output",
						RoleRef: epco.LocalReference{Name: r6Runtime}}}}}
			Expect(apiClient.Create(ctx, duplicate)).To(Succeed())
			DeferCleanup(func() { deleteObject(duplicate) })
			duplicate = installedDatabase(ctx, duplicate, metav1.ConditionFalse, "OwnershipConflict")
			Expect(duplicate.Status.HasManagedSQL).To(BeFalse())
			Expect(apierrors.IsNotFound(apiClient.Get(ctx,
				client.ObjectKey{Namespace: other.namespace.Name, Name: "conflicting-output"}, &corev1.Secret{}))).To(BeTrue())
			Expect(installedDatabaseSQL(ctx, "postgres", "SELECT pg_get_userbyid(datdba) FROM pg_database WHERE datname='"+
				string(first.database.Spec.Name)+"'")).To(Equal(string(first.roles[r6Owner].Spec.Name)))
			deleteObject(duplicate)
			Expect(first.query(ctx, firstRuntime, "SELECT count(*) FROM app.orders", "")).To(Equal("2"))
			// A separate, already managed database can be deleted while the main
			// application remains live and receives newer desired inputs.
			deleting := &epco.Database{Namespace: first.namespace.Name, Name: "pending-delete",
				Spec: epco.DatabaseSpec{Cluster: first.database.Spec.Cluster, Name: first.database.Spec.Name + "_delete",
					Owner: first.roles[r6Owner].Spec.Name, ReclaimPolicy: epco.ReclaimDelete}}
			Expect(apiClient.Create(ctx, deleting)).To(Succeed())
			DeferCleanup(func() {
				cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				r6NamespaceAccess(cleanup, first.namespace, true)
				deleteObject(deleting)
			})
			deleting = installedDatabase(ctx, deleting, metav1.ConditionTrue, "Ready")
			beforeSource := &corev1.Secret{}
			Expect(apiClient.Get(ctx,
				client.ObjectKey{Namespace: first.namespace.Name, Name: "runtime-source"}, beforeSource)).To(Succeed())
			beforeOutput := firstRuntime.DeepCopy()
			r6NamespaceAccess(ctx, first.namespace, false)
			first.database = installedDatabase(ctx, first.database, metav1.ConditionFalse, "AccessDenied")
			first.roles[r6Runtime] = installedRole(ctx, first.roles[r6Runtime], metav1.ConditionFalse, "AccessDenied")
			deleting = installedDatabase(ctx, deleting, metav1.ConditionFalse, "AccessDenied")
			beforeSource.Data[corev1.BasicAuthPasswordKey] = []byte("r6-requested-during-access-pause")
			Expect(apiClient.Update(ctx, beforeSource)).To(Succeed())
			beforeDatabase := first.database.DeepCopy()
			first.database.Spec.Schemas = append(first.database.Spec.Schemas, epco.SchemaSpec{Name: "latest"})
			Expect(apiClient.Patch(ctx, first.database, client.MergeFrom(beforeDatabase))).To(Succeed())
			Expect(apiClient.Delete(ctx, deleting)).To(Succeed())
			deleting = installedDatabase(ctx, deleting, metav1.ConditionFalse, "AccessDenied")
			Expect(deleting.DeletionTimestamp).NotTo(BeNil())
			Expect(deleting.Finalizers).To(ContainElement(controller.DatabaseFinalizer))
			Consistently(func() bool {
				current := &epco.Database{}
				return r6SecretUnchanged(ctx, beforeOutput) &&
					apiClient.Get(ctx, client.ObjectKeyFromObject(deleting), current) == nil &&
					current.DeletionTimestamp != nil &&
					strings.Join(current.Finalizers, ",") == strings.Join(deleting.Finalizers, ",") &&
					installedDatabaseSQL(ctx, string(first.database.Spec.Name),
						"SELECT to_regnamespace('latest') IS NULL") == "t" &&
					installedDatabaseSQL(ctx, "postgres", "SELECT count(*) FROM pg_database WHERE datname='"+
						string(deleting.Spec.Name)+"'") == "1"
			}).WithContext(ctx).WithTimeout(3 * time.Second).WithPolling(200 * time.Millisecond).Should(BeTrue())
			Expect(first.query(ctx, beforeOutput, "SELECT count(*) FROM app.orders", "")).To(Equal("2"))
			Expect(other.query(ctx, otherRuntime, "SELECT count(*) FROM app.orders", "")).To(Equal("2"))
			r6NamespaceAccess(ctx, first.namespace, true)
			firstRuntime = installedConnectionPassword(ctx, first.namespace.Name,
				"runtime-connection", beforeSource.Data[corev1.BasicAuthPasswordKey])
			first.ready(ctx)
			installedDatabaseRemoved(ctx, deleting)
			Expect(first.query(ctx, firstRuntime, "SELECT count(*) FROM app.orders", "")).To(Equal("2"))
			first.query(ctx, beforeOutput, "SELECT 1", "28P01")
			Expect(installedDatabaseSQL(ctx, string(first.database.Spec.Name),
				"SELECT to_regnamespace('latest') IS NOT NULL")).To(Equal("t"))
			Expect(other.query(ctx, otherRuntime, "SELECT count(*) FROM app.orders", "")).To(Equal("2"))
		}, NodeTimeout(9*time.Minute))

	It("preserves acknowledged outputs through an outage then rotates supplied and generated credentials "+
		"and reuses retained provenance",
		func(ctx SpecContext) {
			cluster, port := r6Platform(ctx), r6ForwardPostgres(ctx)
			app := r6NewApplication(ctx, cluster, port)
			app.migrate(ctx)
			service, err := kubeClient.CoreV1().Services("epco-postgres").Get(ctx, "postgres", metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			originalSelector := maps.Clone(service.Spec.Selector)
			DeferCleanup(func() {
				cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				installedConnectionServiceSelector(cleanup, originalSelector)
			})
			old := map[string]*corev1.Secret{r6Runtime: app.output(ctx, r6Runtime), r6Reader: app.output(ctx, r6Reader)}
			for _, name := range []string{r6Runtime, r6Reader} {
				app.passwordAuthenticationControl(ctx, old[name])
			}
			installedConnectionServiceSelector(ctx, map[string]string{"r6-outage": "unavailable"})
			r6ServiceDisconnected(ctx, app)
			newSources := map[string]*corev1.Secret{}
			for _, name := range []string{r6Runtime, r6Reader} {
				source := &corev1.Secret{}
				Expect(apiClient.Get(ctx,
					client.ObjectKey{Namespace: app.namespace.Name, Name: name + "-source"}, source)).To(Succeed())
				source.Data[corev1.BasicAuthPasswordKey] = []byte("r6-rotated-" + name + "-during-owned-outage")
				Expect(apiClient.Update(ctx, source)).To(Succeed())
				newSources[name] = source
			}
			app.database = installedDatabase(ctx, app.database, metav1.ConditionFalse, "")
			Consistently(func() bool {
				return r6SecretUnchanged(ctx, old[r6Runtime]) && r6SecretUnchanged(ctx, old[r6Reader])
			}).WithContext(ctx).WithTimeout(3*time.Second).Should(BeTrue(), "unapplied source may not replace output")
			for _, name := range []string{r6Runtime, r6Reader} {
				Expect(app.query(ctx, old[name], "SELECT value FROM app.orders", "")).To(Equal("42"))
				unapplied := old[name].DeepCopy()
				unapplied.Data[corev1.BasicAuthPasswordKey] = bytes.Clone(newSources[name].Data[corev1.BasicAuthPasswordKey])
				app.query(ctx, unapplied, "SELECT 1", "28P01")
			}
			installedConnectionServiceSelector(ctx, originalSelector)
			for _, name := range []string{r6Runtime, r6Reader} {
				output := installedConnectionPassword(ctx, app.namespace.Name,
					name+"-connection", newSources[name].Data[corev1.BasicAuthPasswordKey])
				Expect(app.query(ctx, output, "SELECT value FROM app.orders", "")).To(Equal("42"))
				app.query(ctx, old[name], "SELECT 1", "28P01")
			}
			app.ready(ctx)
			r6RetainedCredentials(ctx, app)
		}, NodeTimeout(8*time.Minute))

	It("checkpoints an omitted named grant behind an owned blocker and resumes cleanup after manager restart",
		func(ctx SpecContext) {
			cluster, port := r6Platform(ctx), r6ForwardPostgres(ctx)
			Expect(apiClient.Get(ctx, client.ObjectKeyFromObject(cluster), cluster)).To(Succeed())
			beforeCluster := cluster.DeepCopy()
			cluster.Spec.StatementTimeout = "60s"
			cluster.Spec.ReconcileInterval = "1h"
			Expect(apiClient.Patch(ctx, cluster, client.MergeFrom(beforeCluster))).To(Succeed())
			app := r6NewApplication(ctx, cluster, port)
			app.migrate(ctx)
			defer r6CaptureFailedRevoke(app)
			named := app.output(ctx, r6Named)
			password := bytes.Clone(named.Data[corev1.BasicAuthPasswordKey])
			objectOID := installedDatabaseSQL(ctx, string(app.database.Spec.Name),
				"SELECT c.oid::text FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace "+
					"WHERE n.nspname='app' AND c.relname='orders' AND c.relkind='r'")
			granteeOID := installedDatabaseSQL(ctx, "postgres",
				"SELECT oid::text FROM pg_roles WHERE rolname='"+string(app.roles[r6Named].Spec.Name)+"'")
			Expect(objectOID).NotTo(BeEmpty())
			Expect(granteeOID).NotTo(BeEmpty())
			app.quiescent(ctx, objectOID, granteeOID)
			blockerPID, stop := installedDatabaseBlocker(ctx, string(app.database.Spec.Name),
				"BEGIN; SELECT oid FROM pg_catalog.pg_class WHERE oid="+objectOID+
					"::oid FOR UPDATE; SELECT pg_sleep(120); COMMIT")
			Expect(apiClient.Get(ctx, client.ObjectKeyFromObject(app.database), app.database)).To(Succeed())
			before := app.database.DeepCopy()
			app.database.Spec.Schemas[0].TablePermissions = app.database.Spec.Schemas[0].TablePermissions[1:]
			Expect(apiClient.Patch(ctx, app.database, client.MergeFrom(before))).To(Succeed())
			omissionGeneration := app.database.Generation
			r6PendingNamedRevoke(ctx, app, objectOID, granteeOID, omissionGeneration)
			var backend string
			Eventually(func() bool {
				backend = installedDatabaseSQL(ctx, "postgres",
					"SELECT pid FROM pg_stat_activity WHERE datname='"+string(app.database.Spec.Name)+
						"' AND wait_event_type='Lock' AND query ILIKE '%REVOKE%' AND "+r6PIDSQL(blockerPID)+
						"=ANY(pg_blocking_pids(pid))")
				return backend != "" && !strings.Contains(backend, "\n")
			}).WithContext(ctx).WithTimeout(10*time.Second).Should(BeTrue(), "owned blocker must block checkpointed revoke")
			r6RestartManager(ctx)
			Expect(installedDatabaseSQL(ctx, "postgres",
				"SELECT count(*) FROM pg_stat_activity WHERE pid="+backend)).To(Equal("0"),
				"restart must close old backend before harness releases blocker")
			r6PendingNamedRevoke(ctx, app, objectOID, granteeOID, omissionGeneration)
			stop()
			app.ready(ctx)
			app.query(ctx, named, "SELECT * FROM app.orders", "42501")
			Expect(app.query(ctx, named, "SELECT value FROM app.invoices", "")).To(Equal("7"))
			Expect(app.query(ctx, app.output(ctx, r6Reader), "SELECT value FROM app.orders", "")).To(Equal("42"))
			Expect(bytes.Equal(app.output(ctx, r6Named).Data[corev1.BasicAuthPasswordKey], password)).To(BeTrue())
		}, NodeTimeout(7*time.Minute))
})
