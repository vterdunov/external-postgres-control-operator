//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"maps"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	epco "github.com/vterdunov/external-postgres-control-operator/api/v1alpha1"
	"github.com/vterdunov/external-postgres-control-operator/test/utils"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	installedConnectionMigrationGate      = "migration-gate"
	installedConnectionDatabase           = "epco_e2e_connections"
	installedConnectionService            = "epco_e2e_connections_service"
	installedConnectionAdmin              = "epco_e2e_connections_reader"
	installedConnectionClientImage        = "public.ecr.aws/docker/library/postgres:16.15-bookworm"
	installedConnectionServiceOutput      = "service-connection"
	installedConnectionAdminOutput        = "reader-connection"
	installedConnectionRetryMessage       = "Waiting for application connection"
	installedConnectionEstablishedMessage = "Application connection established"
)

// Both the Job and fresh probes consume the output URI. Credentials enter the
// local kubectl process only through stdin; assertions never print the URI.
func installedConnectionSucceeded(err error) bool { return err == nil }

func installedConnectionLogin(ctx context.Context, output *corev1.Secret, query string, allowed bool) string {
	const script = "IFS= read -r connection; " +
		"exec psql -X -v ON_ERROR_STOP=1 -v VERBOSITY=sqlstate \"$connection\" -Atc \"$1\""
	cmd := kubectlCommand(ctx, "-n", "epco-postgres", "exec", "-i", "deployment/postgres", "--",
		"sh", "-c", script, "sh", query)
	cmd.Stdin = strings.NewReader(string(output.Data["uri"]) + "\n")
	result, err := utils.Run(cmd)
	if allowed {
		ExpectWithOffset(1, installedConnectionSucceeded(err)).To(BeTrue(),
			"fresh application connection or SQL query failed")
	} else {
		ExpectWithOffset(1, installedConnectionSucceeded(err)).To(BeFalse(), "read-only application unexpectedly mutated SQL")
		ExpectWithOffset(1, strings.Contains(result, "42501")).To(BeTrue(), "denial must come from PostgreSQL permissions")
	}
	return strings.TrimSpace(result)
}

func installedConnectionOutput(ctx context.Context, namespace, name string) *corev1.Secret {
	var output *corev1.Secret
	Eventually(func() bool {
		output = &corev1.Secret{}
		return apiClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, output) == nil
	}).WithContext(ctx).WithTimeout(40*time.Second).WithPolling(100*time.Millisecond).
		Should(BeTrue(), "initial application connection Secret must be published before SQL readiness")
	ExpectWithOffset(1, output.Type).To(Equal(corev1.SecretTypeBasicAuth))
	ExpectWithOffset(1, output.OwnerReferences).To(BeEmpty())
	return output
}

func installedConnectionPassword(ctx context.Context, namespace, name string, expected []byte) *corev1.Secret {
	var output *corev1.Secret
	Eventually(func() bool {
		output = &corev1.Secret{}
		err := apiClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, output)
		return err == nil && bytes.Equal(output.Data["password"], expected)
	}).WithContext(ctx).WithTimeout(40*time.Second).WithPolling(100*time.Millisecond).
		Should(BeTrue(), "output must converge to the SQL-acknowledged current source")
	return output
}

func installedConnectionJob(namespace, output string) *batchv1.Job {
	const script = `attempt=0
until psql -X -v ON_ERROR_STOP=1 "$DATABASE_URL" -Atc 'SELECT 1' >/dev/null 2>&1; do
  attempt=$((attempt + 1))
  if [ "$attempt" -ge 150 ]; then
    echo "Application connection retry limit reached"
    exit 1
  fi
  echo "Waiting for application connection"
  sleep 1
done
echo "Application connection established"
until [ -f /migration-gate/ready ]; do sleep 1; done
psql -X -v ON_ERROR_STOP=1 "$DATABASE_URL" -Atc \
  'CREATE TABLE app.orders(value integer); INSERT INTO app.orders VALUES (42)' >/dev/null 2>&1
echo "Migration completed"
`
	return &batchv1.Job{Namespace: namespace, Name: "application-migration",
		Spec: batchv1.JobSpec{BackoffLimit: new(int32(0)), ActiveDeadlineSeconds: new(int64(240)),
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{RestartPolicy: corev1.RestartPolicyNever,
				AutomountServiceAccountToken: new(false),
				Containers: []corev1.Container{{Name: "migration", Image: installedConnectionClientImage,
					Command: []string{"sh", "-ceu", script},
					Env: []corev1.EnvVar{{Name: "PGCONNECT_TIMEOUT", Value: "1"},
						{Name: "DATABASE_URL", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
							Name: output, Key: "uri"}}}},
					VolumeMounts: []corev1.VolumeMount{{Name: installedConnectionMigrationGate,
						MountPath: "/migration-gate", ReadOnly: true}},
				}},
				Volumes: []corev1.Volume{{Name: installedConnectionMigrationGate,
					ConfigMap: &corev1.ConfigMapVolumeSource{
						Name: installedConnectionMigrationGate,
					}}},
			}},
		}}
}

