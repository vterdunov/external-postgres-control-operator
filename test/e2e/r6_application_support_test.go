//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	epco "github.com/vterdunov/external-postgres-control-operator/api/v1alpha1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	r6Owner              = "owner"
	r6Migrator           = "migrator"
	r6Runtime            = "runtime"
	r6Reader             = "reader"
	r6Named              = "named"
	r6Public             = "public"
	r6Orders             = "orders"
	r6Invoices           = "invoices"
	r6ReaderSource       = "reader-source"
	r6ForeignResource    = "foreign-resource"
	r6MigrationContainer = "migration"
	r6ConnectionVolume   = "connection"
	r6GateReady          = "yes"
	r6DatabaseKey        = "dbname"
	r6ACLTable           = "table"
)

// Application SQL uses fresh authenticated TCP sessions. Only credential-free
// SQL results and SQLSTATE are asserted: pgx errors may carry connection data.
type r6Application struct {
	namespace *corev1.Namespace
	cluster   *epco.PostgresCluster
	database  *epco.Database
	roles     map[string]*epco.DatabaseRole
	port      uint16
}

func r6ForwardPostgres(ctx context.Context) uint16 {
	processCtx, cancel := context.WithCancel(ctx)
	DeferCleanup(cancel)
	log, err := os.CreateTemp(filepath.Dir(kubeconfigPath), "r6-postgres-forward-*.log")
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	DeferCleanup(func() {
		Expect(errors.Join(log.Close(), os.Remove(log.Name()))).To(Succeed())
	})
	cmd := kubectlCommand(processCtx, "-n", "epco-postgres", "port-forward", "--address=127.0.0.1",
		"service/postgres", "0:5432")
	cmd.Stdout, cmd.Stderr = log, log
	ExpectWithOffset(1, cmd.Start()).To(Succeed())
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	DeferCleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			Fail("application port-forward did not stop")
		}
	})
	var port int
	Eventually(func() bool {
		output, readErr := os.ReadFile(log.Name())
		if readErr != nil {
			return false
		}
		port = parseForwardedPorts(string(output))[5432]
		return port > 0 && port < 65536
	}).WithContext(ctx).WithTimeout(20 * time.Second).Should(BeTrue())
	return uint16(port)
}

func (app *r6Application) query(ctx context.Context, output *corev1.Secret, sql, expectedState string) string {
	ExpectWithOffset(1, output.Type).To(Equal(corev1.SecretTypeBasicAuth))
	config, err := pgx.ParseConfig("sslmode=disable")
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	config.Host, config.Port = "127.0.0.1", app.port
	config.User, config.Password = string(output.Data["user"]), string(output.Data[corev1.BasicAuthPasswordKey])
	config.Database = string(output.Data[r6DatabaseKey])
	config.ConnectTimeout = 5 * time.Second
	config.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	config.RuntimeParams["application_name"] = "epco-r6-application-probe"
	probe, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	connection, err := pgx.ConnectConfig(probe, config)
	if expectedState == "28P01" || expectedState == "42501-connect" {
		state := ""
		if postgresError, ok := errors.AsType[*pgconn.PgError](err); ok {
			state = postgresError.Code
		}
		if connection != nil {
			closeCtx, closeCancel := boundedContext()
			defer closeCancel()
			ExpectWithOffset(1, installedConnectionSucceeded(connection.Close(closeCtx))).To(BeTrue())
		}
		ExpectWithOffset(1, state).To(Equal(strings.TrimSuffix(expectedState, "-connect")),
			"fresh connection must fail at the specified PostgreSQL boundary (connected=%t, error type=%T)",
			installedConnectionSucceeded(err), err)
		return ""
	}
	ExpectWithOffset(1, installedConnectionSucceeded(err)).To(BeTrue(),
		"application authentication must succeed before SQL assertions")
	defer func() {
		closeCtx, closeCancel := boundedContext()
		defer closeCancel()
		ExpectWithOffset(1, installedConnectionSucceeded(connection.Close(closeCtx))).To(BeTrue())
	}()
	rows, err := connection.Query(probe, sql)
	state := ""
	var result []string
	if err == nil {
		for rows.Next() {
			values, valueErr := rows.Values()
			ExpectWithOffset(1, installedConnectionSucceeded(valueErr)).To(BeTrue(), "application result decode failed")
			fields := make([]string, len(values))
			for i, value := range values {
				fields[i] = fmt.Sprint(value)
			}
			result = append(result, strings.Join(fields, "|"))
		}
		err = rows.Err()
		rows.Close()
	}
	if postgresError, ok := errors.AsType[*pgconn.PgError](err); ok {
		state = postgresError.Code
	}
	if expectedState == "" {
		ExpectWithOffset(1, installedConnectionSucceeded(err)).To(BeTrue(), "application SQL failed")
	} else {
		ExpectWithOffset(1, state).To(Equal(expectedState), "denial must be the expected PostgreSQL error")
	}
	return strings.Join(result, "\n")
}

