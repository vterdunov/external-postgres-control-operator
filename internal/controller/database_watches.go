package controller

import (
	"context"

	epco "github.com/vterdunov/external-postgres-control-operator/api/v1alpha1"
	"github.com/vterdunov/external-postgres-control-operator/internal/postgres"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	databaseClusterIndex     = "spec.databaseCanonicalCluster"
	databaseOwnerIndex       = "spec.databaseOwnerDependency"
	databaseRoleOwnerIndex   = "spec.databaseRoleOwnerDependency"
	databaseAdminSecretIndex = "spec.databaseAdministrativeSecret"
)

func databaseClusterKey(database *epco.Database) string {
	return CanonicalManagementKey(database.Namespace, database.Spec.Cluster, SQLObjectDatabase, "").Cluster.String()
}

// SetupWithManager registers cached dependencies and lifecycle watches.
func (r *DatabaseReconciler) SetupWithManager(manager ctrl.Manager) error {
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
	if r.Databases == nil {
		r.Databases = postgres.NewDatabaseFactory()
	}
	if r.Locks == nil {
		r.Locks = &sharedDatabaseLocks
	}
	for _, index := range []struct {
		object  client.Object
		name    string
		extract client.IndexerFunc
	}{
		{&epco.Database{}, databaseClusterIndex, func(object client.Object) []string { return []string{databaseClusterKey(object.(*epco.Database))} }},
		{&epco.Database{}, databaseOwnerIndex, func(object client.Object) []string {
			database := object.(*epco.Database)
			names := map[string]bool{string(database.Spec.Owner): true}
			for _, schema := range database.Spec.Schemas {
				if schema.Ensure != epco.EnsureAbsent && schema.Owner != "" {
					names[string(schema.Owner)] = true
				}
			}
			keys := make([]string, 0, len(names))
			for name := range names {
				if name != "" {
					keys = append(keys, databaseClusterKey(database)+"/"+name)
				}
			}
			return keys
		}},
		{&epco.DatabaseRole{}, databaseRoleOwnerIndex, func(object client.Object) []string {
			role := object.(*epco.DatabaseRole)
			return []string{roleClusterKey(role) + "/" + EffectiveRoleName(role)}
		}},
		{&epco.PostgresCluster{}, databaseAdminSecretIndex, func(object client.Object) []string { return secretReferences(object.(*epco.PostgresCluster)) }},
	} {
		if err := manager.GetFieldIndexer().IndexField(context.Background(), index.object, index.name, index.extract); err != nil {
			return err
		}
	}
	r.ownerRoleIndexEnabled = true
	primary := predicate.Funcs{UpdateFunc: func(e event.UpdateEvent) bool {
		return e.ObjectOld.GetGeneration() != e.ObjectNew.GetGeneration() || !equality.Semantic.DeepEqual(e.ObjectOld.GetDeletionTimestamp(), e.ObjectNew.GetDeletionTimestamp()) || !equality.Semantic.DeepEqual(e.ObjectOld.GetFinalizers(), e.ObjectNew.GetFinalizers())
	}}
	ownership := predicate.Funcs{UpdateFunc: func(e event.UpdateEvent) bool {
		before, after := e.ObjectOld.(*epco.Database), e.ObjectNew.(*epco.Database)
		return before.Generation != after.Generation || before.Status.HasManagedSQL != after.Status.HasManagedSQL || !equality.Semantic.DeepEqual(before.DeletionTimestamp, after.DeletionTimestamp)
	}}
	owners := predicate.Funcs{UpdateFunc: func(e event.UpdateEvent) bool {
		before, after := e.ObjectOld.(*epco.DatabaseRole), e.ObjectNew.(*epco.DatabaseRole)
		return before.Generation != after.Generation || before.Status.HasManagedSQL != after.Status.HasManagedSQL || before.Status.AppliedGeneration != after.Status.AppliedGeneration || !equality.Semantic.DeepEqual(before.DeletionTimestamp, after.DeletionTimestamp) || roleReadiness(before) != roleReadiness(after)
	}}
	descriptors := predicate.Funcs{UpdateFunc: func(e event.UpdateEvent) bool {
		return e.ObjectOld.GetGeneration() != e.ObjectNew.GetGeneration() || !equality.Semantic.DeepEqual(e.ObjectOld.GetDeletionTimestamp(), e.ObjectNew.GetDeletionTimestamp())
	}}
	namespaces := predicate.Funcs{UpdateFunc: func(e event.UpdateEvent) bool {
		return !equality.Semantic.DeepEqual(e.ObjectOld.GetLabels(), e.ObjectNew.GetLabels())
	}}
	return ctrl.NewControllerManagedBy(manager).
		For(&epco.Database{}, builder.WithPredicates(primary)).
		Watches(&epco.Database{}, handler.EnqueueRequestsFromMapFunc(r.databasesForDatabase), builder.WithPredicates(ownership)).
		Watches(&epco.DatabaseRole{}, handler.EnqueueRequestsFromMapFunc(r.databasesForRole), builder.WithPredicates(owners)).
		Watches(&epco.PostgresCluster{}, handler.EnqueueRequestsFromMapFunc(r.databasesForCluster), builder.WithPredicates(descriptors)).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.databasesForSecret), builder.WithPredicates(predicate.ResourceVersionChangedPredicate{})).
		Watches(&corev1.Namespace{}, handler.EnqueueRequestsFromMapFunc(r.databasesForNamespace), builder.WithPredicates(namespaces)).
		Complete(r)
}

