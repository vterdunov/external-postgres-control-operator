// Package controller reconciles EPCO resources.
package controller

import (
	"context"
	"errors"
	"time"

	epco "github.com/vterdunov/external-postgres-control-operator/api/v1alpha1"
	"github.com/vterdunov/external-postgres-control-operator/internal/postgres"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const secretReferenceIndex = "spec.connectionSecretReferences"

const readyCondition = "Ready"

// PostgresClusterReconciler reports authentication and writable server state.
type PostgresClusterReconciler struct {
	Client    client.Client
	APIReader client.Reader
	Builder   *postgres.ConfigBuilder
	Checker   postgres.Checker
	Now       func() time.Time
}

// +kubebuilder:rbac:groups=postgresql.epco.io,resources=postgresclusters,verbs=get;list;watch
// +kubebuilder:rbac:groups=postgresql.epco.io,resources=postgresclusters/status,verbs=get;patch;update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

// Reconcile checks a snapshot, then discards results if any input changed.
func (r *PostgresClusterReconciler) Reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	cluster := &epco.PostgresCluster{}
	if err := reader.Get(ctx, request.NamespacedName, cluster); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !cluster.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}
	secrets, err := readSecrets(ctx, reader, cluster)
	if err != nil {
		return ctrl.Result{}, err
	}
	config, checkError := r.Builder.Build(cluster, secrets)
	interval := reconcileInterval(cluster)
	if checkError == nil {
		interval = config.ReconcileInterval
		checkError = r.Checker.Check(ctx, config)
	}
	latest := &epco.PostgresCluster{}
	if err = reader.Get(ctx, request.NamespacedName, latest); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !latest.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}
	if cluster.UID != latest.UID || cluster.ResourceVersion != latest.ResourceVersion ||
		cluster.Generation != latest.Generation || !equality.Semantic.DeepEqual(cluster.Spec, latest.Spec) {
		return freshCheck(), nil
	}
	currentSecrets, err := readSecrets(ctx, reader, latest)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !sameSecrets(secrets, currentSecrets) {
		return freshCheck(), nil
	}
	base := latest.DeepCopy()
	now := time.Now()
	if r.Now != nil {
		now = r.Now()
	}
	writeStatus(latest, checkError, now)
	patch := client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})
	if err = r.Client.Status().Patch(ctx, latest, patch); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		if apierrors.IsConflict(err) {
			return freshCheck(), nil
		}
		return ctrl.Result{}, err
	}
	if checkError != nil {
		interval = min(30*time.Second, interval)
	}
	return ctrl.Result{RequeueAfter: interval}, nil
}

func freshCheck() ctrl.Result { return ctrl.Result{RequeueAfter: time.Millisecond} }

func reconcileInterval(cluster *epco.PostgresCluster) time.Duration {
	interval, err := time.ParseDuration(string(cluster.Spec.ReconcileInterval))
	if err != nil || interval <= 0 {
		return 5 * time.Minute
	}
	return interval
}

func secretReferences(cluster *epco.PostgresCluster) []string {
	refs := []string{cluster.Spec.CredentialsSecret.Name}
	if config := cluster.Spec.TLS; config != nil && config.Mode != "disable" {
		if config.CASecret != nil {
			refs = append(refs, config.CASecret.Name)
		}
		if config.ClientCertificateSecret != nil {
			refs = append(refs, config.ClientCertificateSecret.Name)
		}
	}
	unique := make([]string, 0, len(refs))
	seen := make(map[string]bool, len(refs))
	for _, name := range refs {
		if name != "" && !seen[name] {
			unique = append(unique, name)
			seen[name] = true
		}
	}
	return unique
}

func readSecrets(ctx context.Context, reader client.Reader, cluster *epco.PostgresCluster) (map[string]*corev1.Secret, error) {
	refs := secretReferences(cluster)
	secrets := make(map[string]*corev1.Secret, len(refs))
	for _, name := range refs {
		secret := &corev1.Secret{}
		err := reader.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: name}, secret)
		if apierrors.IsNotFound(err) {
			secrets[name] = nil
			continue
		}
		if err != nil {
			return nil, err
		}
		secrets[name] = secret
	}
	return secrets, nil
}

func sameSecrets(previous, current map[string]*corev1.Secret) bool {
	if len(previous) != len(current) {
		return false
	}
	for name, old := range previous {
		latest, exists := current[name]
		if !exists {
			return false
		}
		if old == nil || latest == nil {
			if old != latest {
				return false
			}
			continue
		}
		if old.UID != latest.UID || old.ResourceVersion != latest.ResourceVersion {
			return false
		}
	}
	return true
}

func writeStatus(cluster *epco.PostgresCluster, err error, now time.Time) {
	applied := err == nil
	status := &cluster.Status
	status.Applied = &applied
	status.ObservedGeneration = cluster.Generation
	status.EffectiveName = ""
	reason, message := safeResult(err)
	status.Message = message
	condition := metav1.Condition{Type: readyCondition, Status: metav1.ConditionFalse, Reason: reason, Message: message,
		ObservedGeneration: cluster.Generation, LastTransitionTime: metav1.NewTime(now)}
	if applied {
		status.AppliedGeneration = cluster.Generation
		timestamp := metav1.NewTime(now)
		status.LastSuccessfulReconcileTime = &timestamp
		condition.Status = metav1.ConditionTrue
	}
	meta.SetStatusCondition(&status.Conditions, condition)
}

// Never publish error text, including text returned by an injected checker.
func safeResult(err error) (string, string) {
	if err == nil {
		return "Ready", "PostgreSQL authentication and writable state verified"
	}
	if failure, ok := errors.AsType[*postgres.Failure](err); ok {
		switch failure.Reason {
		case postgres.SecretNotFound:
			return postgres.SecretNotFound, "A referenced Secret does not exist"
		case postgres.InvalidSecret:
			return postgres.InvalidSecret, "A referenced Secret is invalid"
		case postgres.InvalidConfiguration:
			return postgres.InvalidConfiguration, "PostgreSQL connection configuration is invalid"
		case postgres.NotWritable:
			return postgres.NotWritable, "PostgreSQL is in recovery or the session is read-only"
		}
	}
	return postgres.ConnectionUnavailable, "Unable to authenticate, connect to PostgreSQL, or verify writable state"
}

// SetupWithManager registers namespace-local Secret indexes and lifecycle watches.
func (r *PostgresClusterReconciler) SetupWithManager(manager ctrl.Manager) error {
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
	if r.Checker == nil {
		r.Checker = postgres.NewChecker()
	}
	if err := manager.GetFieldIndexer().IndexField(context.Background(), &epco.PostgresCluster{},
		secretReferenceIndex, func(object client.Object) []string { return secretReferences(object.(*epco.PostgresCluster)) }); err != nil {
		return err
	}
	return ctrl.NewControllerManagedBy(manager).
		For(&epco.PostgresCluster{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.clustersForSecret),
			builder.WithPredicates(predicate.ResourceVersionChangedPredicate{})).
		Complete(r)
}

func (r *PostgresClusterReconciler) clustersForSecret(ctx context.Context, object client.Object) []reconcile.Request {
	clusters := &epco.PostgresClusterList{}
	if err := r.Client.List(ctx, clusters, client.InNamespace(object.GetNamespace()),
		client.MatchingFields{secretReferenceIndex: object.GetName()}); err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "Unable to list PostgresClusters for Secret")
		return nil
	}
	requests := make([]reconcile.Request, 0, len(clusters.Items))
	for _, cluster := range clusters.Items {
		requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&cluster)})
	}
	return requests
}