func installedConnectionJobLogs(ctx context.Context, job *batchv1.Job) string {
	pods, err := kubeClient.CoreV1().Pods(job.Namespace).List(
		ctx, metav1.ListOptions{LabelSelector: "job-name=" + job.Name})
	if err != nil || len(pods.Items) != 1 {
		return ""
	}
	logs, err := kubeClient.CoreV1().Pods(job.Namespace).GetLogs(
		pods.Items[0].Name, &corev1.PodLogOptions{Container: "migration"}).DoRaw(ctx)
	if err != nil {
		return ""
	}
	return string(logs)
}

func installedConnectionServiceSelector(ctx context.Context, selector map[string]string) {
	ExpectWithOffset(1, retry.RetryOnConflict(retry.DefaultRetry, func() error {
		service, err := kubeClient.CoreV1().Services("epco-postgres").Get(ctx, "postgres", metav1.GetOptions{})
		if err != nil {
			return err
		}
		service.Spec.Selector = maps.Clone(selector)
		_, err = kubeClient.CoreV1().Services("epco-postgres").Update(ctx, service, metav1.UpdateOptions{})
		return err
	})).To(Succeed())
}

var _ = Describe("R4 Installed application connection Secrets", func() {
	It("runs a retrying same-wave migration before full Database Ready, "+
		"then rotates and recovers namespace-local outputs", func(ctx SpecContext) {
		appNamespace := &corev1.Namespace{GenerateName: "epco-connections-",
			Labels: map[string]string{installedAccessLabel: installedAllowedLabel}}
		Expect(apiClient.Create(ctx, appNamespace)).To(Succeed())
		DeferCleanup(func() { deleteObject(appNamespace) })
		cluster := installedRoleDescriptor(ctx, "application-connections", &epco.ClusterAccess{
			NamespaceSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{installedAccessLabel: installedAllowedLabel},
			}})
		Expect(apiClient.Get(ctx, client.ObjectKeyFromObject(cluster), cluster)).To(Succeed())
		cluster.Spec.ReconcileInterval = "2s"
		Expect(apiClient.Update(ctx, cluster)).To(Succeed())

		service, err := kubeClient.CoreV1().Services("epco-postgres").Get(ctx, "postgres", metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		originalSelector := maps.Clone(service.Spec.Selector)
		installedConnectionServiceSelector(ctx, map[string]string{"epco-r4-fixture": "intentionally-unavailable"})

		serviceSource := &corev1.Secret{Namespace: appNamespace.Name, Name: "service-source",
			Type: corev1.SecretTypeBasicAuth,
			Data: map[string][]byte{"username": []byte(installedConnectionService),
				"password": []byte("first-installed-application-password")}}
		serviceRole := &epco.DatabaseRole{Namespace: appNamespace.Name, Name: "service-role-resource",
			Spec: epco.DatabaseRoleSpec{Cluster: epco.ClusterReference{Namespace: cluster.Namespace, Name: cluster.Name},
				Name: installedConnectionService, Login: true, ReclaimPolicy: epco.ReclaimDelete,
				PasswordSecret: &epco.LocalReference{Name: serviceSource.Name}}}
		readerRole := &epco.DatabaseRole{Namespace: appNamespace.Name, Name: "reader-role-resource",
			Spec: epco.DatabaseRoleSpec{Cluster: serviceRole.Spec.Cluster, Name: installedConnectionAdmin,
				Login: true, ReclaimPolicy: epco.ReclaimDelete,
				PasswordGeneration: &epco.PasswordGeneration{SecretName: "reader-source"}}}
		database := &epco.Database{Namespace: appNamespace.Name, Name: "application-database-resource",
			Spec: epco.DatabaseSpec{Cluster: serviceRole.Spec.Cluster, Name: installedConnectionDatabase,
				Owner: installedConnectionService, ReclaimPolicy: epco.ReclaimDelete,
				Permissions: &epco.DatabasePermissions{Connect: []epco.Permission{
					{Name: "public", Ensure: epco.EnsureAbsent}, {Name: installedConnectionService},
					{Name: installedConnectionAdmin}}},
				Schemas: []epco.SchemaSpec{{Name: "app",
					Permissions: &epco.SchemaPermissions{Usage: []epco.Permission{{Name: installedConnectionAdmin}}},
					DefaultPrivileges: []epco.DefaultPrivileges{{ForRole: installedConnectionService,
						Tables: &epco.TablePermissions{Select: []epco.Permission{{Name: installedConnectionAdmin}}}}},
					TablePermissions: []epco.NamedTablePermissions{{Name: "orders",
						Permissions: epco.TablePermissions{Select: []epco.Permission{{Name: installedConnectionAdmin}}}}}}},
				ConnectionSecrets: []epco.ConnectionSecret{
					{Name: installedConnectionServiceOutput, RoleRef: epco.LocalReference{Name: serviceRole.Name}},
					{Name: installedConnectionAdminOutput, RoleRef: epco.LocalReference{Name: readerRole.Name}}},
			}}
		gate := &corev1.ConfigMap{Namespace: appNamespace.Name, Name: installedConnectionMigrationGate,
			Data: map[string]string{}}
		job := installedConnectionJob(appNamespace.Name, installedConnectionServiceOutput)
		DeferCleanup(func() {
			cleanup, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			installedConnectionServiceSelector(cleanup, originalSelector)
			Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
				latest := &corev1.Namespace{}
				if err := apiClient.Get(cleanup, client.ObjectKeyFromObject(appNamespace), latest); err != nil {
					return client.IgnoreNotFound(err)
				}
				latest.Labels = map[string]string{installedAccessLabel: installedAllowedLabel}
				return apiClient.Update(cleanup, latest)
			})).To(Succeed())
			_ = kubeClient.BatchV1().Jobs(job.Namespace).Delete(cleanup, job.Name,
				metav1.DeleteOptions{PropagationPolicy: new(metav1.DeletePropagationForeground)})
			deleteObject(database)
			deleteObject(serviceRole)
			deleteObject(readerRole)
			deleteObject(serviceSource)
			deleteObject(gate)
		})

		// Apply this deployment wave without waiting for either SQL role or Database.
		for _, object := range []client.Object{serviceSource, serviceRole, readerRole, database, gate} {
			Expect(apiClient.Create(ctx, object)).To(Succeed())
		}
		_, err = kubeClient.BatchV1().Jobs(job.Namespace).Create(ctx, job, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		serviceOutput := installedConnectionOutput(ctx, appNamespace.Name, installedConnectionServiceOutput)
		readerOutput := installedConnectionOutput(ctx, appNamespace.Name, installedConnectionAdminOutput)
		Expect(bytes.Equal(serviceOutput.Data["password"], serviceSource.Data["password"])).To(BeTrue())
		readerSource := &corev1.Secret{}
		Expect(apiClient.Get(ctx,
			client.ObjectKey{Namespace: appNamespace.Name, Name: "reader-source"}, readerSource)).To(Succeed())
		Expect(bytes.Equal(readerOutput.Data["password"], readerSource.Data["password"])).To(BeTrue())
		database = installedDatabase(ctx, database, metav1.ConditionFalse, "")
		Expect(database.Status.HasManagedSQL).To(BeFalse(), "SQL outage must not be hidden by initial Secret publication")
		Eventually(func() bool {
			return strings.Contains(installedConnectionJobLogs(ctx, job), installedConnectionRetryMessage)
		}).
			WithContext(ctx).WithTimeout(40*time.Second).WithPolling(200*time.Millisecond).Should(BeTrue(),
			"migration Job must start from the early output and retry before SQL is available")

		installedConnectionServiceSelector(ctx, originalSelector)
		Eventually(func() bool {
			return strings.Contains(installedConnectionJobLogs(ctx, job), installedConnectionEstablishedMessage)
		}).
			WithContext(ctx).WithTimeout(60 * time.Second).WithPolling(200 * time.Millisecond).Should(BeTrue())
		Eventually(func() bool {
			latest := &epco.Database{}
			if apiClient.Get(ctx, client.ObjectKeyFromObject(database), latest) != nil {
				return false
			}
			condition := meta.FindStatusCondition(latest.Status.Conditions, "Ready")
			return latest.Status.HasManagedSQL && condition != nil && condition.Status == metav1.ConditionFalse
		}).WithContext(ctx).WithTimeout(40*time.Second).Should(BeTrue(),
			"named table is absent while core SQL lifecycle is managed")
		Expect(installedDatabaseSQL(ctx, installedConnectionDatabase,
			"SELECT to_regclass('app.orders') IS NULL")).To(Equal("t"))
		Expect(apiClient.Get(ctx, client.ObjectKeyFromObject(gate), gate)).To(Succeed())
		gate.Data = map[string]string{"ready": "yes"}
		Expect(apiClient.Update(ctx, gate)).To(Succeed())
		Eventually(func() bool {
			latest, err := kubeClient.BatchV1().Jobs(job.Namespace).Get(ctx, job.Name, metav1.GetOptions{})
			return err == nil && latest.Status.Succeeded == 1
		}).WithContext(ctx).WithTimeout(90*time.Second).Should(BeTrue(),
			"migration must create the missing named table without waiting for Database Ready")
		database = installedDatabase(ctx, database, metav1.ConditionTrue, "Ready")
		serviceOutput = installedConnectionOutput(ctx, appNamespace.Name, installedConnectionServiceOutput)
		readerOutput = installedConnectionOutput(ctx, appNamespace.Name, installedConnectionAdminOutput)
		Expect(installedConnectionLogin(ctx, serviceOutput, "SELECT current_user, current_database()", true)).
			To(Equal(installedConnectionService + "|" + installedConnectionDatabase))
		Expect(installedConnectionLogin(ctx, readerOutput, "SELECT value FROM app.orders", true)).To(Equal("42"))
		installedConnectionLogin(ctx, readerOutput, "INSERT INTO app.orders VALUES (99)", false)
		Expect(installedConnectionLogin(ctx, serviceOutput,
			"INSERT INTO app.orders VALUES (43); SELECT count(*) FROM app.orders", true)).
			To(ContainSubstring("2"))
		// Confirm defaults before relying on them for a newly created object.
		Expect(installedDatabaseSQL(ctx, installedConnectionDatabase,
			"SELECT has_schema_privilege('"+installedConnectionAdmin+"','app','USAGE') AND EXISTS ("+
				"SELECT 1 FROM pg_default_acl d CROSS JOIN LATERAL aclexplode(d.defaclacl) a "+
				"WHERE d.defaclrole='"+installedConnectionService+"'::regrole "+
				"AND d.defaclnamespace='app'::regnamespace AND a.grantee='"+installedConnectionAdmin+
				"'::regrole AND a.privilege_type='SELECT')")).
			To(Equal("t"))
		installedConnectionLogin(ctx, serviceOutput,
			"CREATE TABLE app.future(value integer); INSERT INTO app.future VALUES (23)", true)
		Expect(installedConnectionLogin(ctx, readerOutput, "SELECT value FROM app.future", true)).To(Equal("23"))

		passwords := [][]byte{bytes.Clone(serviceSource.Data["password"]), bytes.Clone(readerSource.Data["password"])}
		Expect(apiClient.Get(ctx, client.ObjectKeyFromObject(serviceSource), serviceSource)).To(Succeed())
		serviceSource.Data["password"] = []byte("second-installed-application-password")
		passwords = append(passwords, bytes.Clone(serviceSource.Data["password"]))
		Expect(apiClient.Update(ctx, serviceSource)).To(Succeed())
		serviceOutput = installedConnectionPassword(ctx, appNamespace.Name,
			installedConnectionServiceOutput, serviceSource.Data["password"])
		Expect(installedConnectionLogin(ctx, serviceOutput,
			"SELECT value FROM app.orders ORDER BY value LIMIT 1", true)).To(Equal("42"))
		oldOutputUID := serviceOutput.UID
		Expect(apiClient.Delete(ctx, serviceOutput, client.Preconditions{UID: new(serviceOutput.UID)})).To(Succeed())
		serviceOutput = installedConnectionPassword(ctx, appNamespace.Name,
			installedConnectionServiceOutput, serviceSource.Data["password"])
		Expect(serviceOutput.UID).NotTo(Equal(oldOutputUID),
			"deleted output must be recreated after current SQL acknowledgement")
		Expect(installedConnectionLogin(ctx, serviceOutput, "SELECT current_user", true)).
			To(Equal(installedConnectionService))

		Expect(apiClient.Get(ctx, client.ObjectKeyFromObject(appNamespace), appNamespace)).To(Succeed())
		appNamespace.Labels = nil
		Expect(apiClient.Update(ctx, appNamespace)).To(Succeed())
		database = installedDatabase(ctx, database, metav1.ConditionFalse, "AccessDenied")
		readerRole = installedRole(ctx, readerRole, metav1.ConditionFalse, "AccessDenied")
		beforeReader := bytes.Clone(readerOutput.Data["password"])
		Expect(apiClient.Get(ctx, client.ObjectKeyFromObject(readerSource), readerSource)).To(Succeed())
		readerSource.Data["password"] = []byte("denied-namespace-rotation-password")
		passwords = append(passwords, bytes.Clone(readerSource.Data["password"]))
		Expect(apiClient.Update(ctx, readerSource)).To(Succeed())
		database.Labels = map[string]string{installedAccessLabel: installedAllowedLabel}
		Expect(apiClient.Update(ctx, database)).To(Succeed())
		database = installedDatabase(ctx, database, metav1.ConditionFalse, "AccessDenied")
		Consistently(func() bool {
			current := &corev1.Secret{}
			return apiClient.Get(ctx, client.ObjectKeyFromObject(readerOutput), current) == nil &&
				bytes.Equal(current.Data["password"], beforeReader)
		}).WithContext(ctx).WithTimeout(3*time.Second).Should(BeTrue(),
			"Database labels do not authorize the Namespace or refresh denied output")
		Expect(apiClient.Get(ctx, client.ObjectKeyFromObject(appNamespace), appNamespace)).To(Succeed())
		appNamespace.Labels = map[string]string{installedAccessLabel: installedAllowedLabel}
		Expect(apiClient.Update(ctx, appNamespace)).To(Succeed())
		readerOutput = installedConnectionPassword(ctx, appNamespace.Name,
			installedConnectionAdminOutput, readerSource.Data["password"])
		database = installedDatabase(ctx, database, metav1.ConditionTrue, "Ready")
		Expect(installedConnectionLogin(ctx, readerOutput, "SELECT count(*) FROM app.orders", true)).To(Equal("2"))

		// Check raw controller logs and credential-free metadata/status by boolean
		// membership, keeping secret values out of failure diagnostics.
		rawLogs, err := kubeClient.CoreV1().Pods(operatorNamespace).List(
			ctx, metav1.ListOptions{LabelSelector: installedManagerSelector})
		Expect(err).NotTo(HaveOccurred())
		Expect(rawLogs.Items).To(HaveLen(1))
		logs, err := kubeClient.CoreV1().Pods(operatorNamespace).GetLogs(
			rawLogs.Items[0].Name, &corev1.PodLogOptions{Container: "manager"}).DoRaw(ctx)
		Expect(err).NotTo(HaveOccurred())
		status, err := json.Marshal(database.Status)
		Expect(err).NotTo(HaveOccurred())
		metadata, err := json.Marshal([]map[string]string{serviceOutput.Annotations, readerOutput.Annotations})
		Expect(err).NotTo(HaveOccurred())
		fixtureAdmin, err := kubeClient.CoreV1().Secrets("epco-postgres").Get(ctx, "postgres", metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		passwords = append(passwords, fixtureAdmin.Data["password"])
		for _, password := range passwords {
			for _, artifact := range [][]byte{logs, status, metadata} {
				Expect(bytes.Contains(artifact, password)).To(BeFalse(),
					"application or administrative credentials leaked into logs/status/provenance")
			}
		}
		for _, output := range []*corev1.Secret{serviceOutput, readerOutput} {
			for _, value := range output.Data {
				Expect(bytes.Contains(value, fixtureAdmin.Data["password"])).To(BeFalse(),
					"application output contains administrative credentials")
			}
			Expect(output.Namespace).To(Equal(appNamespace.Name))
			Expect(string(output.Data["dbname"])).To(Equal(installedConnectionDatabase))
			Expect(string(output.Data["host"])).To(Equal(installedPostgresHost))
			Expect(output.UID).NotTo(Equal(types.UID("")))
		}
		Expect(installedConnectionJobLogs(ctx, job)).To(ContainSubstring("Migration completed"))
	}, NodeTimeout(8*time.Minute))
})
