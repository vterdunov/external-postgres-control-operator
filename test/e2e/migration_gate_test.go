//go:build e2e

package e2e

import (
	"reflect"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func migrationGateJob() *batchv1.Job {
	return &batchv1.Job{Namespace: "app", Name: "application-migration", UID: types.UID("job-uid")}
}

func migrationGatePod(job *batchv1.Job, name string) *corev1.Pod {
	return &corev1.Pod{
		Namespace: job.Namespace, Name: name, UID: types.UID(name + "-uid"),
		Labels: map[string]string{
			"batch.kubernetes.io/job-name": job.Name, "job-name": job.Name,
		},
		Annotations: map[string]string{"fixture": "keep"},
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: "batch/v1", Kind: "Job", Name: job.Name, UID: job.UID, Controller: new(true),
		}},
		Spec:   corev1.PodSpec{Containers: []corev1.Container{{Name: "migration", Image: "docker.io/library/postgres:16.15-bookworm"}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

func migrationGateClient(t *testing.T, job *batchv1.Job, pods ...*corev1.Pod) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := batchv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	objects := []client.Object{job}
	for _, pod := range pods {
		objects = append(objects, pod)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
}

func migrationGateReadPod(t *testing.T, kubeClient client.Client, pod *corev1.Pod) *corev1.Pod {
	t.Helper()
	actual := &corev1.Pod{}
	if err := kubeClient.Get(t.Context(), client.ObjectKeyFromObject(pod), actual); err != nil {
		t.Fatal(err)
	}
	return actual
}

func TestInstalledRefreshMigrationGate(t *testing.T) {
	const annotation = "testing.epco.io/migration-gate"

	t.Run("updates only the Job controller's Pod metadata", func(t *testing.T) {
		job := migrationGateJob()
		owned := migrationGatePod(job, "owned")
		foreign := migrationGatePod(job, "foreign")
		foreign.OwnerReferences[0].UID = types.UID("another-job-uid")
		otherNamespace := migrationGatePod(job, "other-namespace")
		otherNamespace.Namespace = "elsewhere"
		kubeClient := migrationGateClient(t, job, owned, foreign, otherNamespace)
		beforeOwned := migrationGateReadPod(t, kubeClient, owned)
		beforeForeign := migrationGateReadPod(t, kubeClient, foreign)
		beforeOtherNamespace := migrationGateReadPod(t, kubeClient, otherNamespace)

		if err := installedRefreshMigrationGate(t.Context(), kubeClient, job, "gate-revision-2"); err != nil {
			t.Fatal(err)
		}
		actual := migrationGateReadPod(t, kubeClient, owned)
		if actual.Annotations[annotation] != "gate-revision-2" || actual.Annotations["fixture"] != "keep" || len(actual.Annotations) != 2 {
			t.Fatalf("unexpected Pod annotations: %v", actual.Annotations)
		}
		if actual.UID != beforeOwned.UID || !reflect.DeepEqual(actual.Spec, beforeOwned.Spec) ||
			!reflect.DeepEqual(actual.Labels, beforeOwned.Labels) ||
			!reflect.DeepEqual(actual.OwnerReferences, beforeOwned.OwnerReferences) ||
			!reflect.DeepEqual(actual.Status, beforeOwned.Status) {
			t.Fatal("refresh changed Pod identity, spec, unrelated metadata, or status")
		}
		if !reflect.DeepEqual(migrationGateReadPod(t, kubeClient, foreign), beforeForeign) ||
			!reflect.DeepEqual(migrationGateReadPod(t, kubeClient, otherNamespace), beforeOtherNamespace) {
			t.Fatal("refresh changed a foreign Pod")
		}
	})

	t.Run("accepts a Pod that finished immediately after the gate opened", func(t *testing.T) {
		job := migrationGateJob()
		pod := migrationGatePod(job, "finished")
		pod.Status.Phase = corev1.PodSucceeded
		kubeClient := migrationGateClient(t, job, pod)
		if err := installedRefreshMigrationGate(t.Context(), kubeClient, job, "gate-revision-3"); err != nil {
			t.Fatal(err)
		}
		actual := migrationGateReadPod(t, kubeClient, pod)
		if actual.Annotations[annotation] != "gate-revision-3" || actual.Status.Phase != corev1.PodSucceeded {
			t.Fatalf("finished Pod annotation/status: %v / %s", actual.Annotations, actual.Status.Phase)
		}
	})

	t.Run("rejects a Job without an API assigned UID", func(t *testing.T) {
		job := migrationGateJob()
		job.UID = ""
		pod := migrationGatePod(job, "missing-job-uid")
		kubeClient := migrationGateClient(t, job, pod)
		before := migrationGateReadPod(t, kubeClient, pod)
		if err := installedRefreshMigrationGate(t.Context(), kubeClient, job, "gate-revision-4"); err == nil {
			t.Fatal("expected an error for a Job without a UID")
		}
		if !reflect.DeepEqual(migrationGateReadPod(t, kubeClient, pod), before) {
			t.Fatal("Pod changed despite missing Job UID")
		}
	})

	for _, test := range []struct {
		name string
		pods func(*batchv1.Job) []*corev1.Pod
	}{
		{name: "no Pod", pods: func(*batchv1.Job) []*corev1.Pod { return nil }},
		{name: "wrong Job UID", pods: func(job *batchv1.Job) []*corev1.Pod {
			pod := migrationGatePod(job, "wrong-uid")
			pod.OwnerReferences[0].UID = types.UID("other-uid")
			return []*corev1.Pod{pod}
		}},
		{name: "wrong Job name", pods: func(job *batchv1.Job) []*corev1.Pod {
			pod := migrationGatePod(job, "wrong-name")
			pod.OwnerReferences[0].Name = "other-job"
			return []*corev1.Pod{pod}
		}},
		{name: "wrong owner kind", pods: func(job *batchv1.Job) []*corev1.Pod {
			pod := migrationGatePod(job, "wrong-kind")
			pod.OwnerReferences[0].Kind = "Deployment"
			return []*corev1.Pod{pod}
		}},
		{name: "non-controller owner", pods: func(job *batchv1.Job) []*corev1.Pod {
			pod := migrationGatePod(job, "non-controller")
			pod.OwnerReferences[0].Controller = new(false)
			return []*corev1.Pod{pod}
		}},
		{name: "multiple owned Pods", pods: func(job *batchv1.Job) []*corev1.Pod {
			return []*corev1.Pod{migrationGatePod(job, "first"), migrationGatePod(job, "second")}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			job := migrationGateJob()
			pods := test.pods(job)
			kubeClient := migrationGateClient(t, job, pods...)
			before := make([]*corev1.Pod, 0, len(pods))
			for _, pod := range pods {
				before = append(before, migrationGateReadPod(t, kubeClient, pod))
			}
			if err := installedRefreshMigrationGate(t.Context(), kubeClient, job, "gate-revision-4"); err == nil {
				t.Fatal("expected a selection error")
			}
			for i, pod := range pods {
				if !reflect.DeepEqual(migrationGateReadPod(t, kubeClient, pod), before[i]) {
					t.Fatalf("Pod %q changed despite selection error", pod.Name)
				}
			}
		})
	}
}
