// Package envtest provides an isolated real Kubernetes API for API and controller tests.
// It does not start workload, namespace, or garbage collection controllers.
package envtest

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	epco "github.com/vterdunov/external-postgres-control-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	controllerconfig "sigs.k8s.io/controller-runtime/pkg/config"
	controlplane "sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

const DefaultTimeout = 20 * time.Second

type Options struct {
	CRDDirectory          string
	BinaryAssetsDirectory string
	Timeout               time.Duration
	Clock                 clock.Clock // For reconcilers under test; lifecycle deadlines use wall time.
}

type Environment struct {
	Config *rest.Config
	Scheme *runtime.Scheme
	Client client.Client // Direct, uncached API client.
	Clock  clock.Clock

	controlPlane *controlplane.Environment
	timeout      time.Duration
	managers     []*Manager
	stopOnce     sync.Once
	stopErr      error
}

// Start installs the repository CRDs in a fresh API server and etcd. ctx bounds
// startup checks; control plane process startup and shutdown have their own bounds.
func Start(ctx context.Context, options Options) (*Environment, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if options.CRDDirectory == "" {
		return nil, errors.New("envtest CRDDirectory is required")
	}
	if options.Timeout <= 0 {
		options.Timeout = DefaultTimeout
	}
	if options.Clock == nil {
		options.Clock = clock.RealClock{}
	}
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		return nil, err
	}
	if err := epco.AddToScheme(scheme); err != nil {
		return nil, err
	}
	plane := &controlplane.Environment{
		CRDDirectoryPaths: []string{options.CRDDirectory}, ErrorIfCRDPathMissing: true,
		BinaryAssetsDirectory:    options.BinaryAssetsDirectory,
		ControlPlaneStartTimeout: options.Timeout, ControlPlaneStopTimeout: options.Timeout,
	}
	config, err := plane.Start()
	if err != nil {
		return nil, errors.Join(fmt.Errorf("start envtest: %w", err), plane.Stop())
	}
	apiClient, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil || ctx.Err() != nil {
		return nil, errors.Join(err, ctx.Err(), plane.Stop())
	}
	return &Environment{
		Config: config, Scheme: scheme, Client: apiClient, Clock: options.Clock,
		controlPlane: plane, timeout: options.Timeout,
	}, nil
}

// Stop joins manager goroutines before stopping their API server. It is idempotent.
func (e *Environment) Stop(ctx context.Context) error {
	e.stopOnce.Do(func() {
		for _, v := range slices.Backward(e.managers) {
			e.stopErr = errors.Join(e.stopErr, v.Stop(ctx))
		}
		e.stopErr = errors.Join(e.stopErr, e.controlPlane.Stop())
	})
	return e.stopErr
}

type ManagerOptions struct {
	Namespace     string
	Register      func(manager.Manager) error
	CachedObjects []client.Object // Informers to establish before waiting for cache sync.
}

type Manager struct {
	Manager manager.Manager
	cancel  context.CancelFunc
	done    chan struct{}
	err     error
}

// StartManager registers test controllers before starting the cache and queues.
// API-only and manual reconcile tests do not need to call this method.
func (e *Environment) StartManager(ctx context.Context, options ManagerOptions) (*Manager, error) {
	managerOptions := manager.Options{
		Scheme: e.Scheme, Metrics: metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0", LeaderElection: false, GracefulShutdownTimeout: &e.timeout,
		// Each isolated manager intentionally registers the same production controller.
		Controller: controllerconfig.Controller{SkipNameValidation: new(true)},
	}
	if options.Namespace != "" {
		managerOptions.Cache.DefaultNamespaces = map[string]cache.Config{options.Namespace: {}}
	}
	mgr, err := manager.New(e.Config, managerOptions)
	if err != nil {
		return nil, err
	}
	if options.Register != nil {
		if err := options.Register(mgr); err != nil {
			return nil, err
		}
	}
	for _, object := range options.CachedObjects {
		if _, err := mgr.GetCache().GetInformer(ctx, object, cache.BlockUntilSynced(false)); err != nil {
			return nil, err
		}
	}
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	instance := &Manager{Manager: mgr, cancel: cancel, done: make(chan struct{})}
	e.managers = append(e.managers, instance)
	go func() {
		defer close(instance.done)
		instance.err = mgr.Start(runCtx)
	}()
	syncCtx, syncCancel := context.WithTimeout(ctx, e.timeout)
	defer syncCancel()
	if !mgr.GetCache().WaitForCacheSync(syncCtx) {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), e.timeout)
		defer stopCancel()
		return nil, errors.Join(errors.New("manager cache failed to sync before deadline"), instance.Stop(stopCtx))
	}
	select {
	case <-instance.done:
		return nil, errors.Join(errors.New("manager exited during startup"), instance.err)
	default:
		return instance, nil
	}
}

func (m *Manager) Stop(ctx context.Context) error {
	m.cancel()
	select {
	case <-m.done:
		return m.err
	case <-ctx.Done():
		return fmt.Errorf("wait for manager shutdown: %w", ctx.Err())
	}
}
