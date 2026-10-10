//go:build envtest

package envtest_test

import (
	"context"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	epco "github.com/vterdunov/external-postgres-control-operator/api/v1alpha1"
	"github.com/vterdunov/external-postgres-control-operator/internal/controller"
	"github.com/vterdunov/external-postgres-control-operator/internal/postgres"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// This bounded SQL model injects child failure after confirmed database state.
// API resourceVersions, generation, caches, watches and finalizers are real.
const extensionAPIPending = "epco_pending_extension"

type extensionAPIFactory struct {
	connectionAPIFactory
	extensionMu sync.Mutex
	installed   map[string]map[string]postgres.ExtensionState
}

type extensionAPISession struct {
	postgres.DatabaseSession
	factory *extensionAPIFactory
	host    string
}

func (f *extensionAPIFactory) Open(ctx context.Context, config *postgres.Config) (postgres.DatabaseSession, error) {
	session, err := f.connectionAPIFactory.Open(ctx, config)
	if err != nil {
		return nil, err
	}
	return &extensionAPISession{DatabaseSession: session, factory: f, host: config.Connection.Host}, nil
}

func (f *extensionAPIFactory) state(host, database string) postgres.ExtensionState {
	f.extensionMu.Lock()
	defer f.extensionMu.Unlock()
	return f.installed[host+"/"+database]["hstore"]
}

func (s *extensionAPISession) InspectExtensions(ctx context.Context, database string,
	specs []epco.ExtensionSpec) ([]postgres.ExtensionState, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.factory.extensionMu.Lock()
	defer s.factory.extensionMu.Unlock()
	states := make([]postgres.ExtensionState, 0, len(specs))
	for _, spec := range specs {
		state := s.factory.installed[s.host+"/"+database][string(spec.Name)]
		state.Name = string(spec.Name)
		states = append(states, state)
	}
	return states, nil
}

func (s *extensionAPISession) ReconcileExtensions(ctx context.Context, database string,
	specs []epco.ExtensionSpec) (*postgres.ExtensionResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.factory.extensionMu.Lock()
	if s.factory.installed == nil {
		s.factory.installed = make(map[string]map[string]postgres.ExtensionState)
	}
	key := s.host + "/" + database
	if s.factory.installed[key] == nil {
		s.factory.installed[key] = make(map[string]postgres.ExtensionState)
	}
	for _, spec := range specs {
		if spec.Name == extensionAPIPending {
			s.factory.extensionMu.Unlock()
			return nil, &postgres.Failure{Reason: postgres.DependenciesNotReady,
				Message: "isolated missing extension prerequisite"}
		}
		state := s.factory.installed[key][string(spec.Name)]
		state.Name = string(spec.Name)
		state.Exists = spec.Ensure != epco.EnsureAbsent
		if state.Exists {
			if state.Version == "" {
				state.Version = "1.8"
			}
			if state.Schema == "" {
				state.Schema = "public"
			}
			if spec.Version != "" {
				state.Version = spec.Version
			}
			if spec.Schema != "" {
				state.Schema = string(spec.Schema)
			}
		}
		s.factory.installed[key][state.Name] = state
	}
	s.factory.extensionMu.Unlock()
	states, err := s.InspectExtensions(ctx, database, specs)
	return &postgres.ExtensionResult{States: states}, err
}

func (s *extensionAPISession) Apply(ctx context.Context,
	desired postgres.DatabaseDesired) (*postgres.DatabaseApplyResult, error) {
	core := desired
	core.Spec = *desired.Spec.DeepCopy()
	core.Spec.Extensions = nil
	if _, err := s.DatabaseSession.Apply(ctx, core); err != nil {
		return nil, err
	}
	_, err := s.ReconcileExtensions(ctx, desired.Name, desired.Spec.Extensions)
	return &postgres.DatabaseApplyResult{BaseManaged: true}, err
}

func (s *extensionAPISession) CheckConnection(ctx context.Context, database, role string) error {
	return s.DatabaseSession.(postgres.DatabaseConnectionSession).CheckConnection(ctx, database, role)
}

func (s *extensionAPISession) PreflightACL(ctx context.Context, desired postgres.DatabaseDesired) error {
	return s.DatabaseSession.(postgres.DatabaseACLSession).PreflightACL(ctx, desired)
}

func (s *extensionAPISession) PrepareACL(ctx context.Context,
	desired postgres.ACLDesired) (*postgres.ACLResult, error) {
	return s.DatabaseSession.(postgres.DatabaseACLSession).PrepareACL(ctx, desired)
}

func (s *extensionAPISession) ReconcileACL(ctx context.Context,
	desired postgres.ACLDesired) (*postgres.ACLResult, error) {
	return s.DatabaseSession.(postgres.DatabaseACLSession).ReconcileACL(ctx, desired)
}

func extensionAPIDatabase(cluster *epco.PostgresCluster, name string) *epco.Database {
	db := watchedDatabase(cluster, name)
	db.Spec.Extensions = []epco.ExtensionSpec{{Name: "hstore", Schema: "public"}}
	return db
}

var _ = Describe("R5 Database extensions real API and watches", func() {
	It("records confirmed lifecycle while extension work is pending and reserves the SQL name", func(ctx SpecContext) {
		factory := &extensionAPIFactory{}
		startConnectionAPIManager(ctx, factory)
		cluster := createRoleDescriptor(ctx, nil)
		db := extensionAPIDatabase(cluster, "pending-extension")
		db.Spec.Extensions[0].Name = extensionAPIPending
		Expect(scope.Create(ctx, db)).To(Succeed())
		latest := expectDatabaseStatus(ctx, db, metav1.ConditionFalse, postgres.DependenciesNotReady)
		Expect(latest.Status.HasManagedSQL).To(BeTrue())
		Expect(latest.Status.AppliedGeneration).To(BeZero())
		Expect(latest.Status.LastSuccessfulReconcileTime).To(BeNil())
		other := extensionAPIDatabase(cluster, "extension-contender")
		other.Spec.Name = epco.SQLName(db.Name)
		Expect(scope.Create(ctx, other)).To(Succeed())
		expectDatabaseStatus(ctx, other, metav1.ConditionFalse, postgres.OwnershipConflict)
		Expect(factory.state(cluster.Spec.Connection.Host, db.Name).Exists).To(BeFalse())
		latest.Spec.Extensions = []epco.ExtensionSpec{{Name: "hstore"}}
		Expect(environment.Client.Update(ctx, latest)).To(Succeed())
		latest = expectDatabaseStatus(ctx, db, metav1.ConditionTrue, "Ready")
		Expect(latest.Status.AppliedGeneration).To(Equal(latest.Generation))
		Expect(factory.state(cluster.Spec.Connection.Host, db.Name).Exists).To(BeTrue())
	}, NodeTimeout(50*time.Second))

	It("reconciles extensions on endpoint changes and descriptor recreation at the same generation",
		func(ctx SpecContext) {
			factory := &extensionAPIFactory{}
			startConnectionAPIManager(ctx, factory)
			cluster := createRoleDescriptor(ctx, nil)
			db := extensionAPIDatabase(cluster, "extension-endpoint")
			Expect(scope.Create(ctx, db)).To(Succeed())
			latest := expectDatabaseStatus(ctx, db, metav1.ConditionTrue, "Ready")
			generation := latest.Generation
			oldHost := cluster.Spec.Connection.Host
			cluster.Spec.Connection.Host = "extension-second.example"
			Expect(environment.Client.Update(ctx, cluster)).To(Succeed())
			Eventually(func() bool { return factory.state("extension-second.example", db.Name).Exists }).
				WithContext(ctx).WithTimeout(10 * time.Second).Should(BeTrue())
			Expect(factory.state(oldHost, db.Name).Exists).To(BeTrue(), "old endpoint is retained")
			originalUID := cluster.UID
			Expect(environment.Client.Delete(ctx, cluster)).To(Succeed())
			Eventually(func() bool {
				return environment.Client.Get(ctx, client.ObjectKeyFromObject(cluster), &epco.PostgresCluster{}) != nil
			}).
				WithContext(ctx).WithTimeout(10 * time.Second).Should(BeTrue())
			replacement := &epco.PostgresCluster{Name: cluster.Name, Namespace: cluster.Namespace, Spec: cluster.Spec}
			replacement.Spec.Connection.Host = "extension-replacement.example"
			Expect(scope.Create(ctx, replacement)).To(Succeed())
			Expect(replacement.UID).NotTo(Equal(originalUID))
			Eventually(func() bool { return factory.state("extension-replacement.example", db.Name).Exists }).
				WithContext(ctx).WithTimeout(10 * time.Second).Should(BeTrue())
			latest = expectDatabaseStatus(ctx, db, metav1.ConditionTrue, "Ready")
			Expect(latest.Generation).To(Equal(generation))
		}, NodeTimeout(60*time.Second))

	It("publishes and rotates outputs independently of an unrelated pending extension", func(ctx SpecContext) {
		cluster, role, source := connectionAPIFixture(ctx)
		roleReconciler := connectionAPIRoleReconciler()
		connectionAPIApplyRole(ctx, roleReconciler, role)
		factory := &extensionAPIFactory{}
		startConnectionAPIManager(ctx, factory)
		db := connectionAPIDatabase(cluster, "extension-output")
		db.Spec.Extensions = []epco.ExtensionSpec{{Name: extensionAPIPending}}
		Expect(scope.Create(ctx, db)).To(Succeed())
		connectionAPIExpectPassword(ctx, db, source.Data[passwordKey])
		latest := expectDatabaseStatus(ctx, db, metav1.ConditionFalse, postgres.DependenciesNotReady)
		generation := latest.Generation
		outputUID := connectionAPIOutput(ctx, db).UID
		source.Data[passwordKey] = []byte("extension-output-rotation")
		Expect(environment.Client.Update(ctx, source)).To(Succeed())
		connectionAPIApplyRole(ctx, roleReconciler, role)
		connectionAPIExpectPassword(ctx, db, source.Data[passwordKey])
		Expect(connectionAPIOutput(ctx, db).UID).To(Equal(outputUID))
		latest = expectDatabaseStatus(ctx, db, metav1.ConditionFalse, postgres.DependenciesNotReady)
		Expect(latest.Generation).To(Equal(generation))
		Expect(latest.Status.AppliedGeneration).To(BeZero())
	}, NodeTimeout(50*time.Second))

	It("pauses extension mutation and reclaim deletion until namespace authorization returns", func(ctx SpecContext) {
		factory := &extensionAPIFactory{}
		startConnectionAPIManager(ctx, factory)
		labels := namespace.DeepCopy()
		labels.Labels = map[string]string{roleTestAccessLabel: roleTestAllowed}
		Expect(environment.Client.Update(ctx, labels)).To(Succeed())
		cluster := createRoleDescriptor(ctx, &epco.ClusterAccess{NamespaceSelector: &metav1.LabelSelector{
			MatchLabels: map[string]string{roleTestAccessLabel: roleTestAllowed}}})
		db := extensionAPIDatabase(cluster, "extension-finalizer")
		db.Spec.ReclaimPolicy = epco.ReclaimDelete
		Expect(scope.Create(ctx, db)).To(Succeed())
		db = expectDatabaseStatus(ctx, db, metav1.ConditionTrue, "Ready")
		labels.Labels = nil
		Expect(environment.Client.Update(ctx, labels)).To(Succeed())
		db = expectDatabaseStatus(ctx, db, metav1.ConditionFalse, postgres.AccessDenied)
		db.Spec.Extensions[0].Version = "denied-update"
		Expect(environment.Client.Update(ctx, db)).To(Succeed())
		expectDatabaseStatus(ctx, db, metav1.ConditionFalse, postgres.AccessDenied)
		Expect(factory.state(cluster.Spec.Connection.Host, db.Name).Version).To(Equal("1.8"))
		Expect(environment.Client.Delete(ctx, db)).To(Succeed())
		latest := expectDatabaseStatus(ctx, db, metav1.ConditionFalse, postgres.AccessDenied)
		Expect(latest.Finalizers).To(ContainElement(controller.DatabaseFinalizer))
		factory.mu.Lock()
		Expect(factory.exists[db.Name]).To(BeTrue())
		factory.mu.Unlock()
		cluster.Spec.Access = &epco.ClusterAccess{NamespaceSelector: &metav1.LabelSelector{}}
		Expect(environment.Client.Update(ctx, cluster)).To(Succeed())
		expectDatabaseGone(ctx, db)
		factory.mu.Lock()
		Expect(factory.exists[db.Name]).To(BeFalse())
		factory.mu.Unlock()
	}, NodeTimeout(60*time.Second))
})

var _ postgres.DatabaseExtensionSession = (*extensionAPISession)(nil)
