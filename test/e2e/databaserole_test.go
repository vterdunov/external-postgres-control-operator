//go:build e2e

package e2e

import (
	"bytes"
	"context"
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
	installedManagerSelector = "control-plane=controller-manager"
	installedAccessLabel     = "epco-r1-access"
	installedRoleTLSMode     = "disable"
	installedPostgresHost    = "postgres.epco-postgres.svc.cluster.local"
	installedAllowedLabel    = "allowed"
	installedGeneratedName   = "generated-source"
)

func installedRole(ctx context.Context, role *epco.DatabaseRole,
	status metav1.ConditionStatus, reason string) *epco.DatabaseRole {
	var latest *epco.DatabaseRole
	Eventually(func(g Gomega) {
		latest = &epco.DatabaseRole{}
		g.Expect(apiClient.Get(ctx, client.ObjectKeyFromObject(role), latest)).To(Succeed())
		condition := meta.FindStatusCondition(latest.Status.Conditions, "Ready")
		g.Expect(condition).NotTo(BeNil())
		g.Expect(condition.Status).To(Equal(status))
		g.Expect(condition.Reason).To(Equal(reason))
		g.Expect(condition.ObservedGeneration).To(Equal(latest.Generation))
	}).WithContext(ctx).WithTimeout(40 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())
	return latest
}
func installedRoleDescriptor(ctx context.Context, name string, access *epco.ClusterAccess) *epco.PostgresCluster {
	fixture, err := kubeClient.CoreV1().Secrets("epco-postgres").Get(ctx, "postgres", metav1.GetOptions{})
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	admin := &corev1.Secret{
		Namespace: operatorNamespace,
		Name:      name + "-admin",
		Type:      corev1.SecretTypeBasicAuth,
		Data:      map[string][]byte{"username": []byte("postgres"), "password": fixture.Data["password"]},
	}
	ExpectWithOffset(1, apiClient.Create(ctx, admin)).To(Succeed())
	DeferCleanup(func() { deleteObject(admin) })
	cluster := &epco.PostgresCluster{
		Namespace: operatorNamespace,
		Name:      name,
		Spec: epco.PostgresClusterSpec{
			Connection:        epco.PostgresConnection{Host: installedPostgresHost},
			CredentialsSecret: epco.LocalReference{Name: admin.Name},
			TLS:               &epco.PostgresTLS{Mode: installedRoleTLSMode},
			Access:            access,
			ReconcileInterval: "1h",
			ConnectTimeout:    "2s",
			StatementTimeout:  "2s",
		},
	}
	ExpectWithOffset(1, apiClient.Create(ctx, cluster)).To(Succeed())
	DeferCleanup(func() { deleteObject(cluster) })
	installedClusterStatus(ctx, client.ObjectKeyFromObject(cluster), metav1.ConditionTrue, "Ready")
	return cluster
}
func expectApplicationLogin(ctx context.Context, sqlName string, password []byte) {
	const loginScript = "IFS= read -r PGPASSWORD; export PGPASSWORD; " +
		"exec psql -h 127.0.0.1 -U \"$1\" -d postgres -Atc 'SELECT current_user'"
	cmd := kubectlCommand(ctx, "-n", "epco-postgres", "exec", "-i", "deployment/postgres", "--",
		"sh", "-c", loginScript, "sh", sqlName)
	cmd.Stdin = strings.NewReader(string(password) + "\n")
	output, err := utils.Run(cmd)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	ExpectWithOffset(1, strings.TrimSpace(output)).To(Equal(sqlName))
}
func expectSQLRoleGone(ctx context.Context, sqlName string) {
	query := "SELECT count(*) FROM pg_catalog.pg_roles WHERE rolname = '" + sqlName + "'"
	cmd := kubectlCommand(ctx, "-n", "epco-postgres", "exec", "deployment/postgres", "--",
		"psql", "-h", "127.0.0.1", "-U", "postgres", "-Atc", query)
	output, err := utils.Run(cmd)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	ExpectWithOffset(1, strings.TrimSpace(output)).To(Equal("0"))
}