func r6Permissions(names ...epco.SQLName) []epco.Permission {
	permissions := make([]epco.Permission, 0, len(names))
	for _, name := range names {
		permissions = append(permissions, epco.Permission{Name: name})
	}
	return permissions
}

func r6Platform(ctx context.Context) *epco.PostgresCluster {
	name := "r6-" + strings.ToLower(rand.Text()[:8])
	cluster := installedRoleDescriptor(ctx, name, &epco.ClusterAccess{
		NamespaceSelector: &metav1.LabelSelector{
			MatchLabels: map[string]string{installedAccessLabel: installedAllowedLabel},
		},
	})
	ExpectWithOffset(1, apiClient.Get(ctx, client.ObjectKeyFromObject(cluster), cluster)).To(Succeed())
	before := cluster.DeepCopy()
	cluster.Spec.ReconcileInterval = "2s"
	ExpectWithOffset(1, apiClient.Patch(ctx, cluster, client.MergeFrom(before))).To(Succeed())
	return cluster
}

func r6NamespaceAccess(ctx context.Context, namespace *corev1.Namespace, allowed bool) {
	ExpectWithOffset(1, retry.RetryOnConflict(retry.DefaultRetry, func() error {
		current := &corev1.Namespace{}
		if err := apiClient.Get(ctx, client.ObjectKeyFromObject(namespace), current); err != nil {
			return client.IgnoreNotFound(err)
		}
		if allowed {
			current.Labels[installedAccessLabel] = installedAllowedLabel
		} else {
			delete(current.Labels, installedAccessLabel)
		}
		return apiClient.Update(ctx, current)
	})).To(Succeed())
}

