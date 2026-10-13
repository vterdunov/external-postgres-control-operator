//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"maps"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	epco "github.com/vterdunov/external-postgres-control-operator/api/v1alpha1"
	"github.com/vterdunov/external-postgres-control-operator/internal/controller"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const r6MigrationJobKind = "Job"

func r6RetainedCredentials(ctx context.Context, app *r6Application) {
	previousDatabase := app.database.DeepCopy()
	app.database.Spec.ReclaimPolicy = epco.ReclaimRetain
	ExpectWithOffset(1, apiClient.Patch(ctx, app.database, client.MergeFrom(previousDatabase))).To(Succeed())
	role := app.roles[r6Reader]
	previousRole := role.DeepCopy()
	role.Spec.ReclaimPolicy = epco.ReclaimRetain
	ExpectWithOffset(1, apiClient.Patch(ctx, role, client.MergeFrom(previousRole))).To(Succeed())
	app.ready(ctx)
	output := app.output(ctx, r6Reader)
	source := &corev1.Secret{}
	ExpectWithOffset(1, apiClient.Get(ctx,
		client.ObjectKey{Namespace: app.namespace.Name, Name: r6ReaderSource}, source)).To(Succeed())
	sourceUID, outputUID := source.UID, output.UID
	password := bytes.Clone(source.Data[corev1.BasicAuthPasswordKey])
	oldRoleUID, oldDatabaseUID := role.UID, app.database.UID
	deleteObject(app.database)
	deleteObject(role)
	ExpectWithOffset(1, r6SecretUnchanged(ctx, output)).To(BeTrue())
	ExpectWithOffset(1, r6SecretUnchanged(ctx, source)).To(BeTrue())
	// A retained source with mismatched provenance must remain untouched until
	// its legitimate metadata is restored. This never uses an unrelated source.
	sourceAnnotations := maps.Clone(source.Annotations)
	DeferCleanup(func() {
		r6RestoreProvenance(source, controller.GeneratedRoleResourceAnnotation, role.Name)
	})
	source.Annotations[controller.GeneratedRoleResourceAnnotation] = r6ForeignResource
	ExpectWithOffset(1, apiClient.Update(ctx, source)).To(Succeed())
	conflictingSource := source.DeepCopy()
	recreatedRole := &epco.DatabaseRole{Namespace: role.Namespace, Name: role.Name, Spec: role.Spec}
	ExpectWithOffset(1, apiClient.Create(ctx, recreatedRole)).To(Succeed())
	app.roles[r6Reader] = recreatedRole
	installedRole(ctx, recreatedRole, metav1.ConditionFalse, "ConnectionSecretConflict")
	ExpectWithOffset(1, r6SecretUnchanged(ctx, conflictingSource)).To(BeTrue())
	ExpectWithOffset(1, apiClient.Get(ctx, client.ObjectKeyFromObject(source), source)).To(Succeed())
	source.Annotations = sourceAnnotations
	ExpectWithOffset(1, apiClient.Update(ctx, source)).To(Succeed())
	app.roles[r6Reader] = installedRole(ctx, recreatedRole, metav1.ConditionTrue, "Ready")
	ExpectWithOffset(1, app.roles[r6Reader].UID).NotTo(Equal(oldRoleUID))
	ExpectWithOffset(1, apiClient.Get(ctx, client.ObjectKeyFromObject(source), source)).To(Succeed())
	ExpectWithOffset(1, source.UID).To(Equal(sourceUID))
	ExpectWithOffset(1, bytes.Equal(source.Data[corev1.BasicAuthPasswordKey], password)).To(BeTrue())
	ExpectWithOffset(1, source.Annotations[controller.GeneratedRoleUIDAnnotation]).
		To(Equal(string(app.roles[r6Reader].UID)))
	ExpectWithOffset(1, source.OwnerReferences).To(BeEmpty())
	outputAnnotations := maps.Clone(output.Annotations)
	DeferCleanup(func() {
		r6RestoreProvenance(output, controller.ConnectionDatabaseResourceAnnotation, app.database.Name)
	})
	output.Annotations[controller.ConnectionDatabaseResourceAnnotation] = r6ForeignResource
	ExpectWithOffset(1, apiClient.Update(ctx, output)).To(Succeed())
	conflictingOutput := output.DeepCopy()
	recreatedDatabase := &epco.Database{Namespace: app.database.Namespace, Name: app.database.Name,
		Spec: app.database.Spec}
	ExpectWithOffset(1, apiClient.Create(ctx, recreatedDatabase)).To(Succeed())
	app.database = recreatedDatabase
	app.database = installedDatabase(ctx, app.database, metav1.ConditionFalse, "ConnectionSecretConflict")
	ExpectWithOffset(1, r6SecretUnchanged(ctx, conflictingOutput)).To(BeTrue())
	ExpectWithOffset(1, apiClient.Get(ctx, client.ObjectKeyFromObject(output), output)).To(Succeed())
	output.Annotations = outputAnnotations
	ExpectWithOffset(1, apiClient.Update(ctx, output)).To(Succeed())
	app.ready(ctx)
	output = app.output(ctx, r6Reader)
	ExpectWithOffset(1, app.database.UID).NotTo(Equal(oldDatabaseUID))
	ExpectWithOffset(1, output.UID).To(Equal(outputUID))
	ExpectWithOffset(1, bytes.Equal(output.Data[corev1.BasicAuthPasswordKey], password)).To(BeTrue())
	ExpectWithOffset(1, output.Annotations[controller.ConnectionDatabaseUIDAnnotation]).To(Equal(string(app.database.UID)))
	ExpectWithOffset(1, app.query(ctx, output, "SELECT value FROM app.orders", "")).To(Equal("42"))
	previousDatabase = app.database.DeepCopy()
	app.database.Spec.ReclaimPolicy = epco.ReclaimDelete
	ExpectWithOffset(1, apiClient.Patch(ctx, app.database, client.MergeFrom(previousDatabase))).To(Succeed())
	previousRole = app.roles[r6Reader].DeepCopy()
	app.roles[r6Reader].Spec.ReclaimPolicy = epco.ReclaimDelete
	ExpectWithOffset(1, apiClient.Patch(ctx, app.roles[r6Reader], client.MergeFrom(previousRole))).To(Succeed())
	app.ready(ctx)
}

