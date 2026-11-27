package controller

import (
	"testing"

	epco "github.com/vterdunov/external-postgres-control-operator/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"
)

func TestExternalPostgresDependencyUpdate(t *testing.T) {
	original := &epco.ExternalPostgres{
		Name:            "descriptor",
		Namespace:       "workload",
		UID:             "original",
		Generation:      1,
		ResourceVersion: "10",
	}

	t.Run("status heartbeat does not enqueue dependents", func(t *testing.T) {
		heartbeat := original.DeepCopy()
		heartbeat.ResourceVersion = "11"
		heartbeat.Status.LastSuccessfulReconcileTime = new(metav1.Now())
		if externalPostgresDependencyEvents.Update(event.UpdateEvent{ObjectOld: original, ObjectNew: heartbeat}) {
			t.Error("status-only update enqueued dependent SQL work")
		}
	})

	t.Run("same-key replacement enqueues dependents", func(t *testing.T) {
		// An informer may combine a deletion and recreation into one update.
		replacement := original.DeepCopy()
		replacement.UID = "replacement"
		replacement.ResourceVersion = "12"
		if !externalPostgresDependencyEvents.Update(event.UpdateEvent{ObjectOld: original, ObjectNew: replacement}) {
			t.Error("replacement descriptor did not enqueue dependent SQL work")
		}
	})
}