func r6NewApplication(ctx context.Context, cluster *epco.PostgresCluster, port uint16) *r6Application {
	app := &r6Application{cluster: cluster, port: port, roles: map[string]*epco.DatabaseRole{},
		namespace: &corev1.Namespace{GenerateName: "epco-r6-",
			Labels: map[string]string{installedAccessLabel: installedAllowedLabel}}}
	ExpectWithOffset(1, apiClient.Create(ctx, app.namespace)).To(Succeed())
	prefix := "r6_" + strings.ReplaceAll(app.namespace.Name, "-", "_")
	var suppliedSource *corev1.Secret
	for _, name := range []string{r6Owner, r6Migrator, r6Runtime, r6Reader, r6Named} {
		role := &epco.DatabaseRole{Namespace: app.namespace.Name, Name: name,
			Spec: epco.DatabaseRoleSpec{Cluster: epco.ClusterReference{Namespace: cluster.Namespace, Name: cluster.Name},
				Name: epco.SQLName(prefix + "_" + name), Login: name != r6Owner, ReclaimPolicy: epco.ReclaimDelete}}
		if name == r6Runtime {
			source := &corev1.Secret{Namespace: app.namespace.Name, Name: name + "-source",
				Type: corev1.SecretTypeBasicAuth,
				Data: map[string][]byte{corev1.BasicAuthUsernameKey: []byte(role.Spec.Name),
					corev1.BasicAuthPasswordKey: []byte(rand.Text())}}
			suppliedSource = source
			role.Spec.PasswordSecret = &epco.LocalReference{Name: source.Name}
		} else if role.Spec.Login {
			role.Spec.PasswordGeneration = &epco.PasswordGeneration{SecretName: name + "-source"}
		}
		if name == r6Migrator {
			role.Spec.InRoles = []epco.SQLName{app.roles[r6Owner].Spec.Name}
		}
		app.roles[name] = role
	}
	owner, runtimeRole := app.roles[r6Owner].Spec.Name, app.roles[r6Runtime].Spec.Name
	reader, named := app.roles[r6Reader].Spec.Name, app.roles[r6Named].Spec.Name
	app.database = &epco.Database{Namespace: app.namespace.Name, Name: "application",
		Spec: epco.DatabaseSpec{Name: epco.SQLName(prefix + "_db"), Owner: owner, Cluster: app.roles[r6Owner].Spec.Cluster,
			ReclaimPolicy: epco.ReclaimDelete,
			Permissions: &epco.DatabasePermissions{Connect: append(
				r6Permissions(app.roles[r6Migrator].Spec.Name, runtimeRole, reader, named),
				epco.Permission{Name: r6Public, Ensure: epco.EnsureAbsent})},
			Schemas: []epco.SchemaSpec{{Name: installedDatabaseApplicationSchema, Owner: owner,
				Permissions: &epco.SchemaPermissions{Usage: r6Permissions(runtimeRole, reader, named)},
				DefaultPrivileges: []epco.DefaultPrivileges{{ForRole: owner,
					Tables: &epco.TablePermissions{Select: r6Permissions(runtimeRole, reader),
						Insert: r6Permissions(runtimeRole), Update: r6Permissions(runtimeRole), Delete: r6Permissions(runtimeRole)},
					Sequences: &epco.SequencePermissions{Usage: r6Permissions(runtimeRole)}}},
				TablePermissions: []epco.NamedTablePermissions{
					{Name: r6Orders, Permissions: epco.TablePermissions{Select: r6Permissions(named)}},
					{Name: r6Invoices, Permissions: epco.TablePermissions{Select: r6Permissions(named)}},
				}}, {Name: r6Public, Permissions: &epco.SchemaPermissions{
				Usage:  []epco.Permission{{Name: r6Public, Ensure: epco.EnsureAbsent}},
				Create: []epco.Permission{{Name: r6Public, Ensure: epco.EnsureAbsent}},
			}}},
		}}
	for _, name := range []string{r6Migrator, r6Runtime, r6Reader, r6Named} {
		app.database.Spec.ConnectionSecrets = append(app.database.Spec.ConnectionSecrets,
			epco.ConnectionSecret{Name: name + "-connection", RoleRef: epco.LocalReference{Name: name}})
	}
	// One cleanup owns the complete application. Authorization is restored first;
	// database deletion removes role dependencies, then role reclaim may proceed.
	DeferCleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		r6NamespaceAccess(cleanup, app.namespace, true)
		jobs, err := kubeClient.BatchV1().Jobs(app.namespace.Name).List(cleanup, metav1.ListOptions{})
		Expect(err).NotTo(HaveOccurred())
		for _, job := range jobs.Items {
			Expect(kubeClient.BatchV1().Jobs(job.Namespace).Delete(cleanup, job.Name,
				metav1.DeleteOptions{PropagationPolicy: new(metav1.DeletePropagationForeground)})).To(Succeed())
		}
		currentDatabase := &epco.Database{}
		if apiClient.Get(cleanup, client.ObjectKeyFromObject(app.database), currentDatabase) == nil {
			if currentDatabase.Spec.ReclaimPolicy != epco.ReclaimDelete {
				before := currentDatabase.DeepCopy()
				currentDatabase.Spec.ReclaimPolicy = epco.ReclaimDelete
				Expect(apiClient.Patch(cleanup, currentDatabase, client.MergeFrom(before))).To(Succeed())
			}
			deleteObject(currentDatabase)
		}
		for _, name := range []string{r6Migrator, r6Runtime, r6Reader, r6Named, r6Owner} {
			role := &epco.DatabaseRole{}
			if apiClient.Get(cleanup, client.ObjectKeyFromObject(app.roles[name]), role) == nil {
				if role.Spec.ReclaimPolicy != epco.ReclaimDelete {
					before := role.DeepCopy()
					role.Spec.ReclaimPolicy = epco.ReclaimDelete
					Expect(apiClient.Patch(cleanup, role, client.MergeFrom(before))).To(Succeed())
				}
				deleteObject(role)
			}
		}
		deleteObject(app.namespace)
	})
	ExpectWithOffset(1, apiClient.Create(ctx, suppliedSource)).To(Succeed())
	for _, name := range []string{r6Owner, r6Migrator, r6Runtime, r6Reader, r6Named} {
		ExpectWithOffset(1, apiClient.Create(ctx, app.roles[name])).To(Succeed())
	}
	ExpectWithOffset(1, apiClient.Create(ctx, app.database)).To(Succeed())
	return app
}

func (app *r6Application) output(ctx context.Context, name string) *corev1.Secret {
	output := installedConnectionOutput(ctx, app.namespace.Name, name+"-connection")
	ExpectWithOffset(1, output.Namespace).To(Equal(app.namespace.Name))
	ExpectWithOffset(1, string(output.Data[r6DatabaseKey])).To(Equal(string(app.database.Spec.Name)))
	ExpectWithOffset(1, string(output.Data["user"])).To(Equal(string(app.roles[name].Spec.Name)))
	return output
}