func r6PendingNamedRevoke(ctx context.Context, app *r6Application, objectOID, granteeOID string, generation int64) {
	Eventually(func() bool {
		current := &epco.Database{}
		if apiClient.Get(ctx, client.ObjectKeyFromObject(app.database), current) != nil {
			return false
		}
		for _, target := range current.Status.ManagedACLTargets {
			if target.Database != string(app.database.Spec.Name) {
				continue
			}
			for _, binding := range target.Bindings {
				if binding.Scope == r6ACLTable && binding.Class == r6ACLTable &&
					binding.Schema == installedDatabaseApplicationSchema &&
					binding.Table == r6Orders && binding.Grantee == string(app.roles[r6Named].Spec.Name) &&
					binding.Privilege == "SELECT" && binding.Pending == "revoke" &&
					binding.Generation == generation && current.Generation == generation &&
					r6BindingOIDsMatch(binding, objectOID, granteeOID) {
					return true
				}
			}
		}
		return false
	}).WithContext(ctx).WithTimeout(20*time.Second).Should(BeTrue(), "concrete revoke must be durably checkpointed")
}

func r6HealthyManager(ctx context.Context, renewedAfter time.Time) {
	Eventually(func() bool {
		pods, err := kubeClient.CoreV1().Pods(operatorNamespace).List(ctx,
			metav1.ListOptions{LabelSelector: installedManagerSelector})
		if err != nil || len(pods.Items) != 1 {
			return false
		}
		pod := pods.Items[0]
		if pod.DeletionTimestamp != nil || pod.Status.Phase != corev1.PodRunning || len(pod.Status.ContainerStatuses) == 0 {
			return false
		}
		for _, container := range pod.Status.ContainerStatuses {
			if !container.Ready {
				return false
			}
		}
		lease, err := kubeClient.CoordinationV1().Leases(operatorNamespace).Get(ctx, "ccd893cc.epco.io", metav1.GetOptions{})
		return err == nil && lease.Spec.HolderIdentity != nil && lease.Spec.RenewTime != nil &&
			!lease.Spec.RenewTime.Before(&metav1.MicroTime{Time: renewedAfter}) &&
			strings.HasPrefix(*lease.Spec.HolderIdentity, pod.Name+"_")
	}).WithContext(ctx).WithTimeout(60*time.Second).WithPolling(200*time.Millisecond).Should(BeTrue(),
		"one surviving Ready manager must hold a newly renewed Lease")
}

