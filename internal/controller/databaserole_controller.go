package controller

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"time"

	epco "github.com/vterdunov/external-postgres-control-operator/api/v1alpha1"
	"github.com/vterdunov/external-postgres-control-operator/internal/postgres"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// RoleFinalizer protects explicit SQL cleanup responsibility.
const RoleFinalizer = "postgresql.epco.io/role-cleanup"

var sharedRoleLocks ManagementLocks

// DatabaseRoleReconciler reconciles roles using cached snapshots and optimistic checkpoints.
type DatabaseRoleReconciler struct {
	Client    client.Client
	APIReader client.Reader
	Builder   *postgres.ConfigBuilder
	Roles     postgres.RoleFactory
	Locks     *ManagementLocks
	Now       func() time.Time
}

// +kubebuilder:rbac:groups=postgresql.epco.io,resources=databaseroles,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=postgresql.epco.io,resources=databaseroles/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=postgresql.epco.io,resources=databaseroles/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;delete

func (r *DatabaseRoleReconciler) Reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	role := &epco.DatabaseRole{}
	if err := r.Client.Get(ctx, request.NamespacedName, role); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	key := CanonicalManagementKey(role.Namespace, role.Spec.Cluster, SQLObjectRole, EffectiveRoleName(role))
	locks := r.Locks
	if locks == nil {
		locks = &sharedRoleLocks
	}
	release, err := locks.Acquire(ctx, key)
	if err != nil {
		return ctrl.Result{}, err
	}
	defer release()
	cluster := &epco.ExternalPostgres{}
	if err = r.Client.Get(ctx, key.Cluster, cluster); err != nil {
		if apierrors.IsNotFound(err) {
			return r.roleStatus(ctx, role, false, postgres.DependenciesNotReady, "The referenced ExternalPostgres does not exist", 30*time.Second)
		}
		return ctrl.Result{}, err
	}
	interval := reconcileInterval(cluster)
	if err = AuthorizeNamespace(ctx, r.Client, cluster, role.Namespace); err != nil {
		return r.roleError(ctx, role, err, interval)
	}
	if !cluster.DeletionTimestamp.IsZero() {
		return r.roleStatus(ctx, role, false, postgres.DependenciesNotReady, "The ExternalPostgres is being deleted", interval)
	}
	if !role.DeletionTimestamp.IsZero() && (!controllerutil.ContainsFinalizer(role, RoleFinalizer) || role.Spec.ReclaimPolicy != epco.ReclaimDelete || !hasManagedRole(role)) {
		return r.releaseRole(ctx, role)
	}
	if role.Spec.ManagementPolicy != epco.ManagementObserve {
		reader := r.APIReader
		if reader == nil {
			reader = r.Client
		}
		owner, err := FindRoleOwnershipConflict(ctx, reader, role)
		if err != nil {
			return ctrl.Result{}, err
		}
		if owner != nil {
			return r.roleStatus(ctx, role, false, postgres.OwnershipConflict, "Another DatabaseRole has recorded management of this SQL name", interval)
		}
		if role.DeletionTimestamp.IsZero() && controllerutil.AddFinalizer(role, RoleFinalizer) {
			if err = r.Client.Update(ctx, role); err != nil {
				return roleWriteResult(err)
			}
		}
	}
	secrets, err := readSecrets(ctx, r.Client, cluster)
	if err != nil {
		return ctrl.Result{}, err
	}
	config, err := r.Builder.Build(cluster, secrets)
	if err != nil {
		return r.roleError(ctx, role, err, interval)
	}
	var password *string
	var source *corev1.Secret
	if role.DeletionTimestamp.IsZero() && role.Spec.ManagementPolicy != epco.ManagementObserve {
		if postgres.IsProtectedRole(key.SQLName, config.Connection.User) {
			return r.roleStatus(ctx, role, false, postgres.ProtectedRoleReason, "The SQL role is protected from management", interval)
		}
		password, source, err = r.rolePassword(ctx, role)
		if err != nil {
			if apierrors.IsConflict(err) || apierrors.IsAlreadyExists(err) {
				return freshCheck(), nil
			}
			return r.roleError(ctx, role, err, interval)
		}
	}
	session, err := r.Roles.Open(ctx, config)
	if err != nil {
		return r.roleError(ctx, role, err, interval)
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = session.Close(cleanup)
	}()
	if !role.DeletionTimestamp.IsZero() {
		return r.deleteRole(ctx, role, session, key.SQLName, interval)
	}
	return r.reconcileRoleSession(ctx, role, cluster, session, interval, password, source)
}