// The migration process consumes only a namespace-local read-only Secret
// volume. Passwords stay in files/env; psql argv contains no URI or password.
func (app *r6Application) migration(ctx context.Context, name, ddl string, gated bool) *batchv1.Job {
	gateName := name + "-gate"
	gate := &corev1.ConfigMap{Namespace: app.namespace.Name, Name: gateName, Data: map[string]string{}}
	if !gated {
		gate.Data["ready"] = r6GateReady
	}
	ExpectWithOffset(1, apiClient.Create(ctx, gate)).To(Succeed())
	const connectScript = `export PGHOST="$(cat /connection/host)" PGPORT="$(cat /connection/port)"
export PGUSER="$(cat /connection/user)" PGDATABASE="$(cat /connection/dbname)"
export PGPASSWORD="$(cat /connection/password)" PGSSLMODE="$(cat /connection/sslmode)" PGCONNECT_TIMEOUT=2
attempt=0
until psql -X -v ON_ERROR_STOP=1 -Atc 'SELECT 1' >/dev/null 2>&1; do
  attempt=$((attempt + 1))
  [ "$attempt" -lt 100 ] || { echo 'Application connection retry limit reached'; exit 1; }
  echo 'Waiting for application connection'
  sleep 1
done
echo 'Application connection established'
until [ -f /gate/ready ]; do sleep 1; done
psql -X -v ON_ERROR_STOP=1 -v VERBOSITY=sqlstate -At >/dev/null 2>&1 <<'EPCO_R6_SQL'
`
	image := installedClientImage()
	job := &batchv1.Job{Namespace: app.namespace.Name, Name: name,
		Spec: batchv1.JobSpec{BackoffLimit: new(int32(0)), ActiveDeadlineSeconds: new(int64(180)),
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{RestartPolicy: corev1.RestartPolicyNever,
				AutomountServiceAccountToken: new(false),
				Containers: []corev1.Container{{Name: r6MigrationContainer, Image: image,
					Command: []string{"sh", "-ceu", connectScript + ddl + "\nEPCO_R6_SQL\necho 'Migration completed'\n"},
					VolumeMounts: []corev1.VolumeMount{{Name: r6ConnectionVolume, MountPath: "/connection", ReadOnly: true},
						{Name: "gate", MountPath: "/gate", ReadOnly: true}}}},
				Volumes: []corev1.Volume{{Name: r6ConnectionVolume, Secret: &corev1.SecretVolumeSource{
					SecretName: "migrator-connection", DefaultMode: new(int32(0400))}},
					{Name: "gate", ConfigMap: &corev1.ConfigMapVolumeSource{Name: gate.Name}}},
			}},
		}}
	job, err := kubeClient.BatchV1().Jobs(job.Namespace).Create(ctx, job, metav1.CreateOptions{})
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	return job
}

func r6MigrationComplete(ctx context.Context, job *batchv1.Job) {
	Eventually(func() bool {
		current, err := kubeClient.BatchV1().Jobs(job.Namespace).Get(ctx, job.Name, metav1.GetOptions{})
		return err == nil && current.Status.Succeeded == 1
	}).WithContext(ctx).WithTimeout(90*time.Second).Should(BeTrue(), "application migration Job must succeed")
	ExpectWithOffset(1, installedConnectionJobLogs(ctx, job)).To(ContainSubstring("Migration completed"))
}

func (app *r6Application) ready(ctx context.Context) {
	app.database = installedDatabase(ctx, app.database, metav1.ConditionTrue, "Ready")
	for _, name := range []string{r6Owner, r6Migrator, r6Runtime, r6Reader, r6Named} {
		app.roles[name] = installedRole(ctx, app.roles[name], metav1.ConditionTrue, "Ready")
	}
}