var _ = Describe("Installed DatabaseRole controller", func() {
	It("retains and reuses credentials, then resumes an access-denied deletion", func(ctx SpecContext) {
		appNamespace := &corev1.Namespace{
			GenerateName: "epco-role-",
			Labels:       map[string]string{installedAccessLabel: installedAllowedLabel},
		}
		Expect(apiClient.Create(ctx, appNamespace)).To(Succeed())
		DeferCleanup(func() { deleteObject(appNamespace) })
		cluster := installedRoleDescriptor(ctx, "generated-role-connection", &epco.ClusterAccess{
			NamespaceSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{installedAccessLabel: installedAllowedLabel},
			},
		})
		role := &epco.DatabaseRole{
			Namespace: appNamespace.Name,
			Name:      "generated-role",
			Spec: epco.DatabaseRoleSpec{
				Cluster:            epco.ClusterReference{Namespace: operatorNamespace, Name: cluster.Name},
				Name:               "epco_e2e_generated",
				Login:              true,
				PasswordGeneration: &epco.PasswordGeneration{SecretName: installedGeneratedName},
			},
		}
		Expect(apiClient.Create(ctx, role)).To(Succeed())
		DeferCleanup(func() {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
				ns := &corev1.Namespace{}
				if err := apiClient.Get(cleanupCtx, client.ObjectKeyFromObject(appNamespace), ns); err != nil {
					return client.IgnoreNotFound(err)
				}
				ns.Labels = map[string]string{installedAccessLabel: installedAllowedLabel}
				return apiClient.Update(cleanupCtx, ns)
			})).To(Succeed())
			deleteObject(role)
			source := &corev1.Secret{Namespace: appNamespace.Name, Name: installedGeneratedName}
			deleteObject(source)
		})
		role = installedRole(ctx, role, metav1.ConditionTrue, "Ready")
		Expect(role.Status.HasManagedSQL).To(BeTrue())
		Expect(role.Status.GeneratedPasswordInitialized).To(BeTrue())
		source := &corev1.Secret{}
		Expect(apiClient.Get(ctx, client.ObjectKey{
			Namespace: appNamespace.Name,
			Name:      installedGeneratedName,
		}, source)).To(Succeed())
		Expect(source.OwnerReferences).To(BeEmpty())
		sourceUID := source.UID
		expectApplicationLogin(ctx, string(role.Spec.Name), source.Data["password"])
		source.Data["password"] = []byte("explicit-fixture-rotation")
		Expect(apiClient.Update(ctx, source)).To(Succeed())
		Eventually(func() string {
			latest := &epco.DatabaseRole{}
			if apiClient.Get(ctx, client.ObjectKeyFromObject(role), latest) != nil {
				return ""
			}
			return latest.Status.SecretResourceVersion
		}).WithContext(ctx).WithTimeout(40 * time.Second).Should(Equal(source.ResourceVersion))
		expectApplicationLogin(ctx, string(role.Spec.Name), source.Data["password"])
		retainedPassword := bytes.Clone(source.Data["password"])
		oldUID := role.UID
		deleteObject(role)
		Expect(apiClient.Get(ctx, client.ObjectKeyFromObject(source), source)).To(Succeed())
		Expect(source.UID).To(Equal(sourceUID))
		Expect(bytes.Equal(source.Data["password"], retainedPassword)).To(BeTrue())
		expectApplicationLogin(ctx, "epco_e2e_generated", retainedPassword)
		recreated := &epco.DatabaseRole{Namespace: appNamespace.Name, Name: role.Name, Spec: role.Spec}
		Expect(apiClient.Create(ctx, recreated)).To(Succeed())
		role = installedRole(ctx, recreated, metav1.ConditionTrue, "Ready")
		Expect(role.UID).NotTo(Equal(oldUID))
		Expect(apiClient.Get(ctx, client.ObjectKeyFromObject(source), source)).To(Succeed())
		Expect(source.UID).To(Equal(sourceUID))
		Expect(bytes.Equal(source.Data["password"], retainedPassword)).To(BeTrue())
		Expect(source.Annotations[controller.GeneratedRoleUIDAnnotation]).To(Equal(string(role.UID)))
		Expect(source.OwnerReferences).To(BeEmpty())
		role.Spec.ReclaimPolicy = epco.ReclaimDelete
		Expect(apiClient.Update(ctx, role)).To(Succeed())
		role = installedRole(ctx, role, metav1.ConditionTrue, "Ready")
		Expect(apiClient.Get(ctx, client.ObjectKeyFromObject(appNamespace), appNamespace)).To(Succeed())
		appNamespace.Labels = nil
		Expect(apiClient.Update(ctx, appNamespace)).To(Succeed())
		role = installedRole(ctx, role, metav1.ConditionFalse, "AccessDenied")
		Expect(apiClient.Delete(ctx, role)).To(Succeed())
		role = installedRole(ctx, role, metav1.ConditionFalse, "AccessDenied")
		Expect(role.DeletionTimestamp).NotTo(BeNil())
		Expect(role.Finalizers).To(ContainElement(controller.RoleFinalizer))
		Expect(apiClient.Get(ctx, client.ObjectKeyFromObject(source), source)).To(Succeed())
		expectApplicationLogin(ctx, "epco_e2e_generated", retainedPassword)
		appNamespace.Labels = map[string]string{installedAccessLabel: installedAllowedLabel}
		Expect(apiClient.Update(ctx, appNamespace)).To(Succeed())
		Eventually(func() bool {
			return apierrors.IsNotFound(apiClient.Get(ctx, client.ObjectKeyFromObject(role), &epco.DatabaseRole{}))
		}).WithContext(ctx).WithTimeout(40 * time.Second).Should(BeTrue())
		Eventually(func() bool {
			return apierrors.IsNotFound(apiClient.Get(ctx, client.ObjectKeyFromObject(source), &corev1.Secret{}))
		}).WithContext(ctx).WithTimeout(20 * time.Second).Should(BeTrue())
		expectSQLRoleGone(ctx, "epco_e2e_generated")
	}, NodeTimeout(5*time.Minute))

	It("reconciles roles after Lease takeover", func(ctx SpecContext) {
		cluster := installedRoleDescriptor(ctx, "leader-role-connection", nil)
		deployment, err := kubeClient.AppsV1().Deployments(operatorNamespace).Get(
			ctx, "epco-controller-manager", metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		originalReplicas := *deployment.Spec.Replicas
		setReplicas := func(count int32) {
			Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
				latest, getErr := kubeClient.AppsV1().Deployments(operatorNamespace).Get(ctx, deployment.Name, metav1.GetOptions{})
				if getErr != nil {
					return getErr
				}
				latest.Spec.Replicas = new(count)
				_, updateErr := kubeClient.AppsV1().Deployments(operatorNamespace).Update(ctx, latest, metav1.UpdateOptions{})
				return updateErr
			})).To(Succeed())
		}
		DeferCleanup(func() {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
				latest, getErr := kubeClient.AppsV1().Deployments(operatorNamespace).Get(
					cleanupCtx, deployment.Name, metav1.GetOptions{})
				if getErr != nil {
					return getErr
				}
				latest.Spec.Replicas = new(originalReplicas)
				_, updateErr := kubeClient.AppsV1().Deployments(operatorNamespace).Update(
					cleanupCtx, latest, metav1.UpdateOptions{})
				return updateErr
			})).To(Succeed())
			Eventually(func() int {
				pods, listErr := kubeClient.CoreV1().Pods(operatorNamespace).List(cleanupCtx,
					metav1.ListOptions{LabelSelector: installedManagerSelector})
				if listErr != nil {
					return 0
				}
				return len(pods.Items)
			}).WithContext(cleanupCtx).WithTimeout(50 * time.Second).Should(Equal(int(originalReplicas)))
		})
		role := &epco.DatabaseRole{
			Namespace: operatorNamespace,
			Name:      "leader-role",
			Spec: epco.DatabaseRoleSpec{
				Cluster:       epco.ClusterReference{Name: cluster.Name},
				Name:          "epco_e2e_leader",
				ReclaimPolicy: epco.ReclaimDelete,
			},
		}
		Expect(apiClient.Create(ctx, role)).To(Succeed())
		DeferCleanup(func() { deleteObject(role) })
		role = installedRole(ctx, role, metav1.ConditionTrue, "Ready")
		setReplicas(2)
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
		pods, err := kubeClient.CoreV1().Pods(operatorNamespace).List(ctx, metav1.ListOptions{
			LabelSelector: installedManagerSelector,
		})
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
			latest, getErr := kubeClient.CoordinationV1().Leases(operatorNamespace).Get(ctx, lease.Name, metav1.GetOptions{})
			if getErr != nil || latest.Spec.HolderIdentity == nil {
				return ""
			}
			return *latest.Spec.HolderIdentity
		}).WithContext(ctx).WithTimeout(60 * time.Second).Should(SatisfyAll(Not(BeEmpty()), Not(Equal(oldHolder))))
		Expect(apiClient.Get(ctx, client.ObjectKeyFromObject(role), role)).To(Succeed())
		role.Spec.Comment = new("reconciled after takeover")
		Expect(apiClient.Update(ctx, role)).To(Succeed())
		role = installedRole(ctx, role, metav1.ConditionTrue, "Ready")
		Expect(role.Status.AppliedGeneration).To(Equal(role.Generation))
	}, NodeTimeout(4*time.Minute))
})
