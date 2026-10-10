package controller

import (
	"context"
	"errors"
	"slices"
	"time"

	epco "github.com/vterdunov/external-postgres-control-operator/api/v1alpha1"
	"github.com/vterdunov/external-postgres-control-operator/internal/postgres"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// DatabaseFinalizer preserves recorded SQL cleanup responsibility.
const DatabaseFinalizer = "postgresql.epco.io/database-cleanup"

var sharedDatabaseLocks ManagementLocks

var errDatabaseSnapshotChanged = errors.New("database snapshot changed")

// DatabaseReconciler reconciles database and embedded schema lifecycle.
type DatabaseReconciler struct {
	Client    client.Client
	APIReader client.Reader
	Builder   *postgres.ConfigBuilder
	Databases postgres.DatabaseFactory
	Locks     *ManagementLocks
	Now       func() time.Time

	// Computed field selectors are available only after manager cache setup.
	ownerRoleIndexEnabled        bool
	connectionRoleIndexesEnabled bool
}

// +kubebuilder:rbac:groups=postgresql.epco.io,resources=databases,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=postgresql.epco.io,resources=databases/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=postgresql.epco.io,resources=databases/finalizers,verbs=update
// +kubebuilder:rbac:groups=postgresql.epco.io,resources=databaseroles,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;delete

func (r *DatabaseReconciler) Reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	database := &epco.Database{}
	if err := r.Client.Get(ctx, request.NamespacedName, database); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	key := CanonicalManagementKey(database.Namespace, database.Spec.Cluster, SQLObjectDatabase, EffectiveDatabaseName(database))
	locks := r.Locks
	if locks == nil {
		locks = &sharedDatabaseLocks
	}
	release, err := locks.Acquire(ctx, key)
	if err != nil {
		return ctrl.Result{}, err
	}
	defer release()
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	fresh := &epco.Database{}
	if err = reader.Get(ctx, request.NamespacedName, fresh); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if fresh.UID != database.UID {
		return freshCheck(), nil
	}
	if CanonicalManagementKey(fresh.Namespace, fresh.Spec.Cluster, SQLObjectDatabase, EffectiveDatabaseName(fresh)) != key {
		return freshCheck(), nil
	}
	database = fresh
	cluster := &epco.PostgresCluster{}
	if err = r.Client.Get(ctx, key.Cluster, cluster); err != nil {
		if apierrors.IsNotFound(err) {
			return r.databaseStatus(ctx, database, false, postgres.DependenciesNotReady, "The referenced PostgresCluster does not exist", 30*time.Second)
		}
		return ctrl.Result{}, err
	}
	interval := reconcileInterval(cluster)
	if err = AuthorizeNamespace(ctx, r.Client, cluster, database.Namespace); err != nil {
		return r.databaseError(ctx, database, err, interval)
	}
	if !cluster.DeletionTimestamp.IsZero() {
		return r.databaseStatus(ctx, database, false, postgres.DependenciesNotReady, "The PostgresCluster is being deleted", interval)
	}
	deleting := !database.DeletionTimestamp.IsZero()
	if deleting && (!controllerutil.ContainsFinalizer(database, DatabaseFinalizer) || database.Spec.ReclaimPolicy != epco.ReclaimDelete) {
		return r.releaseDatabase(ctx, database)
	}
	if deleting && !database.Status.HasManagedSQL {
		if err = r.cleanupConnectionSecrets(ctx, database, true); err != nil {
			return r.databaseError(ctx, database, err, interval)
		}
		return r.releaseDatabase(ctx, database)
	}
	if !deleting && unsupportedDatabase(database.Spec) {
		return r.databaseStatus(ctx, database, false, postgres.UnsupportedFeature, "A requested database feature is not implemented", interval)
	}
	if deleting || database.Spec.ManagementPolicy != epco.ManagementObserve {
		owner, lookupErr := FindDatabaseOwnershipConflict(ctx, reader, database)
		if lookupErr != nil {
			return ctrl.Result{}, lookupErr
		}
		if owner != nil {
			return r.databaseStatus(ctx, database, false, postgres.OwnershipConflict, "Another Database has recorded management of this SQL name", interval)
		}
	}
	var cleanupErr error
	if managesConnectionOutputs(database) {
		cleanupErr = r.cleanupConnectionSecrets(ctx, database, false)
		if connectionCleanupInterrupted(cleanupErr) {
			return r.databaseError(ctx, database, cleanupErr, interval)
		}
	}
	secrets, err := readSecrets(ctx, r.Client, cluster)
	if err != nil {
		return ctrl.Result{}, err
	}
	config, err := r.Builder.Build(cluster, secrets)
	if err != nil {
		return r.databaseError(ctx, database, err, interval)
	}
	return r.reconcileDatabaseConfigured(ctx, database, cluster, config, secrets, interval, cleanupErr)
}