func (app *r6Application) migrate(ctx context.Context) {
	owner, migrator := string(app.roles[r6Owner].Spec.Name), string(app.roles[r6Migrator].Spec.Name)
	ddl := "DO $identity$ BEGIN IF session_user <> '" + migrator +
		"' THEN RAISE EXCEPTION 'unexpected migrator'; END IF; END $identity$;\nSET ROLE " + owner + ";\n" +
		"DO $identity$ BEGIN IF current_user <> '" + owner +
		"' THEN RAISE EXCEPTION 'unexpected creator'; END IF; END $identity$;\n" +
		"CREATE TABLE app.orders(id SERIAL PRIMARY KEY, value integer);\n" +
		"INSERT INTO app.orders(value) VALUES(42);\n" +
		"CREATE TABLE app.invoices(value integer); INSERT INTO app.invoices VALUES(7);\n" +
		"CREATE TABLE app.hidden(value integer); INSERT INTO app.hidden VALUES(9);"
	job := app.migration(ctx, "first-migration", ddl, true)
	app.output(ctx, r6Migrator)
	for _, name := range []string{r6Owner, r6Migrator, r6Runtime, r6Reader, r6Named} {
		app.roles[name] = installedRole(ctx, app.roles[name], metav1.ConditionTrue, "Ready")
	}
	Eventually(func() bool {
		return strings.Contains(installedConnectionJobLogs(ctx, job), "Application connection established")
	}).WithContext(ctx).WithTimeout(60 * time.Second).Should(BeTrue())
	// Default privileges must exist before DDL. No broad current-table grant
	// exists for reader/runtime, so reconciliation cannot mask missing defaults.
	Eventually(func() bool {
		current := &epco.Database{}
		if apiClient.Get(ctx, client.ObjectKeyFromObject(app.database), current) != nil {
			return false
		}
		ready := meta.FindStatusCondition(current.Status.Conditions, "Ready")
		if ready == nil || ready.Status != metav1.ConditionFalse || !current.Status.HasManagedSQL {
			return false
		}
		if installedDatabaseSQL(ctx, string(app.database.Spec.Name),
			"SELECT to_regnamespace('app') IS NOT NULL") != "t" {
			return false
		}
		prerequisites := installedDatabaseSQL(ctx, string(app.database.Spec.Name),
			"SELECT has_database_privilege('"+string(app.roles[r6Migrator].Spec.Name)+
				"',current_database(),'CONNECT') AND has_schema_privilege('"+owner+
				"','app','CREATE') AND has_schema_privilege('"+string(app.roles[r6Reader].Spec.Name)+
				"','app','USAGE') AND has_schema_privilege('"+string(app.roles[r6Runtime].Spec.Name)+
				"','app','USAGE') AND pg_has_role('"+string(app.roles[r6Migrator].Spec.Name)+
				"','"+owner+"','MEMBER')")
		return prerequisites == "t" && installedDatabaseSQL(ctx, string(app.database.Spec.Name),
			"SELECT count(*) FROM pg_default_acl d CROSS JOIN LATERAL aclexplode(d.defaclacl) a "+
				"WHERE d.defaclrole='"+owner+"'::regrole AND d.defaclnamespace='app'::regnamespace "+
				"AND ((d.defaclobjtype='r' AND a.grantee IN ('"+string(app.roles[r6Runtime].Spec.Name)+
				"'::regrole,'"+string(app.roles[r6Reader].Spec.Name)+"'::regrole)) OR "+
				"(d.defaclobjtype='S' AND a.grantee='"+string(app.roles[r6Runtime].Spec.Name)+
				"'::regrole AND a.privilege_type='USAGE'))") == "6"
	}).WithContext(ctx).WithTimeout(40*time.Second).Should(BeTrue(), "actual-creator defaults precede gated DDL")
	ExpectWithOffset(1, installedDatabaseSQL(ctx, string(app.database.Spec.Name),
		"SELECT to_regclass('app.orders') IS NULL")).To(Equal("t"))
	gate := &corev1.ConfigMap{}
	ExpectWithOffset(1, apiClient.Get(ctx,
		client.ObjectKey{Namespace: app.namespace.Name, Name: job.Name + "-gate"}, gate)).To(Succeed())
	gate.Data = map[string]string{"ready": r6GateReady}
	ExpectWithOffset(1, apiClient.Update(ctx, gate)).To(Succeed())
	r6RefreshMigrationGate(ctx, job, gate.ResourceVersion)
	r6MigrationComplete(ctx, job)
	ExpectWithOffset(1, app.query(ctx, app.output(ctx, r6Reader), "SELECT value FROM app.orders", "")).To(Equal("42"))
	app.ready(ctx)
}

func r6SecretUnchanged(ctx context.Context, before *corev1.Secret) bool {
	current := &corev1.Secret{}
	return apiClient.Get(ctx, client.ObjectKeyFromObject(before), current) == nil && current.UID == before.UID &&
		maps.Equal(current.Annotations, before.Annotations) &&
		reflect.DeepEqual(current.OwnerReferences, before.OwnerReferences) &&
		maps.EqualFunc(current.Data, before.Data, bytes.Equal)
}

func r6PIDSQL(pid int) string { return strconv.Itoa(pid) }
