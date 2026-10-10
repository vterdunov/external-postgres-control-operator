package controller_test

import (
	"bytes"
	"context"
	"errors"
	"testing"

	epco "github.com/vterdunov/external-postgres-control-operator/api/v1alpha1"
	"github.com/vterdunov/external-postgres-control-operator/internal/controller"
	"github.com/vterdunov/external-postgres-control-operator/internal/postgres"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestDatabaseConnectionGeneratedSourceWaitsForOwnerInitializationAfterStatusLoss(t *testing.T) {
	db := connectionDatabase()
	role := connectionRole(db)
	role.Spec.PasswordSecret = nil
	role.Spec.PasswordGeneration = &epco.PasswordGeneration{SecretName: connectionSourceName}
	databaseReconciler, cl, _ := databaseFixture(t, db, role)
	connectionOffline(databaseReconciler)
	failure := apierrors.NewServiceUnavailable("isolated initializer status persistence failure")
	decorated := interceptor.NewClient(cl, interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, subresource string,
			object client.Object, options ...client.SubResourceUpdateOption) error {
			if current, ok := object.(*epco.DatabaseRole); ok && subresource == testStatusSubresource &&
				current.Status.GeneratedPasswordInitialized {
				return failure
			}
			return c.SubResource(subresource).Update(ctx, object, options...)
		},
	})
	sqlOpens := 0
	roleReconciler := &controller.DatabaseRoleReconciler{Client: decorated, APIReader: cl,
		Builder: databaseReconciler.Builder, Locks: &controller.ManagementLocks{},
		Roles: roleFactoryFunc(func(context.Context, *postgres.Config) (postgres.RoleSession, error) {
			sqlOpens++
			return nil, connectionOutage()
		})}
	_, err := roleReconciler.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(role)})
	if !errors.Is(err, failure) {
		t.Fatal("initializer status persistence failure was not preserved")
	}
	source := &corev1.Secret{}
	if err := cl.Get(t.Context(), client.ObjectKey{Namespace: role.Namespace, Name: connectionSourceName}, source); err != nil {
		t.Fatal("generated source must already exist at the failed initialization checkpoint")
	}
	if currentRole(t, cl, role).Status.GeneratedPasswordInitialized || sqlOpens != 0 {
		t.Fatal("failed source initialization checkpoint advanced role ownership or SQL")
	}
	runDatabase(t, databaseReconciler, db)
	connectionSecretMissing(t, cl, db)
	requireDatabaseCondition(t, currentDatabase(t, cl, db), metav1.ConditionFalse, "DependenciesNotReady")

	roleReconciler.Client = cl
	runRole(t, roleReconciler, role)
	if !currentRole(t, cl, role).Status.GeneratedPasswordInitialized {
		t.Fatal("role owner did not finish initializing the persisted source")
	}
	requireRoleCondition(t, currentRole(t, cl, role), metav1.ConditionFalse, "DependenciesNotReady")
	runDatabase(t, databaseReconciler, db)
	if !bytes.Equal(connectionSecret(t, cl, db).Data["password"], source.Data["password"]) {
		t.Fatal("initial export must reuse the initialized source while SQL remains unavailable")
	}
}
