package controller_test

import (
	"context"
	"testing"

	"github.com/vterdunov/external-postgres-control-operator/internal/postgres"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestDatabaseOwnerSetPrerequisiteReachesSafeStatus(t *testing.T) {
	const message = "The administrative connection requires SET ROLE authority for the requested database owner"
	db := ownershipDatabase(testOrdersNamespace, dbTestApplicationName, dbTestResourceUID)
	r, cl, session := databaseFixture(t, db)
	session.state.Exists = false
	session.apply = func(context.Context, postgres.DatabaseDesired) (*postgres.DatabaseApplyResult, error) {
		return nil, &postgres.Failure{Reason: postgres.InsufficientPrivileges, Message: message}
	}
	result := runDatabase(t, r, db)
	current := currentDatabase(t, cl, db)
	requireDatabaseCondition(t, current, metav1.ConditionFalse, postgres.InsufficientPrivileges)
	ready := meta.FindStatusCondition(current.Status.Conditions, testReadyCondition)
	if current.Status.Message != message || ready.Message != message {
		t.Fatalf("owner SET prerequisite lost: status=%q Ready=%q", current.Status.Message, ready.Message)
	}
	if current.Status.HasManagedSQL || current.Status.AppliedGeneration != 0 || result.RequeueAfter <= 0 {
		t.Fatal("missing owner SET authority must remain pending without recorded SQL success")
	}
	if session.closed != 1 {
		t.Fatal("missing owner SET authority leaked SQL session")
	}
}