func (r *DatabaseRoleReconciler) reconcileRoleSession(ctx context.Context, role *epco.DatabaseRole, cluster *epco.ExternalPostgres, session postgres.RoleSession, interval time.Duration, password *string, source *corev1.Secret) (ctrl.Result, error) {
	name := EffectiveRoleName(role)
	state, err := session.Inspect(ctx, name)
	if err != nil {
		return r.roleError(ctx, role, err, interval)
	}
	if role.Spec.ManagementPolicy == epco.ManagementObserve {
		role.Status.Applied = nil
		if state.Exists {
			return r.roleStatus(ctx, role, true, "Observed", "The SQL role exists; no changes were made", interval)
		}
		return r.roleStatus(ctx, role, false, postgres.RoleNotFound, "The SQL role does not exist", interval)
	}
	if postgres.IsProtectedRole(name, state.AdminUser) {
		return r.roleStatus(ctx, role, false, postgres.ProtectedRoleReason, "The SQL role is protected from management", interval)
	}
	if err = r.roleDependencies(ctx, role); err != nil {
		return r.roleError(ctx, role, err, interval)
	}
	receipt := passwordApplicationReceipt(role, cluster, source)
	endpoint := roleEndpoint(role, cluster)
	index := endpointIndex(role, endpoint)
	previous := role.Status.ManagedEndpoints[index]
	desired := postgres.RoleDesired{Name: name, Spec: role.Spec, Password: password,
		TrackedMemberships: slices.Clone(previous.Memberships), TrackedParameters: slices.Clone(previous.Parameters)}
	desired.CheckpointMembership = func(ctx context.Context, binding epco.RoleMembershipBinding, pending bool) error {
		intent := &role.Status.ManagedEndpoints[index]
		if pending {
			if !slices.Contains(intent.Memberships, binding) {
				intent.Memberships = append(intent.Memberships, binding)
			}
		} else if !slices.ContainsFunc(previous.Memberships, func(old epco.RoleMembershipBinding) bool {
			return old.Role == binding.Role && old.Member == binding.Member && old.Grantor == binding.Grantor
		}) {
			intent.Memberships = slices.DeleteFunc(intent.Memberships, func(current epco.RoleMembershipBinding) bool {
				return current.Role == binding.Role && current.Member == binding.Member && current.Grantor == binding.Grantor
			})
		}
		if err := boundRoleStatus(role); err != nil {
			return err
		}
		return r.Client.Status().Update(ctx, role)
	}
	addRoleCleanupIntent(role, index, state, name)
	budget := role.DeepCopy()
	budget.Status.PasswordApplied = receipt
	if err = boundRoleStatus(budget); err != nil {
		return r.roleError(ctx, role, err, interval)
	}
	if err = r.Client.Status().Update(ctx, role); err != nil {
		return roleWriteResult(err)
	}
	result, err := session.Apply(ctx, desired)
	if err != nil {
		return r.roleError(ctx, role, err, interval)
	}
	if result == nil {
		return r.roleStatus(ctx, role, false, postgres.UnsupportedFeature, "Role reconciliation did not confirm all requested changes", interval)
	}
	role.Status.ManagedEndpoints[index].Memberships = result.Memberships
	role.Status.ManagedEndpoints[index].Parameters = result.Parameters
	role.Status.PasswordApplied = receipt
	role.Status.SecretResourceVersion = ""
	if receipt != nil {
		role.Status.SecretResourceVersion = receipt.SourceResourceVersion
	}
	role.Status.HasManagedSQL = true
	role.Status.AppliedGeneration = role.Generation
	role.Status.LastSuccessfulReconcileTime = new(metav1.NewTime(r.now()))
	return r.roleStatus(ctx, role, true, "Ready", "The SQL role and requested password are reconciled", interval)
}

