package controller

import (
	"context"
	"sync"

	epco "github.com/vterdunov/external-postgres-control-operator/api/v1alpha1"
	"github.com/vterdunov/external-postgres-control-operator/internal/postgres"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// SQLObjectType distinguishes role and database catalogs.
type SQLObjectType string

const namespaceAccessDeniedMessage = "The resource namespace is not authorized by PostgresCluster"

const (
	SQLObjectRole     SQLObjectType = "role"
	SQLObjectDatabase SQLObjectType = "database"
)

// ManagementKey identifies one SQL object through its canonical descriptor.
type ManagementKey struct {
	Cluster    types.NamespacedName
	ObjectType SQLObjectType
	SQLName    string
}

// CanonicalManagementKey resolves namespace-local references without changing SQL case.
func CanonicalManagementKey(namespace string, reference epco.ClusterReference, kind SQLObjectType, name string) ManagementKey {
	if reference.Namespace != "" {
		namespace = reference.Namespace
	}
	return ManagementKey{Cluster: types.NamespacedName{Namespace: namespace, Name: reference.Name}, ObjectType: kind, SQLName: name}
}

// EffectiveRoleName resolves the immutable metadata-name fallback.
func EffectiveRoleName(role *epco.DatabaseRole) string {
	if role.Spec.Name != "" {
		return string(role.Spec.Name)
	}
	return role.Name
}

// AuthorizeNamespace evaluates ordinary cached Namespace labels before mutations.
func AuthorizeNamespace(ctx context.Context, reader client.Reader, cluster *epco.PostgresCluster, namespace string) error {
	denied := &postgres.Failure{Reason: postgres.AccessDenied, Message: namespaceAccessDeniedMessage}
	if cluster.Spec.Access == nil {
		if namespace == cluster.Namespace {
			return nil
		}
		return denied
	}
	if cluster.Spec.Access.NamespaceSelector == nil {
		return denied
	}
	selector, err := metav1.LabelSelectorAsSelector(cluster.Spec.Access.NamespaceSelector)
	if err != nil {
		return denied
	}
	object := &corev1.Namespace{}
	if err = reader.Get(ctx, client.ObjectKey{Name: namespace}, object); err != nil {
		if apierrors.IsNotFound(err) {
			return denied
		}
		return err
	}
	if !selector.Matches(labels.Set(object.Labels)) {
		return denied
	}
	return nil
}

func hasManagedRole(role *epco.DatabaseRole) bool {
	return role.Status.HasManagedSQL || role.Status.AppliedGeneration > 0
}

// FindRoleOwnershipConflict uses the supplied authoritative reader across namespaces.
func FindRoleOwnershipConflict(ctx context.Context, reader client.Reader, current *epco.DatabaseRole) (*epco.DatabaseRole, error) {
	roles := &epco.DatabaseRoleList{}
	if err := reader.List(ctx, roles); err != nil {
		return nil, err
	}
	key := CanonicalManagementKey(current.Namespace, current.Spec.Cluster, SQLObjectRole, EffectiveRoleName(current))
	for i := range roles.Items {
		other := &roles.Items[i]
		if other.UID == current.UID || !hasManagedRole(other) {
			continue
		}
		if CanonicalManagementKey(other.Namespace, other.Spec.Cluster, SQLObjectRole, EffectiveRoleName(other)) == key {
			return other, nil
		}
	}
	return nil, nil
}

// ManagementLocks serializes each management key and discards idle lock entries.
// Its zero value is usable. Cancellation never admits SQL work.
type ManagementLocks struct {
	mu      sync.Mutex
	entries map[ManagementKey]*managementLock
}

type managementLock struct {
	token chan struct{}
	users int
}

// Acquire returns an idempotent release function for one cancelable lock.
func (l *ManagementLocks) Acquire(ctx context.Context, key ManagementKey) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	l.mu.Lock()
	if l.entries == nil {
		l.entries = make(map[ManagementKey]*managementLock)
	}
	entry := l.entries[key]
	if entry == nil {
		entry = &managementLock{token: make(chan struct{}, 1)}
		entry.token <- struct{}{}
		l.entries[key] = entry
	}
	entry.users++
	l.mu.Unlock()
	done := func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		entry.users--
		if entry.users == 0 {
			delete(l.entries, key)
		}
	}
	select {
	case <-ctx.Done():
		done()
		return nil, ctx.Err()
	case <-entry.token:
		if err := ctx.Err(); err != nil {
			entry.token <- struct{}{}
			done()
			return nil, err
		}
		return sync.OnceFunc(func() { entry.token <- struct{}{}; done() }), nil
	}
}

// EffectiveDatabaseName resolves the immutable metadata-name fallback.
func EffectiveDatabaseName(database *epco.Database) string {
	if database.Spec.Name != "" {
		return string(database.Spec.Name)
	}
	return database.Name
}

// FindDatabaseOwnershipConflict checks recorded managers across namespaces.
func FindDatabaseOwnershipConflict(ctx context.Context, reader client.Reader, current *epco.Database) (*epco.Database, error) {
	databases := &epco.DatabaseList{}
	if err := reader.List(ctx, databases); err != nil {
		return nil, err
	}
	key := CanonicalManagementKey(current.Namespace, current.Spec.Cluster, SQLObjectDatabase, EffectiveDatabaseName(current))
	for i := range databases.Items {
		other := &databases.Items[i]
		if other.UID == current.UID || !other.Status.HasManagedSQL {
			continue
		}
		if CanonicalManagementKey(other.Namespace, other.Spec.Cluster, SQLObjectDatabase, EffectiveDatabaseName(other)) == key {
			return other, nil
		}
	}
	return nil, nil
}