func r6RestartManager(ctx context.Context) {
	lease, err := kubeClient.CoordinationV1().Leases(operatorNamespace).Get(ctx, "ccd893cc.epco.io", metav1.GetOptions{})
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	ExpectWithOffset(1, lease.Spec.HolderIdentity).NotTo(BeNil())
	pods, err := kubeClient.CoreV1().Pods(operatorNamespace).List(ctx,
		metav1.ListOptions{LabelSelector: installedManagerSelector})
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	ExpectWithOffset(1, pods.Items).To(HaveLen(1))
	pod := pods.Items[0]
	ExpectWithOffset(1, strings.HasPrefix(*lease.Spec.HolderIdentity, pod.Name+"_")).To(BeTrue())
	restartedAt := time.Now()
	DeferCleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 65*time.Second)
		defer cancel()
		r6HealthyManager(cleanup, restartedAt)
	})
	ExpectWithOffset(1, kubeClient.CoreV1().Pods(operatorNamespace).
		Delete(ctx, pod.Name, metav1.DeleteOptions{})).To(Succeed())
	r6HealthyManager(ctx, restartedAt)
}

func r6RestoreProvenance(secret *corev1.Secret, key, value string) {
	cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	current := &corev1.Secret{}
	if apiClient.Get(cleanup, client.ObjectKeyFromObject(secret), current) == nil &&
		current.UID == secret.UID && current.Annotations[key] == r6ForeignResource {
		current.Annotations[key] = value
		Expect(apiClient.Update(cleanup, current)).To(Succeed())
	}
}

func r6ServiceDisconnected(ctx context.Context, app *r6Application) {
	Eventually(func() bool {
		slices, err := kubeClient.DiscoveryV1().EndpointSlices("epco-postgres").List(ctx,
			metav1.ListOptions{LabelSelector: "kubernetes.io/service-name=postgres"})
		if err != nil {
			return false
		}
		for _, slice := range slices.Items {
			for _, endpoint := range slice.Endpoints {
				if endpoint.Conditions.Ready == nil || *endpoint.Conditions.Ready {
					return false
				}
			}
		}
		return true
	}).WithContext(ctx).WithTimeout(20*time.Second).WithPolling(100*time.Millisecond).Should(BeTrue(),
		"owned Service ready endpoints must be withdrawn before source mutation")
	for _, name := range []string{r6Runtime, r6Reader} {
		app.roles[name] = installedRole(ctx, app.roles[name], metav1.ConditionFalse, "ConnectionUnavailable")
	}
	app.database = installedDatabase(ctx, app.database, metav1.ConditionFalse, "")
}

func r6RefreshMigrationGate(ctx context.Context, job *batchv1.Job, revision string) {
	EventuallyWithOffset(1, func() error {
		return installedRefreshMigrationGate(ctx, apiClient, job, revision)
	}).WithContext(ctx).WithTimeout(20 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())
}

func (app *r6Application) passwordAuthenticationControl(ctx context.Context, output *corev1.Secret) {
	ExpectWithOffset(1, app.query(ctx, output, "SELECT current_user", "")).
		To(Equal(string(output.Data["user"])))
	bad := output.DeepCopy()
	bad.Data[corev1.BasicAuthPasswordKey] = []byte("r6-known-invalid-password-control")
	ExpectWithOffset(1, bytes.Equal(bad.Data[corev1.BasicAuthPasswordKey], output.Data[corev1.BasicAuthPasswordKey])).
		To(BeFalse(), "negative password control must use a different credential")
	app.query(ctx, bad, "SELECT 1", "28P01")
}

func r6BindingOIDsMatch(binding epco.ACLBinding, objectOID, granteeOID string) bool {
	return strconv.FormatUint(uint64(binding.ObjectOID), 10) == objectOID &&
		strconv.FormatUint(uint64(binding.GranteeOID), 10) == granteeOID
}