func (r *DatabaseReconciler) reconcileDatabaseConfigured(ctx context.Context, database *epco.Database, cluster *epco.PostgresCluster, config *postgres.Config, secrets map[string]*corev1.Secret, interval time.Duration, cleanupErr error) (ctrl.Result, error) {
	deleting := !database.DeletionTimestamp.IsZero()
	var inputs []connectionSecretInput
	var err error
	if managesConnectionOutputs(database) && len(database.Spec.ConnectionSecrets) > 0 {
		inputs, err = r.prepareConnectionInputs(ctx, database, cluster, config, secrets)
		if err != nil {
			return r.databaseError(ctx, database, err, interval)
		}
	}
	session, err := r.Databases.Open(ctx, config)
	if err != nil {
		return r.databaseError(ctx, database, err, interval)
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = session.Close(cleanup)
	}()
	state, err := session.Inspect(ctx, EffectiveDatabaseName(database))
	if err != nil {
		return r.databaseError(ctx, database, err, interval)
	}
	aclDesired := r.databaseACLDesired(database, cluster, config)
	if !deleting {
		if err = preflightDatabaseACL(ctx, database, session); err != nil {
			return r.databaseError(ctx, database, err, interval)
		}
	}
	connectionsReady := len(database.Spec.ConnectionSecrets) == 0
	if len(inputs) > 0 {
		connectionsReady, state, err = r.confirmConnectionState(ctx, database, cluster, session, state, &aclDesired, inputs)
		if err != nil {
			return r.databaseError(ctx, database, err, interval)
		}
	}
	result, err := r.reconcileDatabaseSession(ctx, database, session, state, interval, aclDesired, connectionsReady && cleanupErr == nil)
	if err == nil && cleanupErr != nil {
		return r.databaseError(ctx, database, cleanupErr, interval)
	}
	return result, err
}

func (r *DatabaseReconciler) reconcileDatabaseSession(ctx context.Context, database *epco.Database, session postgres.DatabaseSession, state *postgres.DatabaseState, interval time.Duration, aclDesired postgres.ACLDesired, connectionsReady bool) (ctrl.Result, error) {
	name := EffectiveDatabaseName(database)
	deleting := !database.DeletionTimestamp.IsZero()
	var err error
	if state == nil {
		return r.databaseStatus(ctx, database, false, postgres.UnsupportedFeature, "Database inspection did not confirm SQL state", interval)
	}
	if database.Spec.ManagementPolicy != epco.ManagementObserve || deleting {
		if postgres.IsProtectedDatabase(name, state.AdminDatabase) {
			return r.databaseStatus(ctx, database, false, postgres.ProtectedDatabase, "The SQL database is protected from management", interval)
		}
		if !deleting {
			for _, schema := range database.Spec.Schemas {
				if postgres.IsProtectedSchema(string(schema.Name)) {
					return r.databaseStatus(ctx, database, false, postgres.ProtectedSchema, "A SQL schema is protected from management", interval)
				}
			}
			if controllerutil.AddFinalizer(database, DatabaseFinalizer) {
				if err = r.Client.Update(ctx, database); err != nil {
					return roleWriteResult(err)
				}
			}
		}
	}
	if deleting {
		if state.Exists {
			if err = r.ensureDatabaseCurrent(ctx, database); err != nil {
				return r.databaseError(ctx, database, err, interval)
			}
			if err = session.Drop(ctx, name); err != nil {
				return r.databaseError(ctx, database, err, interval)
			}
		}
		if err = r.cleanupConnectionSecrets(ctx, database, true); err != nil {
			return r.databaseError(ctx, database, err, interval)
		}
		return r.releaseDatabase(ctx, database)
	}
	if database.Spec.ManagementPolicy == epco.ManagementObserve {
		return r.observeDatabaseACL(ctx, database, session, state, interval, aclDesired)
	}
	if database.Spec.Ensure == epco.EnsureAbsent {
		if state.Exists {
			if err = r.ensureDatabaseCurrent(ctx, database); err != nil {
				return r.databaseError(ctx, database, err, interval)
			}
			if err = session.Drop(ctx, name); err != nil {
				return r.databaseError(ctx, database, err, interval)
			}
		}
		if err = r.cleanupConnectionSecrets(ctx, database, true); err != nil {
			return r.databaseError(ctx, database, err, interval)
		}
		return r.databaseSuccess(ctx, database, interval, true)
	}
	return r.applyDatabaseACL(ctx, database, session, state, interval, aclDesired, connectionsReady)
}

