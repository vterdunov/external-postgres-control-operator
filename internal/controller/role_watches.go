package controller

import (
	"context"
	"fmt"

	epco "github.com/vterdunov/external-postgres-control-operator/api/v1alpha1"
	"github.com/vterdunov/external-postgres-control-operator/internal/postgres"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	roleClusterIndex     = "spec.roleCanonicalCluster"
	roleSourceIndex      = "spec.rolePasswordSource"
	roleParentIndex      = "spec.roleParentDependency"
	roleAdminSecretIndex = "spec.roleAdministrativeSecret"
)

func roleClusterKey(role *epco.DatabaseRole) string {
	key := CanonicalManagementKey(role.Namespace, role.Spec.Cluster, SQLObjectRole, "")
	return key.Cluster.String()
}

// SetupWithManager registers cached indexes and input/dependency/lifecycle watches.
func (r *DatabaseRoleReconciler) SetupWithManager(manager ctrl.Manager) error {
	if r.Client == nil {
		r.Client = manager.GetClient()
	}
	if r.APIReader == nil {
		r.APIReader = manager.GetAPIReader()
	}
	if r.Builder == nil {
		var err error
		r.Builder, err = postgres.NewConfigBuilder()
		if err != nil {
			return err
		}
	}
	if r.Roles == nil {
		r.Roles = postgres.NewRoleFactory()
	}
	if r.Locks == nil {
		r.Locks = &sharedRoleLocks
	}
	for _, index := range []struct {
		object  client.Object
		name    string
		extract client.IndexerFunc
	}{
		{&epco.DatabaseRole{}, roleClusterIndex, func(object client.Object) []string { return []string{roleClusterKey(object.(*epco.DatabaseRole))} }},
		{&epco.DatabaseRole{}, roleSourceIndex, func(object client.Object) []string {
			role := object.(*epco.DatabaseRole)
			if role.Spec.PasswordGeneration != nil {
				return []string{role.Spec.PasswordGeneration.SecretName}
			}
			if role.Spec.PasswordSecret != nil {
				return []string{role.Spec.PasswordSecret.Name}
			}
			return nil
		}},
		{&epco.DatabaseRole{}, roleParentIndex, func(object client.Object) []string {
			role := object.(*epco.DatabaseRole)
			keys := make([]string, 0, len(role.Spec.InRoles))
			for _, parent := range role.Spec.InRoles {
				keys = append(keys, roleClusterKey(role)+"/"+string(parent))
			}
			return keys
		}},
		{&epco.PostgresCluster{}, roleAdminSecretIndex, func(object client.Object) []string { return secretReferences(object.(*epco.PostgresCluster)) }},
	} {
		if err := manager.GetFieldIndexer().IndexField(context.Background(), index.object, index.name, index.extract); err != nil {
			return err
		}
	}
	primary := predicate.Funcs{UpdateFunc: func(e event.UpdateEvent) bool {
		return e.ObjectOld.GetGeneration() != e.ObjectNew.GetGeneration() ||
			!equality.Semantic.DeepEqual(e.ObjectOld.GetDeletionTimestamp(), e.ObjectNew.GetDeletionTimestamp()) ||
			!equality.Semantic.DeepEqual(e.ObjectOld.GetFinalizers(), e.ObjectNew.GetFinalizers())
	}}
	dependencies := predicate.Funcs{UpdateFunc: func(e event.UpdateEvent) bool {
		before, after := e.ObjectOld.(*epco.DatabaseRole), e.ObjectNew.(*epco.DatabaseRole)
		return before.Generation != after.Generation || before.Status.HasManagedSQL != after.Status.HasManagedSQL ||
			before.Status.AppliedGeneration != after.Status.AppliedGeneration ||
			!equality.Semantic.DeepEqual(before.DeletionTimestamp, after.DeletionTimestamp) || roleReadiness(before) != roleReadiness(after)
	}}
	return ctrl.NewControllerManagedBy(manager).
		For(&epco.DatabaseRole{}, builder.WithPredicates(primary)).
		Watches(&epco.DatabaseRole{}, handler.EnqueueRequestsFromMapFunc(r.rolesForRole), builder.WithPredicates(dependencies)).
		Watches(&epco.PostgresCluster{}, handler.EnqueueRequestsFromMapFunc(r.rolesForCluster), builder.WithPredicates(predicate.ResourceVersionChangedPredicate{})).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.rolesForSecret), builder.WithPredicates(predicate.ResourceVersionChangedPredicate{})).
		Watches(&corev1.Namespace{}, handler.EnqueueRequestsFromMapFunc(r.rolesForNamespace), builder.WithPredicates(predicate.ResourceVersionChangedPredicate{})).
		Complete(r)
}

func roleReadiness(role *epco.DatabaseRole) string {
	condition := meta.FindStatusCondition(role.Status.Conditions, "Ready")
	if condition == nil {
		return ""
	}
	return fmt.Sprintf("%s/%d", condition.Status, condition.ObservedGeneration)
}

func (r *DatabaseRoleReconciler) roleRequests(ctx context.Context, options ...client.ListOption) []reconcile.Request {
	roles := &epco.DatabaseRoleList{}
	if err := r.Client.List(ctx, roles, options...); err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "Unable to list DatabaseRoles for dependency")
		return nil
	}
	requests := make([]reconcile.Request, 0, len(roles.Items))
	for _, role := range roles.Items {
		requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&role)})
	}
	return requests
}

func (r *DatabaseRoleReconciler) rolesForCluster(ctx context.Context, object client.Object) []reconcile.Request {
	return r.roleRequests(ctx, client.MatchingFields{roleClusterIndex: client.ObjectKeyFromObject(object).String()})
}

func (r *DatabaseRoleReconciler) rolesForNamespace(ctx context.Context, object client.Object) []reconcile.Request {
	return r.roleRequests(ctx, client.InNamespace(object.GetName()))
}

func (r *DatabaseRoleReconciler) rolesForSecret(ctx context.Context, object client.Object) []reconcile.Request {
	requests := r.roleRequests(ctx, client.InNamespace(object.GetNamespace()), client.MatchingFields{roleSourceIndex: object.GetName()})
	clusters := &epco.PostgresClusterList{}
	if err := r.Client.List(ctx, clusters, client.InNamespace(object.GetNamespace()), client.MatchingFields{roleAdminSecretIndex: object.GetName()}); err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "Unable to list PostgresClusters for administrative Secret")
		return requests
	}
	for _, cluster := range clusters.Items {
		requests = append(requests, r.rolesForCluster(ctx, &cluster)...)
	}
	return requests
}

func (r *DatabaseRoleReconciler) rolesForRole(ctx context.Context, object client.Object) []reconcile.Request {
	changed := object.(*epco.DatabaseRole)
	roles := &epco.DatabaseRoleList{}
	if err := r.Client.List(ctx, roles, client.MatchingFields{roleClusterIndex: roleClusterKey(changed)}); err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "Unable to list DatabaseRoles for ownership change")
		return nil
	}
	requests := make([]reconcile.Request, 0, len(roles.Items))
	for _, role := range roles.Items {
		if role.UID != changed.UID && EffectiveRoleName(&role) == EffectiveRoleName(changed) {
			requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&role)})
		}
	}
	return append(requests, r.roleRequests(ctx, client.MatchingFields{roleParentIndex: roleClusterKey(changed) + "/" + EffectiveRoleName(changed)})...)
}
