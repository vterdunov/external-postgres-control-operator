//go:build e2e

package e2e

import (
	"context"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	epco "github.com/vterdunov/external-postgres-control-operator/api/v1alpha1"
	"github.com/vterdunov/external-postgres-control-operator/internal/controller"
	"github.com/vterdunov/external-postgres-control-operator/test/utils"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	installedDatabaseApplicationSchema = "app"
	installedDatabasePostgresName      = "postgres"
	installedDatabaseAdmin             = "epco_e2e_database_admin"
	installedDatabaseOwnerA            = "epco_e2e_database_owner_a"
	installedDatabaseOwnerB            = "epco_e2e_database_owner_b"
	installedDatabaseName              = "epco_e2e_database"
	installedDatabasePassword          = "isolated-database-fixture-password"
)

func installedDatabaseSQL(ctx context.Context, database, query string) string {
	cmd := kubectlCommand(ctx, "-n", "epco-postgres", "exec", "deployment/postgres", "--",
		"psql", "-X", "-v", "ON_ERROR_STOP=1", "-h", "127.0.0.1", "-U", installedDatabasePostgresName,
		"-d", database, "-Atc", query)
	output, err := utils.Run(cmd)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	return strings.TrimSpace(output)
}

func installedDatabase(ctx context.Context, database *epco.Database,
	status metav1.ConditionStatus, reason string) *epco.Database {
	var latest *epco.Database
	Eventually(func(g Gomega) {
		latest = &epco.Database{}
		g.Expect(apiClient.Get(ctx, client.ObjectKeyFromObject(database), latest)).To(Succeed())
		condition := meta.FindStatusCondition(latest.Status.Conditions, "Ready")
		g.Expect(condition).NotTo(BeNil())
		g.Expect(condition.Status).To(Equal(status))
		if reason != "" {
			g.Expect(condition.Reason).To(Equal(reason))
		}
		g.Expect(condition.ObservedGeneration).To(Equal(latest.Generation))
		if status == metav1.ConditionTrue {
			g.Expect(latest.Status.HasManagedSQL).To(BeTrue())
			g.Expect(latest.Status.AppliedGeneration).To(Equal(latest.Generation))
		}
	}).WithContext(ctx).WithTimeout(40 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())
	return latest
}

func installedDatabaseOwner(ctx context.Context, cluster *epco.PostgresCluster, name string) {
	role := &epco.DatabaseRole{Name: strings.ReplaceAll(name, "_", "-"), Namespace: operatorNamespace,
		Spec: epco.DatabaseRoleSpec{Name: epco.SQLName(name), Cluster: epco.ClusterReference{Name: cluster.Name}}}
	ExpectWithOffset(1, apiClient.Create(ctx, role)).To(Succeed())
	DeferCleanup(func() { deleteObject(role) })
	installedRole(ctx, role, metav1.ConditionTrue, "Ready")
}

func installedDatabaseRemoved(ctx context.Context, database *epco.Database) {
	Eventually(func() bool {
		return apierrors.IsNotFound(apiClient.Get(ctx, client.ObjectKeyFromObject(database), &epco.Database{}))
	}).WithContext(ctx).WithTimeout(40 * time.Second).Should(BeTrue())
}

// Start a real fixture backend and identify it through its catalog PID. Only
// this explicitly owned backend is canceled by test teardown or repair.
func installedDatabaseBlocker(ctx context.Context, database, sql string) (int, func()) {
	processCtx, cancel := context.WithCancel(ctx)
	cmd := kubectlCommand(processCtx, "-n", "epco-postgres", "exec", "deployment/postgres", "--",
		"psql", "-X", "-v", "ON_ERROR_STOP=1", "-h", "127.0.0.1", "-U", installedDatabasePostgresName,
		"-d", database, "-Atc", sql)
	ExpectWithOffset(1, cmd.Start()).To(Succeed())
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var pid int
	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		cleanup, stopCleanup := boundedContext()
		defer stopCleanup()
		if pid != 0 {
			installedDatabaseSQL(cleanup, installedDatabasePostgresName, "SELECT pg_cancel_backend("+strconv.Itoa(pid)+")")
		}
		cancel()
		Eventually(done).WithTimeout(10 * time.Second).Should(Receive())
	}
	DeferCleanup(stop)
	Eventually(func(g Gomega) {
		output := installedDatabaseSQL(ctx, installedDatabasePostgresName,
			"SELECT pid FROM pg_stat_activity WHERE datname = '"+
				database+"' AND wait_event = 'PgSleep' AND query = '"+strings.ReplaceAll(sql, "'", "''")+"'")
		var err error
		pid, err = strconv.Atoi(output)
		g.Expect(err).NotTo(HaveOccurred())
	}).WithContext(ctx).WithTimeout(10 * time.Second).Should(Succeed())
	return pid, stop
}