func (r *DatabaseReconciler) applyDatabaseSession(ctx context.Context, database *epco.Database, session postgres.DatabaseSession, state *postgres.DatabaseState, interval time.Duration, connectionsReady bool) (ctrl.Result, error) {
	name := EffectiveDatabaseName(database)
	var err error
	// Enabling admission is a prerequisite for inspecting schemas on a previously disabled target.
	if state.Exists && !state.AllowConnections && database.Spec.AllowConnections != nil && *database.Spec.AllowConnections && len(database.Spec.Schemas) > 0 {
		if err = r.databaseOwnerDependencies(ctx, database, state, nil, false); err != nil {
			return r.databaseError(ctx, database, err, interval)
		}
		interim := database.Spec.DeepCopy()
		interim.Schemas = nil
		interim.ConnectionSecrets = nil
		if err = r.ensureDatabaseCurrent(ctx, database); err != nil {
			return r.databaseError(ctx, database, err, interval)
		}
		result, applyErr := session.Apply(ctx, postgres.DatabaseDesired{Name: name, Spec: *interim})
		if applyErr != nil {
			return r.databaseError(ctx, database, applyErr, interval)
		}
		if result == nil {
			return r.databaseStatus(ctx, database, false, postgres.UnsupportedFeature, "Database reconciliation did not confirm all requested changes", interval)
		}
		state, err = session.Inspect(ctx, name)
		if err != nil {
			return r.databaseError(ctx, database, err, interval)
		}
		if state == nil {
			return r.databaseStatus(ctx, database, false, postgres.UnsupportedFeature, "Database inspection did not confirm SQL state", interval)
		}
	}
	var schemas []postgres.SchemaState
	if state.Exists && len(database.Spec.Schemas) > 0 {
		schemas, err = session.InspectSchemas(ctx, name, database.Spec.Schemas)
		if err != nil {
			return r.databaseError(ctx, database, err, interval)
		}
	}
	if err = r.databaseOwnerDependencies(ctx, database, state, schemas, true); err != nil {
		return r.databaseError(ctx, database, err, interval)
	}
	if err = r.ensureDatabaseCurrent(ctx, database); err != nil {
		return r.databaseError(ctx, database, err, interval)
	}
	lifecycle := database.Spec.DeepCopy()
	lifecycle.ConnectionSecrets = nil
	result, err := session.Apply(ctx, postgres.DatabaseDesired{Name: name, Spec: *lifecycle})
	if err != nil {
		return r.databaseError(ctx, database, err, interval)
	}
	if result == nil {
		return r.databaseStatus(ctx, database, false, postgres.UnsupportedFeature, "Database reconciliation did not confirm all requested changes", interval)
	}
	return r.databaseSuccess(ctx, database, interval, connectionsReady)
}

func unsupportedDatabase(spec epco.DatabaseSpec) bool {
	return len(spec.Extensions) > 0 || len(spec.FDWs) > 0 || len(spec.Servers) > 0
}