func (r *DatabaseReconciler) databaseRequests(ctx context.Context, options ...client.ListOption) []reconcile.Request {
	databases := &epco.DatabaseList{}
	if err := r.Client.List(ctx, databases, options...); err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "Unable to list Databases for dependency")
		return nil
	}
	requests := make([]reconcile.Request, 0, len(databases.Items))
	for _, database := range databases.Items {
		requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&database)})
	}
	return requests
}
func (r *DatabaseReconciler) databasesForCluster(ctx context.Context, object client.Object) []reconcile.Request {
	return r.databaseRequests(ctx, client.MatchingFields{databaseClusterIndex: client.ObjectKeyFromObject(object).String()})
}
func (r *DatabaseReconciler) databasesForNamespace(ctx context.Context, object client.Object) []reconcile.Request {
	return r.databaseRequests(ctx, client.InNamespace(object.GetName()))
}
func (r *DatabaseReconciler) databasesForRole(ctx context.Context, object client.Object) []reconcile.Request {
	role := object.(*epco.DatabaseRole)
	return r.databaseRequests(ctx, client.MatchingFields{databaseOwnerIndex: roleClusterKey(role) + "/" + EffectiveRoleName(role)})
}
func (r *DatabaseReconciler) databasesForDatabase(ctx context.Context, object client.Object) []reconcile.Request {
	changed := object.(*epco.Database)
	databases := &epco.DatabaseList{}
	if err := r.Client.List(ctx, databases, client.MatchingFields{databaseClusterIndex: databaseClusterKey(changed)}); err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "Unable to list Databases for ownership change")
		return nil
	}
	requests := make([]reconcile.Request, 0, len(databases.Items))
	for _, database := range databases.Items {
		if database.UID != changed.UID && EffectiveDatabaseName(&database) == EffectiveDatabaseName(changed) {
			requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&database)})
		}
	}
	return requests
}
func (r *DatabaseReconciler) databasesForSecret(ctx context.Context, object client.Object) []reconcile.Request {
	clusters := &epco.PostgresClusterList{}
	if err := r.Client.List(ctx, clusters, client.InNamespace(object.GetNamespace()), client.MatchingFields{databaseAdminSecretIndex: object.GetName()}); err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "Unable to list PostgresClusters for administrative Secret")
		return nil
	}
	var requests []reconcile.Request
	for _, cluster := range clusters.Items {
		requests = append(requests, r.databasesForCluster(ctx, &cluster)...)
	}
	return requests
}
