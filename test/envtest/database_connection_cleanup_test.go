//go:build envtest

package envtest_test

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"maps"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	epco "github.com/vterdunov/external-postgres-control-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

const (
	connectionAPICachedAbsence  = "cache missing"
	connectionAPICachedOldUID   = "cache has older UID"
	connectionAPIIndependentKey = "independent"
)

func connectionAPIHistoryContains(ctx context.Context, db *epco.Database, name string) bool {
	return connectionAPIHistoryEntry(ctx, db, name) != nil
}

func connectionAPIHistoryHasUID(ctx context.Context, db *epco.Database, name, uid string) bool {
	return connectionAPIHistoryEntry(ctx, db, name)["uid"] == uid
}

func connectionAPIHistoryEntry(ctx context.Context, db *epco.Database, name string) map[string]any {
	latest := &epco.Database{}
	ExpectWithOffset(1, environment.Client.Get(ctx, client.ObjectKeyFromObject(db), latest)).To(Succeed())
	data, err := json.Marshal(latest.Status)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	var status map[string]any
	ExpectWithOffset(1, json.Unmarshal(data, &status)).To(Succeed())
	history, _ := status["connectionSecretHistory"].([]any)
	for _, state := range history {
		entry, _ := state.(map[string]any)
		if entry["name"] == name {
			return entry
		}
	}
	return nil
}