func (r *DatabaseReconciler) observeDatabase(ctx context.Context, database *epco.Database, session postgres.DatabaseSession, state *postgres.DatabaseState, interval time.Duration) (ctrl.Result, error) {
	if !state.Exists {
		return r.databaseStatus(ctx, database, false, postgres.DatabaseNotFound, "The SQL database does not exist", interval)
	}
	if len(database.Spec.Schemas) > 0 {
		schemas, err := session.InspectSchemas(ctx, EffectiveDatabaseName(database), database.Spec.Schemas)
		if err != nil {
			return r.databaseError(ctx, database, err, interval)
		}
		for _, desired := range database.Spec.Schemas {
			found := slices.ContainsFunc(schemas, func(s postgres.SchemaState) bool { return s.Name == string(desired.Name) && s.Exists })
			if found != (desired.Ensure != epco.EnsureAbsent) {
				return r.databaseStatus(ctx, database, false, postgres.DependenciesNotReady, "A listed SQL schema does not match its requested presence", interval)
			}
		}
	}
	return r.databaseStatus(ctx, database, true, "Observed", "The SQL database and listed schemas were observed; no changes were made", interval)
}

func (r *DatabaseReconciler) databaseOwnerDependencies(ctx context.Context, database *epco.Database, state *postgres.DatabaseState, schemas []postgres.SchemaState, includeSchemas bool) error {
	needed := make(map[string]bool)
	if !state.Exists || state.Owner != string(database.Spec.Owner) {
		needed[string(database.Spec.Owner)] = true
	}
	if includeSchemas {
		for _, schema := range database.Spec.Schemas {
			if schema.Ensure == epco.EnsureAbsent {
				continue
			}
			i := slices.IndexFunc(schemas, func(s postgres.SchemaState) bool { return s.Name == string(schema.Name) && s.Exists })
			if i < 0 {
				owner := string(schema.Owner)
				if owner == "" {
					owner = string(database.Spec.Owner)
				}
				needed[owner] = true
			} else if schema.Owner != "" && schemas[i].Owner != string(schema.Owner) {
				needed[string(schema.Owner)] = true
			}
		}
	}
	if len(needed) == 0 {
		return nil
	}
	roles := &epco.DatabaseRoleList{}
	if r.ownerRoleIndexEnabled {
		for name := range needed {
			candidates := &epco.DatabaseRoleList{}
			if err := r.Client.List(ctx, candidates, client.MatchingFields{
				databaseRoleOwnerIndex: databaseClusterKey(database) + "/" + name,
			}); err != nil {
				return err
			}
			roles.Items = append(roles.Items, candidates.Items...)
		}
	} else if err := r.Client.List(ctx, roles); err != nil {
		return err
	}
	key := CanonicalManagementKey(database.Namespace, database.Spec.Cluster, SQLObjectDatabase, "").Cluster
	recorded := make(map[string]bool)
	for _, role := range roles.Items {
		if CanonicalManagementKey(role.Namespace, role.Spec.Cluster, SQLObjectRole, "").Cluster == key && hasManagedRole(&role) {
			recorded[EffectiveRoleName(&role)] = true
		}
	}
	for _, role := range roles.Items {
		name := EffectiveRoleName(&role)
		if !needed[name] || CanonicalManagementKey(role.Namespace, role.Spec.Cluster, SQLObjectRole, "").Cluster != key {
			continue
		}
		if !hasManagedRole(&role) && (recorded[name] || role.Spec.ManagementPolicy == epco.ManagementObserve) {
			continue
		}
		condition := meta.FindStatusCondition(role.Status.Conditions, readyCondition)
		if !role.DeletionTimestamp.IsZero() || condition == nil || condition.Status != metav1.ConditionTrue || condition.ObservedGeneration != role.Generation {
			return &postgres.Failure{Reason: postgres.DependenciesNotReady, Message: "A managed SQL owner role is not ready"}
		}
	}
	return nil
}