func addRoleCleanupIntent(role *epco.DatabaseRole, index int, state *postgres.RoleState, name string) {
	intent := &role.Status.ManagedEndpoints[index]
	if !state.Exists && state.ServerVersion >= 160000 && state.BootstrapUser != "" {
		binding := epco.RoleMembershipBinding{Role: name, Member: state.AdminUser, Grantor: state.BootstrapUser, AdminOption: true, Administrative: true}
		if !slices.Contains(intent.Memberships, binding) {
			intent.Memberships = append(intent.Memberships, binding)
		}
	}
	for _, binding := range state.Memberships {
		if binding.Administrative && !slices.Contains(intent.Memberships, binding) {
			intent.Memberships = append(intent.Memberships, binding)
		}
	}
	for name := range maps.Keys(role.Spec.Parameters) {
		name = postgres.CanonicalRoleParameterName(name, state.ServerVersion)
		if !slices.Contains(intent.Parameters, name) {
			intent.Parameters = append(intent.Parameters, name)
		}
	}
	slices.Sort(intent.Parameters)
}

func roleEndpoint(_ *epco.DatabaseRole, cluster *epco.ExternalPostgres) epco.RoleEndpointState {
	port := cluster.Spec.Connection.Port
	if port == 0 {
		port = 5432
	}
	return epco.RoleEndpointState{Host: strings.TrimSuffix(strings.ToLower(strings.TrimSpace(cluster.Spec.Connection.Host)), "."), Port: port}
}

func endpointIndex(role *epco.DatabaseRole, endpoint epco.RoleEndpointState) int {
	for i, current := range role.Status.ManagedEndpoints {
		if current.Host == endpoint.Host && current.Port == endpoint.Port {
			return i
		}
	}
	role.Status.ManagedEndpoints = append(role.Status.ManagedEndpoints, endpoint)
	return len(role.Status.ManagedEndpoints) - 1
}

func (r *DatabaseRoleReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func roleWriteResult(err error) (ctrl.Result, error) {
	if apierrors.IsConflict(err) {
		return freshCheck(), nil
	}
	return ctrl.Result{}, client.IgnoreNotFound(err)
}

func (r *DatabaseRoleReconciler) roleStatus(ctx context.Context, role *epco.DatabaseRole, ready bool, reason, message string, interval time.Duration) (ctrl.Result, error) {
	role.Status.ObservedGeneration = role.Generation
	role.Status.EffectiveName = EffectiveRoleName(role)
	role.Status.Message = message
	if role.Spec.ManagementPolicy == epco.ManagementObserve {
		role.Status.Applied = nil
	} else {
		role.Status.Applied = new(ready)
	}
	condition := metav1.Condition{Type: readyCondition, Status: metav1.ConditionFalse, Reason: reason, Message: message,
		ObservedGeneration: role.Generation, LastTransitionTime: metav1.NewTime(r.now())}
	if ready {
		condition.Status = metav1.ConditionTrue
	}
	meta.SetStatusCondition(&role.Status.Conditions, condition)
	if err := boundRoleStatus(role); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.Client.Status().Update(ctx, role); err != nil {
		return roleWriteResult(err)
	}
	if !ready {
		interval = min(interval, 30*time.Second)
	}
	return ctrl.Result{RequeueAfter: interval}, nil
}

func (r *DatabaseRoleReconciler) roleError(ctx context.Context, role *epco.DatabaseRole, err error, interval time.Duration) (ctrl.Result, error) {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return ctrl.Result{}, err
	}
	failure, ok := errors.AsType[*postgres.Failure](err)
	if !ok {
		return ctrl.Result{}, err
	}
	message := "PostgreSQL role reconciliation could not complete"
	switch failure.Reason {
	case postgres.AccessDenied:
		message = "The resource namespace is not authorized by ExternalPostgres"
	case "InvalidSecret":
		message = "The local password or connection Secret is invalid"
	case "SecretNotFound":
		message = "A referenced local Secret does not exist"
	case postgres.CredentialsMissing:
		message = "The initialized generated password source is missing"
	case postgres.ConnectionSecretConflict:
		message = "The generated password source has conflicting provenance"
	case postgres.DependenciesNotReady:
		message = "A required role, connection or credential operation is pending"
	case postgres.StatusSizeExceeded:
		message = failure.Message
	case postgres.InsufficientPrivileges:
		message = "The administrative connection lacks required SQL privileges"
	case postgres.DeletionBlocked:
		message = "SQL dependencies prevent role deletion"
	case "NotWritable":
		message = "PostgreSQL is read-only"
	case "InvalidConfiguration":
		message = "The requested SQL or connection configuration is invalid"
	case postgres.UnsupportedFeature:
		message = "A requested role feature is not implemented"
	case postgres.ProtectedRoleReason:
		message = "The SQL role is protected from management"
	}
	return r.roleStatus(ctx, role, false, failure.Reason, message, interval)
}