func installedDatabaseFixtureCleanup(name string) {
	DeferCleanup(func() {
		ctx, cancel := boundedContext()
		defer cancel()
		installedDatabaseSQL(ctx, installedDatabasePostgresName, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
	})
}

var _ = Describe("Installed Database controller", func() {
	It("manages restricted owners and schemas and repairs blocked deletion", func(ctx SpecContext) {
		cluster := installedRoleDescriptor(ctx, "database-restricted-connection", nil)
		installedDatabaseOwner(ctx, cluster, installedDatabaseOwnerA)
		installedDatabaseOwner(ctx, cluster, installedDatabaseOwnerB)
		installedDatabaseSQL(ctx, installedDatabasePostgresName, "CREATE ROLE "+installedDatabaseAdmin+
			" LOGIN NOSUPERUSER CREATEDB NOCREATEROLE INHERIT PASSWORD '"+installedDatabasePassword+"'; "+
			"GRANT "+installedDatabaseOwnerA+", "+installedDatabaseOwnerB+" TO "+installedDatabaseAdmin)
		DeferCleanup(func() {
			cleanup, cancel := boundedContext()
			defer cancel()
			installedDatabaseSQL(cleanup, installedDatabasePostgresName, "DROP ROLE "+installedDatabaseAdmin)
		})
		installedDatabaseFixtureCleanup(installedDatabaseName)
		admin := &corev1.Secret{}
		adminKey := client.ObjectKey{Namespace: operatorNamespace, Name: cluster.Spec.CredentialsSecret.Name}
		Expect(apiClient.Get(ctx, adminKey, admin)).To(Succeed())
		admin.Data[corev1.BasicAuthUsernameKey] = []byte(installedDatabaseAdmin)
		admin.Data[corev1.BasicAuthPasswordKey] = []byte(installedDatabasePassword)
		Expect(apiClient.Update(ctx, admin)).To(Succeed())
		installedClusterStatus(ctx, client.ObjectKeyFromObject(cluster), metav1.ConditionTrue, "Ready")
		Expect(installedDatabaseSQL(ctx, installedDatabasePostgresName,
			"SELECT rolcanlogin, rolcreatedb, rolsuper FROM pg_roles WHERE rolname='"+
				installedDatabaseOwnerA+"'")).To(Equal("f|f|f"))
		database := &epco.Database{Name: "database-restricted", Namespace: operatorNamespace,
			Spec: epco.DatabaseSpec{Name: installedDatabaseName, Owner: installedDatabaseOwnerA,
				Cluster: epco.ClusterReference{Name: cluster.Name},
				Schemas: []epco.SchemaSpec{{Name: installedDatabaseApplicationSchema}, {Name: "kept"}}}}
		Expect(apiClient.Create(ctx, database)).To(Succeed())
		DeferCleanup(func() { deleteObject(database) })
		database = installedDatabase(ctx, database, metav1.ConditionTrue, "Ready")
		Expect(installedDatabaseSQL(ctx, installedDatabasePostgresName,
			"SELECT pg_get_userbyid(datdba) FROM pg_database WHERE datname='"+
				installedDatabaseName+"'")).To(Equal(installedDatabaseOwnerA))
		Expect(installedDatabaseSQL(ctx, installedDatabaseName,
			"SELECT nspname, pg_get_userbyid(nspowner) FROM pg_namespace WHERE nspname IN ('app','kept') ORDER BY nspname")).
			To(Equal("app|" + installedDatabaseOwnerA + "\nkept|" + installedDatabaseOwnerA))
		installedDatabaseSQL(ctx, installedDatabaseName, "SET ROLE "+installedDatabaseOwnerA+
			"; CREATE TABLE app.sentinel(value int); INSERT INTO app.sentinel VALUES(42)")
		database.Spec.Owner = installedDatabaseOwnerB
		database.Spec.Schemas = []epco.SchemaSpec{{Name: installedDatabaseApplicationSchema, Owner: installedDatabaseOwnerB}}
		Expect(apiClient.Update(ctx, database)).To(Succeed())
		database = installedDatabase(ctx, database, metav1.ConditionTrue, "Ready")
		Expect(installedDatabaseSQL(ctx, installedDatabasePostgresName,
			"SELECT pg_get_userbyid(datdba) FROM pg_database WHERE datname='"+
				installedDatabaseName+"'")).To(Equal(installedDatabaseOwnerB))
		Expect(installedDatabaseSQL(ctx, installedDatabaseName,
			"SELECT nspname, pg_get_userbyid(nspowner) FROM pg_namespace WHERE nspname IN ('app','kept') ORDER BY nspname")).
			To(Equal("app|" + installedDatabaseOwnerB + "\nkept|" + installedDatabaseOwnerA))
		Expect(installedDatabaseSQL(ctx, installedDatabaseName, "SELECT pg_get_userbyid(relowner) FROM pg_class "+
			"WHERE oid='app.sentinel'::regclass")).To(Equal(installedDatabaseOwnerA))
		database.Spec.Schemas = []epco.SchemaSpec{{Name: installedDatabaseApplicationSchema, Ensure: epco.EnsureAbsent}}
		Expect(apiClient.Update(ctx, database)).To(Succeed())
		database = installedDatabase(ctx, database, metav1.ConditionFalse, "")
		Expect(installedDatabaseSQL(ctx, installedDatabaseName, "SELECT value FROM app.sentinel")).To(Equal("42"))
		installedDatabaseSQL(ctx, installedDatabaseName, "DROP TABLE app.sentinel")
		database = installedDatabase(ctx, database, metav1.ConditionTrue, "Ready")
		Expect(installedDatabaseSQL(ctx, installedDatabaseName,
			"SELECT nspname FROM pg_namespace WHERE nspname IN ('app','kept') ORDER BY nspname")).To(Equal("kept"))
		oldUID := database.UID
		deleteObject(database)
		Expect(installedDatabaseSQL(ctx, installedDatabasePostgresName, "SELECT count(*) FROM pg_database WHERE datname='"+
			installedDatabaseName+"'")).To(Equal("1"), "default retain preserves SQL")
		recreated := &epco.Database{Name: database.Name, Namespace: database.Namespace, Spec: database.Spec}
		recreated.Spec.ReclaimPolicy = epco.ReclaimDelete
		Expect(apiClient.Create(ctx, recreated)).To(Succeed())
		database = installedDatabase(ctx, recreated, metav1.ConditionTrue, "Ready")
		Expect(database.UID).NotTo(Equal(oldUID))
		pid, stop := installedDatabaseBlocker(ctx, installedDatabaseName, "SELECT pg_sleep(120)")
		Expect(apiClient.Delete(ctx, database)).To(Succeed())
		database = installedDatabase(ctx, database, metav1.ConditionFalse, "")
		Expect(database.Finalizers).To(ContainElement(controller.DatabaseFinalizer))
		Expect(database.DeletionTimestamp).NotTo(BeNil())
		Expect(installedDatabaseSQL(ctx, installedDatabasePostgresName, "SELECT count(*) FROM pg_stat_activity WHERE pid="+
			strconv.Itoa(pid))).To(Equal("1"), "production DROP must preserve active sessions")
		Expect(installedDatabaseSQL(ctx, installedDatabasePostgresName, "SELECT count(*) FROM pg_database WHERE datname='"+
			installedDatabaseName+"'")).To(Equal("1"))
		stop()
		installedDatabaseRemoved(ctx, database)
		Expect(installedDatabaseSQL(ctx, installedDatabasePostgresName, "SELECT count(*) FROM pg_database WHERE datname='"+
			installedDatabaseName+"'")).To(Equal("0"))
	}, NodeTimeout(6*time.Minute))

	It("pauses a denied finalizer and resumes deletion when namespace authorization returns", func(ctx SpecContext) {
		appNamespace := &corev1.Namespace{GenerateName: "epco-database-",
			Labels: map[string]string{installedAccessLabel: installedAllowedLabel}}
		Expect(apiClient.Create(ctx, appNamespace)).To(Succeed())
		DeferCleanup(func() { deleteObject(appNamespace) })
		cluster := installedRoleDescriptor(ctx, "database-access-connection", &epco.ClusterAccess{
			NamespaceSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{installedAccessLabel: installedAllowedLabel}}})
		const name = "epco_e2e_database_access"
		installedDatabaseFixtureCleanup(name)
		database := &epco.Database{Name: "access-database", Namespace: appNamespace.Name,
			Spec: epco.DatabaseSpec{Name: name, Owner: installedDatabasePostgresName, ReclaimPolicy: epco.ReclaimDelete,
				Cluster: epco.ClusterReference{Name: cluster.Name, Namespace: cluster.Namespace}}}
		Expect(apiClient.Create(ctx, database)).To(Succeed())
		DeferCleanup(func() {
			cleanup, cancel := boundedContext()
			defer cancel()
			Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
				latest := &corev1.Namespace{}
				if err := apiClient.Get(cleanup, client.ObjectKeyFromObject(appNamespace), latest); err != nil {
					return client.IgnoreNotFound(err)
				}
				latest.Labels = map[string]string{installedAccessLabel: installedAllowedLabel}
				return apiClient.Update(cleanup, latest)
			})).To(Succeed())
			deleteObject(database)
		})
		database = installedDatabase(ctx, database, metav1.ConditionTrue, "Ready")
		Expect(apiClient.Get(ctx, client.ObjectKeyFromObject(appNamespace), appNamespace)).To(Succeed())
		appNamespace.Labels = nil
		Expect(apiClient.Update(ctx, appNamespace)).To(Succeed())
		database = installedDatabase(ctx, database, metav1.ConditionFalse, "AccessDenied")
		Expect(apiClient.Delete(ctx, database)).To(Succeed())
		database = installedDatabase(ctx, database, metav1.ConditionFalse, "AccessDenied")
		Expect(database.DeletionTimestamp).NotTo(BeNil())
		Expect(database.Finalizers).To(ContainElement(controller.DatabaseFinalizer))
		Expect(database.Status.HasManagedSQL).To(BeTrue())
		Consistently(func() string {
			return installedDatabaseSQL(ctx, installedDatabasePostgresName,
				"SELECT count(*) FROM pg_database WHERE datname='"+name+"'")
		}).WithContext(ctx).WithTimeout(2 * time.Second).Should(Equal("1"))
		appNamespace.Labels = map[string]string{installedAccessLabel: installedAllowedLabel}
		Expect(apiClient.Update(ctx, appNamespace)).To(Succeed())
		installedDatabaseRemoved(ctx, database)
		Expect(installedDatabaseSQL(ctx, installedDatabasePostgresName,
			"SELECT count(*) FROM pg_database WHERE datname='"+name+"'")).To(Equal("0"))
	}, NodeTimeout(4*time.Minute))

	It("closes a blocked schema session on leader cancellation and converges after Lease takeover", func(ctx SpecContext) {
		cluster := installedRoleDescriptor(ctx, "database-leader-connection", nil)
		Expect(apiClient.Get(ctx, client.ObjectKeyFromObject(cluster), cluster)).To(Succeed())
		cluster.Spec.StatementTimeout = "60s"
		Expect(apiClient.Update(ctx, cluster)).To(Succeed())
		const name = "epco_e2e_database_takeover"
		installedDatabaseFixtureCleanup(name)
		database := &epco.Database{Name: "leader-database", Namespace: operatorNamespace,
			Spec: epco.DatabaseSpec{Name: name, Owner: installedDatabasePostgresName,
				Cluster:       epco.ClusterReference{Name: cluster.Name},
				ReclaimPolicy: epco.ReclaimDelete}}
		Expect(apiClient.Create(ctx, database)).To(Succeed())
		DeferCleanup(func() { deleteObject(database) })
		database = installedDatabase(ctx, database, metav1.ConditionTrue, "Ready")
		deployment, err := kubeClient.AppsV1().Deployments(operatorNamespace).Get(ctx,
			"epco-controller-manager", metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		originalReplicas := *deployment.Spec.Replicas
		setReplicas := func(ctx context.Context, count int32) {
			_, scaleErr := utils.Run(kubectlCommand(ctx, "-n", operatorNamespace, "scale",
				"deployment/"+deployment.Name, "--replicas="+strconv.Itoa(int(count))))
			Expect(scaleErr).NotTo(HaveOccurred())
		}
		DeferCleanup(func() {
			cleanup, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			setReplicas(cleanup, originalReplicas)
			_, rolloutErr := utils.Run(kubectlCommand(cleanup, "-n", operatorNamespace, "rollout", "status",
				"deployment/"+deployment.Name, "--timeout=50s"))
			Expect(rolloutErr).NotTo(HaveOccurred())
		})
		setReplicas(ctx, 2)
		Eventually(func() int32 {
			latest, getErr := kubeClient.AppsV1().Deployments(operatorNamespace).Get(ctx, deployment.Name, metav1.GetOptions{})
			if getErr != nil {
				return 0
			}
			return latest.Status.ReadyReplicas
		}).WithContext(ctx).WithTimeout(60 * time.Second).Should(Equal(int32(2)))
		lease, err := kubeClient.CoordinationV1().Leases(operatorNamespace).Get(ctx, "ccd893cc.epco.io", metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(lease.Spec.HolderIdentity).NotTo(BeNil())
		oldHolder := *lease.Spec.HolderIdentity
		_, stop := installedDatabaseBlocker(ctx, name, "BEGIN; CREATE SCHEMA takeover; SELECT pg_sleep(120); COMMIT")
		database.Spec.Schemas = []epco.SchemaSpec{{Name: "takeover"}}
		Expect(apiClient.Update(ctx, database)).To(Succeed())
		var sqlPID string
		Eventually(func(g Gomega) {
			sqlPID = installedDatabaseSQL(ctx, installedDatabasePostgresName,
				"SELECT pid FROM pg_stat_activity WHERE datname='"+name+
					"' AND wait_event_type='Lock' AND query ILIKE '%CREATE SCHEMA%'")
			_, parseErr := strconv.Atoi(sqlPID)
			g.Expect(parseErr).NotTo(HaveOccurred())
		}).WithContext(ctx).WithTimeout(10 * time.Second).Should(Succeed())
		pods, err := kubeClient.CoreV1().Pods(operatorNamespace).List(ctx,
			metav1.ListOptions{LabelSelector: installedManagerSelector})
		Expect(err).NotTo(HaveOccurred())
		leaderPod := ""
		for _, pod := range pods.Items {
			if strings.HasPrefix(oldHolder, pod.Name+"_") {
				leaderPod = pod.Name
			}
		}
		Expect(leaderPod).NotTo(BeEmpty())
		Expect(kubeClient.CoreV1().Pods(operatorNamespace).Delete(ctx, leaderPod, metav1.DeleteOptions{})).To(Succeed())
		Eventually(func() string {
			return installedDatabaseSQL(ctx, installedDatabasePostgresName,
				"SELECT count(*) FROM pg_stat_activity WHERE pid="+sqlPID)
		}).WithContext(ctx).WithTimeout(15*time.Second).Should(Equal("0"),
			"leader cancellation must dispose the blocked SQL session before its 60s statement timeout")
		Eventually(func() string {
			latest, getErr := kubeClient.CoordinationV1().Leases(operatorNamespace).Get(ctx, lease.Name, metav1.GetOptions{})
			if getErr != nil || latest.Spec.HolderIdentity == nil {
				return ""
			}
			return *latest.Spec.HolderIdentity
		}).WithContext(ctx).WithTimeout(60 * time.Second).Should(SatisfyAll(Not(BeEmpty()), Not(Equal(oldHolder))))
		stop()
		database = installedDatabase(ctx, database, metav1.ConditionTrue, "Ready")
		Expect(installedDatabaseSQL(ctx, name,
			"SELECT pg_get_userbyid(nspowner) FROM pg_namespace WHERE nspname='takeover'")).
			To(Equal(installedDatabasePostgresName))
	}, NodeTimeout(5*time.Minute))
})