func (r *DatabaseReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}
func (r *DatabaseReconciler) databaseSuccess(ctx context.Context, database *epco.Database, interval time.Duration, connectionsReady bool) (ctrl.Result, error) {
	if err := ctx.Err(); err != nil {
		return ctrl.Result{}, err
	}
	database.Status.HasManagedSQL = true
	if !connectionsReady {
		return r.databaseStatus(ctx, database, false, postgres.DependenciesNotReady, "Application connection outputs are not fully reconciled", interval)
	}
	database.Status.AppliedGeneration = database.Generation
	database.Status.LastSuccessfulReconcileTime = new(metav1.NewTime(r.now()))
	return r.databaseStatus(ctx, database, true, "Ready", "The SQL database and listed schemas are reconciled", interval)
}
func (r *DatabaseReconciler) databaseStatus(ctx context.Context, database *epco.Database, ready bool, reason, message string, interval time.Duration) (ctrl.Result, error) {
	database.Status.ObservedGeneration = database.Generation
	database.Status.EffectiveName = EffectiveDatabaseName(database)
	database.Status.Message = message
	if database.Spec.ManagementPolicy == epco.ManagementObserve {
		database.Status.Applied = nil
	} else {
		database.Status.Applied = new(ready)
	}
	condition := metav1.Condition{Type: readyCondition, Status: metav1.ConditionFalse, Reason: reason, Message: message, ObservedGeneration: database.Generation, LastTransitionTime: metav1.NewTime(r.now())}
	if ready {
		condition.Status = metav1.ConditionTrue
	}
	meta.SetStatusCondition(&database.Status.Conditions, condition)
	if err := boundDatabasePermissionReports(database); err != nil {
		return roleWriteResult(err)
	}
	if err := r.ensureDatabaseCurrent(ctx, database); err != nil {
		if errors.Is(err, errDatabaseSnapshotChanged) {
			return freshCheck(), nil
		}
		return roleWriteResult(err)
	}
	if err := r.Client.Status().Update(ctx, database); err != nil {
		return roleWriteResult(err)
	}
	if !ready {
		interval = min(interval, 30*time.Second)
	}
	return ctrl.Result{RequeueAfter: interval}, nil
}
func (r *DatabaseReconciler) databaseError(ctx context.Context, database *epco.Database, err error, interval time.Duration) (ctrl.Result, error) {
	if apierrors.IsConflict(err) || apierrors.IsAlreadyExists(err) {
		return freshCheck(), nil
	}
	if errors.Is(err, errDatabaseSnapshotChanged) {
		return freshCheck(), nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return ctrl.Result{}, err
	}
	failure, ok := errors.AsType[*postgres.Failure](err)
	if !ok {
		return ctrl.Result{}, err
	}
	message := "PostgreSQL database reconciliation could not complete"
	switch failure.Reason {
	case postgres.AccessDenied:
		message = namespaceAccessDeniedMessage
	case postgres.DependenciesNotReady:
		message = "A required SQL or application credential operation is pending"
	case postgres.InsufficientPrivileges:
		message = "The administrative connection lacks required SQL privileges"
	case postgres.DeletionBlocked:
		message = "SQL dependencies prevent database or schema deletion"
	case postgres.UnsupportedFeature:
		message = "A requested database feature is unsupported"
	case postgres.InvalidSecret:
		message = "A referenced connection Secret is invalid"
	case postgres.SecretNotFound:
		message = "A referenced connection Secret does not exist"
	case postgres.ConnectionSecretConflict:
		message = "An application connection Secret has conflicting ownership or provenance"
	case postgres.CredentialsMissing:
		message = "The initialized application password source is missing"
	case postgres.InvalidConfiguration:
		message = "An application or SQL configuration is invalid"
	case postgres.ProtectedRoleReason:
		message = "Administrative credentials cannot be exported"
	case postgres.StatusSizeExceeded, postgres.ACLBindingUnresolved:
		message = failure.Message
	}
	return r.databaseStatus(ctx, database, false, failure.Reason, message, interval)
}
func (r *DatabaseReconciler) releaseDatabase(ctx context.Context, database *epco.Database) (ctrl.Result, error) {
	if !controllerutil.RemoveFinalizer(database, DatabaseFinalizer) {
		return ctrl.Result{}, nil
	}
	return roleWriteResult(r.Client.Update(ctx, database))
}

// ensureDatabaseCurrent rejects SQL/status work for an obsolete desired snapshot.
func (r *DatabaseReconciler) ensureDatabaseCurrent(ctx context.Context, database *epco.Database) error {
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	current := &epco.Database{}
	if err := reader.Get(ctx, client.ObjectKeyFromObject(database), current); err != nil {
		if apierrors.IsNotFound(err) {
			return errDatabaseSnapshotChanged
		}
		return err
	}
	if current.UID != database.UID || current.Generation != database.Generation ||
		!equality.Semantic.DeepEqual(current.Spec, database.Spec) ||
		!equality.Semantic.DeepEqual(current.DeletionTimestamp, database.DeletionTimestamp) {
		return errDatabaseSnapshotChanged
	}
	return nil
}