func (r *DatabaseRoleReconciler) roleDependencies(ctx context.Context, role *epco.DatabaseRole) error {
	if len(role.Spec.InRoles) == 0 {
		return nil
	}
	roles := &epco.DatabaseRoleList{}
	if err := r.Client.List(ctx, roles); err != nil {
		return err
	}
	key := CanonicalManagementKey(role.Namespace, role.Spec.Cluster, SQLObjectRole, "").Cluster
	recorded := make(map[string]bool)
	for _, other := range roles.Items {
		if CanonicalManagementKey(other.Namespace, other.Spec.Cluster, SQLObjectRole, "").Cluster == key && hasManagedRole(&other) {
			recorded[EffectiveRoleName(&other)] = true
		}
	}
	for _, other := range roles.Items {
		if other.UID == role.UID || CanonicalManagementKey(other.Namespace, other.Spec.Cluster, SQLObjectRole, "").Cluster != key || !slices.Contains(role.Spec.InRoles, epco.SQLName(EffectiveRoleName(&other))) {
			continue
		}
		if recorded[EffectiveRoleName(&other)] && !hasManagedRole(&other) {
			continue
		}
		if !hasManagedRole(&other) && other.Spec.ManagementPolicy == epco.ManagementObserve {
			continue
		}
		condition := meta.FindStatusCondition(other.Status.Conditions, "Ready")
		if !other.DeletionTimestamp.IsZero() || condition == nil || condition.Status != metav1.ConditionTrue || condition.ObservedGeneration != other.Generation {
			return &postgres.Failure{Reason: postgres.DependenciesNotReady, Message: "A managed parent SQL role is not ready"}
		}
	}
	return nil
}

func (r *DatabaseRoleReconciler) releaseRole(ctx context.Context, role *epco.DatabaseRole) (ctrl.Result, error) {
	if !controllerutil.RemoveFinalizer(role, RoleFinalizer) {
		return ctrl.Result{}, nil
	}
	return roleWriteResult(r.Client.Update(ctx, role))
}

func (r *DatabaseRoleReconciler) deleteRole(ctx context.Context, role *epco.DatabaseRole, session postgres.RoleSession, name string, interval time.Duration) (ctrl.Result, error) {
	if err := session.Drop(ctx, name); err != nil {
		return r.roleError(ctx, role, err, interval)
	}
	if err := r.deleteGeneratedSource(ctx, role); err != nil {
		return roleWriteResult(err)
	}
	return r.releaseRole(ctx, role)
}