var _ = Describe("R4 independent connection output cleanup", func() {
	DescribeTable("confirms original output absence before retiring cleanup history after a stale cache miss",
		func(ctx SpecContext, observation string, replaced bool) {
			cluster, role, _ := connectionAPIFixture(ctx)
			connectionAPIApplyRole(ctx, connectionAPIRoleReconciler(), role)
			db := connectionAPIDatabase(cluster, "cached-absence-cleanup")
			Expect(scope.Create(ctx, db)).To(Succeed())
			var older *corev1.Secret
			if observation == connectionAPICachedOldUID {
				older = &corev1.Secret{Namespace: db.Namespace, Name: db.Spec.ConnectionSecrets[0].Name,
					Data: map[string][]byte{connectionAPIIndependentKey: []byte("retired-older-incarnation")}}
				Expect(environment.Client.Create(ctx, older)).To(Succeed())
				Expect(environment.Client.Delete(ctx, older)).To(Succeed())
			}
			reconciler := connectionAPIDirectReconciler(environment.Client)
			reconciler.Databases = &connectionAPIFactory{}
			_, err := reconciler.Reconcile(ctx, connectionAPIRequestFor(db))
			Expect(err).NotTo(HaveOccurred())
			expectDatabaseStatus(ctx, db, metav1.ConditionTrue, "Ready")
			original := connectionAPIOutput(ctx, db).DeepCopy()
			outputKey := client.ObjectKeyFromObject(original)
			Expect(connectionAPIHistoryContains(ctx, db, original.Name)).To(BeTrue())
			Expect(connectionAPIHistoryHasUID(ctx, db, original.Name, string(original.UID))).To(BeTrue(),
				"successful publication must durably record the current output incarnation")
			if older != nil {
				Expect(original.UID).NotTo(Equal(older.UID))
			}
			var replacement *corev1.Secret
			if replaced {
				Expect(environment.Client.Delete(ctx, original)).To(Succeed())
				replacement = &corev1.Secret{Namespace: original.Namespace, Name: original.Name,
					Data: map[string][]byte{connectionAPIIndependentKey: []byte("replacement-owned-data")}}
				Expect(scope.Create(ctx, replacement)).To(Succeed())
				Expect(replacement.UID).NotTo(Equal(original.UID))
			}
			Expect(environment.Client.Get(ctx, client.ObjectKeyFromObject(db), db)).To(Succeed())
			db.Spec.ConnectionSecrets = nil
			Expect(environment.Client.Update(ctx, db)).To(Succeed())
			// The root has observed omission while the Secret informer still has
			// not observed its own earlier successful CREATE, or still holds the
			// older incarnation. Direct APIReader remains authoritative, and no
			// API write or event is mocked.
			reconciler.Client = interceptor.NewClient(connectionAPIWatchClient(), interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey,
					object client.Object, options ...client.GetOption) error {
					if cached, ok := object.(*corev1.Secret); ok && key == outputKey {
						if older != nil {
							*cached = *older.DeepCopy()
							return nil
						}
						return apierrors.NewNotFound(schema.GroupResource{Resource: "secrets"}, key.Name)
					}
					return c.Get(ctx, key, object, options...)
				},
			})
			_, err = reconciler.Reconcile(ctx, connectionAPIRequestFor(db))
			Expect(err).NotTo(HaveOccurred())
			current := &corev1.Secret{}
			readErr := environment.Client.Get(ctx, outputKey, current)
			if replaced {
				Expect(readErr).NotTo(HaveOccurred())
				Expect(current.UID).To(Equal(replacement.UID))
				Expect(bytes.Equal(current.Data[connectionAPIIndependentKey],
					replacement.Data[connectionAPIIndependentKey])).To(BeTrue())
				Expect(connectionAPIHistoryContains(ctx, db, original.Name)).To(BeFalse())
				expectDatabaseStatus(ctx, db, metav1.ConditionTrue, "Ready")
			} else if apierrors.IsNotFound(readErr) {
				Expect(connectionAPIHistoryContains(ctx, db, original.Name)).To(BeFalse())
				expectDatabaseStatus(ctx, db, metav1.ConditionTrue, "Ready")
			} else {
				Expect(readErr).NotTo(HaveOccurred())
				Expect(current.UID).To(Equal(original.UID))
				Expect(bytes.Equal(current.Data[passwordKey], original.Data[passwordKey])).To(BeTrue())
				Expect(connectionAPIHistoryContains(ctx, db, original.Name)).To(BeTrue(),
					"a stale cached observation must not forget an original output still present in the actual API")
				Expect(connectionAPIHistoryHasUID(ctx, db, original.Name, string(original.UID))).To(BeTrue(),
					"retained cleanup responsibility must identify the exact original incarnation")
				latest := &epco.Database{}
				Expect(environment.Client.Get(ctx, client.ObjectKeyFromObject(db), latest)).To(Succeed())
				condition := meta.FindStatusCondition(latest.Status.Conditions, "Ready")
				Expect(condition).NotTo(BeNil())
				Expect(condition.Status).To(Equal(metav1.ConditionFalse))
				Expect(condition.ObservedGeneration).To(Equal(latest.Generation))
			}

			reconciler.Client = environment.Client
			_, err = reconciler.Reconcile(ctx, connectionAPIRequestFor(db))
			Expect(err).NotTo(HaveOccurred())
			readErr = environment.Client.Get(ctx, outputKey, current)
			if replaced {
				Expect(readErr).NotTo(HaveOccurred())
				Expect(current.UID).To(Equal(replacement.UID))
				Expect(bytes.Equal(current.Data[connectionAPIIndependentKey],
					replacement.Data[connectionAPIIndependentKey])).To(BeTrue())
			} else {
				Expect(apierrors.IsNotFound(readErr)).To(BeTrue(), "normal convergence must remove the omitted own output")
			}
			Expect(connectionAPIHistoryContains(ctx, db, original.Name)).To(BeFalse())
			expectDatabaseStatus(ctx, db, metav1.ConditionTrue, "Ready")
		},
		Entry("original incarnation remains in the API", connectionAPICachedAbsence, false, NodeTimeout(30*time.Second)),
		Entry("a different-UID replacement is preserved", connectionAPICachedAbsence, true, NodeTimeout(30*time.Second)),
		Entry("an older cached UID hides the existing original", connectionAPICachedOldUID, false,
			NodeTimeout(30*time.Second)),
	)

	It("refreshes a declared output while omitted-output deletion fails, then completes cleanup", func(ctx SpecContext) {
		cluster, role, source := connectionAPIFixture(ctx)
		roleReconciler := connectionAPIRoleReconciler()
		connectionAPIApplyRole(ctx, roleReconciler, role)
		db := connectionAPIDatabase(cluster, "independent-cleanup")
		omittedName := db.Spec.ConnectionSecrets[0].Name
		remaining := epco.ConnectionSecret{Name: "remaining-application-output",
			RoleRef: epco.LocalReference{Name: role.Name}}
		db.Spec.ConnectionSecrets = append(db.Spec.ConnectionSecrets, remaining)
		Expect(scope.Create(ctx, db)).To(Succeed())
		reconciler := connectionAPIDirectReconciler(environment.Client)
		reconciler.Databases = &connectionAPIFactory{}
		_, err := reconciler.Reconcile(ctx, connectionAPIRequestFor(db))
		Expect(err).NotTo(HaveOccurred())
		expectDatabaseStatus(ctx, db, metav1.ConditionTrue, "Ready")
		omitted := connectionAPIOutput(ctx, db).DeepCopy()
		remainingKey := client.ObjectKey{Namespace: db.Namespace, Name: remaining.Name}
		current := &corev1.Secret{}
		Expect(environment.Client.Get(ctx, remainingKey, current)).To(Succeed())
		Expect(bytes.Equal(current.Data[passwordKey], source.Data[passwordKey])).To(BeTrue())
		remainingUID := current.UID

		source.Data[passwordKey] = []byte("current-source-during-independent-cleanup")
		Expect(environment.Client.Update(ctx, source)).To(Succeed())
		connectionAPIApplyRole(ctx, roleReconciler, role)
		Expect(environment.Client.Get(ctx, client.ObjectKeyFromObject(db), db)).To(Succeed())
		db.Spec.ConnectionSecrets = []epco.ConnectionSecret{remaining}
		Expect(environment.Client.Update(ctx, db)).To(Succeed())
		attempted := false
		reconciler.Client = interceptor.NewClient(connectionAPIWatchClient(), interceptor.Funcs{
			Delete: func(ctx context.Context, c client.WithWatch, object client.Object,
				options ...client.DeleteOption) error {
				if _, ok := object.(*corev1.Secret); ok && object.GetNamespace() == db.Namespace &&
					object.GetName() == omittedName {
					attempted = true
					return apierrors.NewServiceUnavailable("isolated omitted-output deletion failure")
				}
				return c.Delete(ctx, object, options...)
			},
		})
		_, _ = reconciler.Reconcile(ctx, connectionAPIRequestFor(db))
		Expect(attempted).To(BeTrue(), "test must reach the independent omitted-output cleanup failure")
		Expect(environment.Client.Get(ctx, remainingKey, current)).To(Succeed())
		Expect(bytes.Equal(current.Data[passwordKey], source.Data[passwordKey])).To(BeTrue(),
			"one cleanup failure must not block an independently eligible declared output refresh")
		Expect(current.UID).To(Equal(remainingUID))
		Expect(environment.Client.Get(ctx, client.ObjectKeyFromObject(omitted), &corev1.Secret{})).To(Succeed())
		latest := &epco.Database{}
		Expect(environment.Client.Get(ctx, client.ObjectKeyFromObject(db), latest)).To(Succeed())
		condition := meta.FindStatusCondition(latest.Status.Conditions, "Ready")
		Expect(condition).NotTo(BeNil())
		Expect(condition.Status).To(Equal(metav1.ConditionFalse))
		Expect(condition.ObservedGeneration).To(Equal(latest.Generation))
		Expect(connectionAPIHistoryContains(ctx, db, omittedName)).To(BeTrue(),
			"failed cleanup must keep its durable output identity for retry")

		reconciler.Client = environment.Client
		_, err = reconciler.Reconcile(ctx, connectionAPIRequestFor(db))
		Expect(err).NotTo(HaveOccurred())
		Expect(apierrors.IsNotFound(environment.Client.Get(
			ctx, client.ObjectKeyFromObject(omitted), &corev1.Secret{}))).To(BeTrue())
		Expect(environment.Client.Get(ctx, remainingKey, current)).To(Succeed())
		Expect(bytes.Equal(current.Data[passwordKey], source.Data[passwordKey])).To(BeTrue())
		expectDatabaseStatus(ctx, db, metav1.ConditionTrue, "Ready")
	}, NodeTimeout(30*time.Second))

	It("keeps a same-UID omitted output cleanup checkpoint until restored provenance permits removal",
		func(ctx SpecContext) {
			cluster, role, _ := connectionAPIFixture(ctx)
			connectionAPIApplyRole(ctx, connectionAPIRoleReconciler(), role)
			db := connectionAPIDatabase(cluster, "restored-omitted-provenance")
			Expect(scope.Create(ctx, db)).To(Succeed())
			reconciler := connectionAPIDirectReconciler(environment.Client)
			reconciler.Databases = &connectionAPIFactory{}
			_, err := reconciler.Reconcile(ctx, connectionAPIRequestFor(db))
			Expect(err).NotTo(HaveOccurred())
			expectDatabaseStatus(ctx, db, metav1.ConditionTrue, "Ready")
			original := connectionAPIOutput(ctx, db).DeepCopy()
			outputKey := client.ObjectKeyFromObject(original)
			current := original.DeepCopy()
			current.Annotations = map[string]string{"unrelated-owner": "temporarily-unsafe-provenance"}
			Expect(environment.Client.Update(ctx, current)).To(Succeed())
			Expect(current.UID).To(Equal(original.UID))
			Expect(environment.Client.Get(ctx, client.ObjectKeyFromObject(db), db)).To(Succeed())
			db.Spec.ConnectionSecrets = nil
			Expect(environment.Client.Update(ctx, db)).To(Succeed())
			_, _ = reconciler.Reconcile(ctx, connectionAPIRequestFor(db))
			Expect(environment.Client.Get(ctx, outputKey, current)).To(Succeed())
			Expect(current.UID).To(Equal(original.UID))
			Expect(bytes.Equal(current.Data[passwordKey], original.Data[passwordKey])).To(BeTrue())
			Expect(connectionAPIHistoryContains(ctx, db, original.Name)).To(BeTrue(),
				"the recorded original incarnation remains until its cleanup is confirmed")
			latest := &epco.Database{}
			Expect(environment.Client.Get(ctx, client.ObjectKeyFromObject(db), latest)).To(Succeed())
			condition := meta.FindStatusCondition(latest.Status.Conditions, "Ready")
			Expect(condition).NotTo(BeNil())
			Expect(condition.Status).To(Equal(metav1.ConditionFalse))
			Expect(condition.ObservedGeneration).To(Equal(latest.Generation))

			current.Annotations = maps.Clone(original.Annotations)
			Expect(environment.Client.Update(ctx, current)).To(Succeed())
			_, err = reconciler.Reconcile(ctx, connectionAPIRequestFor(db))
			Expect(err).NotTo(HaveOccurred())
			Expect(apierrors.IsNotFound(environment.Client.Get(ctx, outputKey, &corev1.Secret{}))).To(BeTrue())
			Expect(connectionAPIHistoryContains(ctx, db, original.Name)).To(BeFalse(),
				"confirmed original-output removal must retire its durable history")
			expectDatabaseStatus(ctx, db, metav1.ConditionTrue, "Ready")
		}, NodeTimeout(30*time.Second))
})
