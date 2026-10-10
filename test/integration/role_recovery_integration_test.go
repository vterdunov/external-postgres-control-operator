//go:build integration

package integration

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	epco "github.com/vterdunov/external-postgres-control-operator/api/v1alpha1"
	"github.com/vterdunov/external-postgres-control-operator/internal/controller"
	pgroles "github.com/vterdunov/external-postgres-control-operator/internal/postgres"
	kubeenv "github.com/vterdunov/external-postgres-control-operator/test/envtest"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const recoveryAdminSecret = "admin"

type recoveryStatusClient struct {
	client.Client
	fail   func(*epco.DatabaseRole) bool
	failed atomic.Int32
}

type recoveryStatusWriter struct {
	client.SubResourceWriter
	owner *recoveryStatusClient
}

func (c *recoveryStatusClient) Status() client.SubResourceWriter {
	return &recoveryStatusWriter{SubResourceWriter: c.Client.Status(), owner: c}
}

func (w *recoveryStatusWriter) reject(object client.Object) error {
	role, ok := object.(*epco.DatabaseRole)
	if ok && w.owner.fail != nil && w.owner.fail(role) {
		w.owner.failed.Add(1)
		return errors.New("injected role status persistence failure")
	}
	return nil
}

func (w *recoveryStatusWriter) Update(ctx context.Context, object client.Object,
	options ...client.SubResourceUpdateOption) error {
	if err := w.reject(object); err != nil {
		return err
	}
	return w.SubResourceWriter.Update(ctx, object, options...)
}

func (w *recoveryStatusWriter) Patch(ctx context.Context, object client.Object,
	patch client.Patch, options ...client.SubResourcePatchOption) error {
	if err := w.reject(object); err != nil {
		return err
	}
	return w.SubResourceWriter.Patch(ctx, object, patch, options...)
}

type recoveryRoleFactory struct {
	delegate pgroles.RoleFactory
	before   func(context.Context, pgroles.RoleDesired)
	after    func()
	applies  atomic.Int32
}

type recoveryRoleSession struct {
	pgroles.RoleSession
	owner *recoveryRoleFactory
}

func (f *recoveryRoleFactory) Open(ctx context.Context, config *pgroles.Config) (pgroles.RoleSession, error) {
	session, err := f.delegate.Open(ctx, config)
	if err != nil {
		return nil, err
	}
	return &recoveryRoleSession{RoleSession: session, owner: f}, nil
}

func (s *recoveryRoleSession) Apply(ctx context.Context,
	desired pgroles.RoleDesired) (*pgroles.RoleApplyResult, error) {
	s.owner.applies.Add(1)
	if s.owner.before != nil {
		s.owner.before(ctx, desired)
	}
	result, err := s.RoleSession.Apply(ctx, desired)
	if err == nil && s.owner.after != nil {
		s.owner.after()
	}
	return result, err
}

func recoveryCluster(ctx context.Context, scope *kubeenv.Scope, f *Fixture) *epco.PostgresCluster {
	ca, err := os.ReadFile(f.Server.TLS.CA)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	ExpectWithOffset(1, scope.Create(ctx, &corev1.Secret{Name: recoveryAdminSecret, Namespace: scope.Namespace.Name,
		Type: corev1.SecretTypeBasicAuth,
		Data: map[string][]byte{corev1.BasicAuthUsernameKey: []byte(roleSQLAdmin),
			corev1.BasicAuthPasswordKey: []byte(f.Server.password)}})).To(Succeed())
	ExpectWithOffset(1, scope.Create(ctx, &corev1.Secret{Name: "ca", Namespace: scope.Namespace.Name,
		Data: map[string][]byte{roleCAKey: ca}})).To(Succeed())
	cluster := &epco.PostgresCluster{Name: roleSQLAdmin, Namespace: scope.Namespace.Name,
		Spec: epco.PostgresClusterSpec{Connection: epco.PostgresConnection{Host: loopbackHost,
			Port: int32(f.Server.port), Database: epco.SQLName(f.Database)},
			CredentialsSecret: epco.LocalReference{Name: recoveryAdminSecret},
			TLS: &epco.PostgresTLS{Mode: roleTLSMode,
				CASecret: &epco.SecretKeyReference{Name: "ca", Key: roleCAKey}}}}
	ExpectWithOffset(1, scope.Create(ctx, cluster)).To(Succeed())
	return cluster
}

func recoveryReconciler(api client.Client, factory pgroles.RoleFactory) *controller.DatabaseRoleReconciler {
	builder, err := pgroles.NewConfigBuilder()
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	return &controller.DatabaseRoleReconciler{Client: api, APIReader: api, Builder: builder, Roles: factory,
		Locks: &controller.ManagementLocks{}}
}

func recoveryRole(ctx context.Context, api client.Client, role *epco.DatabaseRole) *epco.DatabaseRole {
	current := &epco.DatabaseRole{}
	ExpectWithOffset(1, api.Get(ctx, client.ObjectKeyFromObject(role), current)).To(Succeed())
	return current
}

func recoverUntilReady(ctx context.Context, api client.Client,
	reconciler *controller.DatabaseRoleReconciler, role *epco.DatabaseRole) *epco.DatabaseRole {
	Eventually(func() bool {
		_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(role)})
		if err != nil {
			return false
		}
		current := recoveryRole(ctx, api, role)
		condition := meta.FindStatusCondition(current.Status.Conditions, "Ready")
		return condition != nil && condition.Status == metav1.ConditionTrue &&
			condition.ObservedGeneration == current.Generation
	}).WithContext(ctx).WithTimeout(10 * time.Second).Should(BeTrue())
	return recoveryRole(ctx, api, role)
}

var _ = Describe("DatabaseRole combined API and PostgreSQL recovery", Ordered, func() {
	var environment *kubeenv.Environment
	var scope *kubeenv.Scope
	var fixture *Fixture
	var cluster *epco.PostgresCluster
	var role *epco.DatabaseRole

	BeforeAll(func(ctx SpecContext) {
		_, source, _, ok := runtime.Caller(0)
		Expect(ok).To(BeTrue())
		var err error
		environment, err = kubeenv.Start(ctx, kubeenv.Options{
			CRDDirectory:          filepath.Join(filepath.Dir(source), "..", "..", "config", "crd", "bases"),
			BinaryAssetsDirectory: os.Getenv("KUBEBUILDER_ASSETS"), Timeout: 30 * time.Second})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			Expect(environment.Stop(ctx)).To(Succeed())
		})
	}, NodeTimeout(60*time.Second))

	BeforeEach(func(ctx SpecContext) {
		var err error
		fixture, err = postgres.NewFixture(ctx)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			cleanup, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			Expect(fixture.Close(cleanup)).To(Succeed())
		})
		scope, _, err = environment.NewScope(ctx, "r1-recovery-")
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			cleanup, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			Expect(scope.Cleanup(cleanup)).To(Succeed())
		})
		cluster = recoveryCluster(ctx, scope, fixture)
		name := roleName(fixture)
		role = &epco.DatabaseRole{Name: databaseRecoveryApplicationName, Namespace: scope.Namespace.Name,
			Spec: epco.DatabaseRoleSpec{
				Name: epco.SQLName(name), Cluster: epco.ClusterReference{Name: cluster.Name},
				MembershipPolicy: epco.MembershipManaged}}
	}, NodeTimeout(30*time.Second))

	DescribeTable("recovers SQL success and status failure, then cleans withdrawn declarations after restart",
		func(ctx SpecContext, parameter string) {
			parent := roleName(fixture)
			roleSQL(ctx, fixture, "CREATE ROLE "+quoteIdentifier(parent))
			role.Spec.InRoles = []epco.SQLName{epco.SQLName(parent)}
			role.Spec.Parameters = map[string]string{parameter: roleWorkMemory}
			Expect(scope.Create(ctx, role)).To(Succeed())
			factory := &recoveryRoleFactory{delegate: pgroles.NewRoleFactory()}
			var sqlSucceeded atomic.Bool
			factory.after = func() { sqlSucceeded.Store(true) }
			fault := &recoveryStatusClient{Client: environment.Client,
				fail: func(*epco.DatabaseRole) bool { return sqlSucceeded.Load() }}
			reconciler := recoveryReconciler(fault, factory)
			Eventually(func() bool {
				_, _ = reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(role)})
				return sqlSucceeded.Load() && fault.failed.Load() > 0
			}).WithContext(ctx).WithTimeout(10 * time.Second).Should(BeTrue())
			Expect(roleMemberships(ctx, fixture, string(role.Spec.Name))).To(ConsistOf(parent))
			Expect(roleSettings(ctx, fixture, string(role.Spec.Name), false)).To(ConsistOf("work_mem=3MB"))
			current := recoveryRole(ctx, environment.Client, role)
			Expect(current.Status.HasManagedSQL).To(BeFalse())
			Expect(current.Status.ManagedEndpoints).NotTo(BeEmpty(), "cleanup intent must predate SQL")
			current.Spec.InRoles = nil
			current.Spec.Parameters = nil
			Expect(environment.Client.Update(ctx, current)).To(Succeed())
			restarted := recoveryReconciler(environment.Client, pgroles.NewRoleFactory())
			current = recoverUntilReady(ctx, environment.Client, restarted, role)
			Expect(current.Status.HasManagedSQL).To(BeTrue())
			Expect(roleMemberships(ctx, fixture, string(role.Spec.Name))).To(BeEmpty())
			Expect(roleSettings(ctx, fixture, string(role.Spec.Name), false)).To(BeEmpty())
		},
		Entry("uppercase parameter checkpoint", roleUpperWorkMemorySetting, NodeTimeout(35*time.Second)),
		Entry("legacy alias parameter checkpoint", "sort_mem", NodeTimeout(35*time.Second)),
	)

	It("prohibits SQL when generated-source initialization cannot be checkpointed", func(ctx SpecContext) {
		role.Spec.PasswordGeneration = &epco.PasswordGeneration{SecretName: "generated"}
		role.Spec.Login = true
		role.Spec.Parameters = map[string]string{roleWorkMemorySetting: roleWorkMemory}
		Expect(scope.Create(ctx, role)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			source := &corev1.Secret{}
			err := environment.Client.Get(ctx, client.ObjectKey{Name: "generated", Namespace: scope.Namespace.Name}, source)
			if apierrors.IsNotFound(err) {
				return
			}
			Expect(err).NotTo(HaveOccurred())
			Expect(environment.Client.Delete(ctx, source,
				client.Preconditions{UID: &source.UID, ResourceVersion: &source.ResourceVersion})).To(Succeed())
		})
		factory := &recoveryRoleFactory{delegate: pgroles.NewRoleFactory()}
		fault := &recoveryStatusClient{Client: environment.Client, fail: func(current *epco.DatabaseRole) bool {
			return current.Status.GeneratedPasswordInitialized
		}}
		reconciler := recoveryReconciler(fault, factory)
		Eventually(func() bool {
			_, _ = reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(role)})
			return fault.failed.Load() > 0
		}).WithContext(ctx).WithTimeout(10 * time.Second).Should(BeTrue())
		Expect(factory.applies.Load()).To(BeZero())
		Expect(roleExists(ctx, fixture, string(role.Spec.Name))).To(BeFalse())
	}, NodeTimeout(25*time.Second))

	It("prohibits SQL when membership and parameter cleanup intent cannot be checkpointed", func(ctx SpecContext) {
		role.Spec.Parameters = map[string]string{roleWorkMemorySetting: roleWorkMemory}
		Expect(scope.Create(ctx, role)).To(Succeed())
		factory := &recoveryRoleFactory{delegate: pgroles.NewRoleFactory()}
		fault := &recoveryStatusClient{Client: environment.Client, fail: func(current *epco.DatabaseRole) bool {
			return len(current.Status.ManagedEndpoints) > 0
		}}
		reconciler := recoveryReconciler(fault, factory)
		Eventually(func() bool {
			_, _ = reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(role)})
			return fault.failed.Load() > 0
		}).WithContext(ctx).WithTimeout(10 * time.Second).Should(BeTrue())
		Expect(factory.applies.Load()).To(BeZero())
		Expect(roleExists(ctx, fixture, string(role.Spec.Name))).To(BeFalse())
	}, NodeTimeout(25*time.Second))

	It("records the SQL-accepted password source and converges concurrent rotation", func(ctx SpecContext) {
		role.Spec.Login = true
		role.Spec.PasswordSecret = &epco.LocalReference{Name: "source"}
		Expect(scope.Create(ctx, role)).To(Succeed())
		source := &corev1.Secret{Name: "source", Namespace: scope.Namespace.Name,
			Type: corev1.SecretTypeBasicAuth,
			Data: map[string][]byte{corev1.BasicAuthUsernameKey: []byte(role.Spec.Name),
				corev1.BasicAuthPasswordKey: []byte("password-A")}}
		Expect(scope.Create(ctx, source)).To(Succeed())
		acceptedVersion := source.ResourceVersion
		factory := &recoveryRoleFactory{delegate: pgroles.NewRoleFactory()}
		var rotated atomic.Bool
		factory.before = func(ctx context.Context, _ pgroles.RoleDesired) {
			if rotated.Swap(true) {
				return
			}
			updated := &corev1.Secret{}
			Expect(environment.Client.Get(ctx, client.ObjectKeyFromObject(source), updated)).To(Succeed())
			updated.Data[corev1.BasicAuthPasswordKey] = []byte("password-B")
			Expect(environment.Client.Update(ctx, updated)).To(Succeed())
		}
		reconciler := recoveryReconciler(environment.Client, factory)
		Eventually(func() bool {
			_, _ = reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(role)})
			return factory.applies.Load() > 0
		}).WithContext(ctx).WithTimeout(10 * time.Second).Should(BeTrue())
		current := recoveryRole(ctx, environment.Client, role)
		Expect(current.Status.SecretResourceVersion).To(Equal(acceptedVersion))
		roleSQL(ctx, fixture, "GRANT CONNECT ON DATABASE "+quoteIdentifier(fixture.Database)+
			" TO "+quoteIdentifier(string(role.Spec.Name)))
		Expect(roleLogin(ctx, fixture, string(role.Spec.Name), "password-A")).To(BeEmpty())
		Expect(roleLogin(ctx, fixture, string(role.Spec.Name), "password-B")).To(Equal("28P01"))
		current = recoverUntilReady(ctx, environment.Client, reconciler, role)
		updated := &corev1.Secret{}
		Expect(environment.Client.Get(ctx, client.ObjectKeyFromObject(source), updated)).To(Succeed())
		Expect(current.Status.SecretResourceVersion).To(Equal(updated.ResourceVersion))
		Expect(roleLogin(ctx, fixture, string(role.Spec.Name), "password-B")).To(BeEmpty())
		Expect(roleLogin(ctx, fixture, string(role.Spec.Name), "password-A")).To(Equal("28P01"))
	}, NodeTimeout(35*time.Second))

	It("preserves dormant cleanup history and cleans the selected A to B to A endpoint", func(ctx SpecContext) {
		name := string(role.Spec.Name)
		parent := roleName(fixture)
		roleSQL(ctx, fixture, "CREATE ROLE "+quoteIdentifier(parent))
		role.Spec.InRoles = []epco.SQLName{epco.SQLName(parent)}
		role.Spec.Parameters = map[string]string{roleWorkMemorySetting: roleWorkMemory}
		Expect(scope.Create(ctx, role)).To(Succeed())
		reconciler := recoveryReconciler(environment.Client, pgroles.NewRoleFactory())
		recoverUntilReady(ctx, environment.Client, reconciler, role)
		other, err := Start(ctx, Options{Version: os.Getenv("EPCO_PG_VERSION"), RunID: postgres.RunID,
			ArtifactsDir: os.Getenv("EPCO_ARTIFACTS_DIR")})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			cleanup, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			Expect(other.Close(cleanup)).To(Succeed())
		})
		otherFixture, err := other.NewFixture(ctx)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			cleanup, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			Expect(otherFixture.Close(cleanup)).To(Succeed())
		})
		// B has independent state; A's history must not be blindly applied to it.
		roleSQL(ctx, otherFixture, "CREATE ROLE "+quoteIdentifier(name))
		roleSQL(ctx, otherFixture, "CREATE ROLE "+quoteIdentifier(parent))
		roleSQL(ctx, otherFixture, "GRANT "+quoteIdentifier(parent)+" TO "+quoteIdentifier(name))
		roleSQL(ctx, otherFixture, "ALTER ROLE "+quoteIdentifier(name)+" SET work_mem TO '9MB'")
		current := recoveryRole(ctx, environment.Client, role)
		current.Spec.InRoles = nil
		current.Spec.Parameters = nil
		Expect(environment.Client.Update(ctx, current)).To(Succeed())
		setEndpoint := func(target *Fixture) {
			latest := &epco.PostgresCluster{}
			Expect(environment.Client.Get(ctx, client.ObjectKeyFromObject(cluster), latest)).To(Succeed())
			latest.Spec.Connection.Port = int32(target.Server.port)
			latest.Spec.Connection.Database = epco.SQLName(target.Database)
			Expect(environment.Client.Update(ctx, latest)).To(Succeed())
			for secretName, key := range map[string]string{recoveryAdminSecret: corev1.BasicAuthPasswordKey, "ca": roleCAKey} {
				secret := &corev1.Secret{}
				Expect(environment.Client.Get(ctx,
					client.ObjectKey{Name: secretName, Namespace: scope.Namespace.Name}, secret)).To(Succeed())
				value := []byte(target.Server.password)
				if secretName == "ca" {
					var err error
					value, err = os.ReadFile(target.Server.TLS.CA)
					Expect(err).NotTo(HaveOccurred())
				}
				secret.Data[key] = value
				Expect(environment.Client.Update(ctx, secret)).To(Succeed())
			}
		}
		setEndpoint(otherFixture)
		current = recoverUntilReady(ctx, environment.Client, reconciler, role)
		Expect(current.Status.ManagedEndpoints).To(HaveLen(2))
		Expect(roleMemberships(ctx, fixture, name)).To(ConsistOf(parent))
		Expect(roleSettings(ctx, fixture, name, false)).To(ConsistOf("work_mem=3MB"))
		Expect(roleMemberships(ctx, otherFixture, name)).To(ConsistOf(parent))
		Expect(roleSettings(ctx, otherFixture, name, false)).To(ConsistOf("work_mem=9MB"))
		setEndpoint(fixture)
		restarted := recoveryReconciler(environment.Client, pgroles.NewRoleFactory())
		recoverUntilReady(ctx, environment.Client, restarted, role)
		Expect(roleMemberships(ctx, fixture, name)).To(BeEmpty())
		Expect(roleSettings(ctx, fixture, name, false)).To(BeEmpty())
		Expect(roleMemberships(ctx, otherFixture, name)).To(ConsistOf(parent))
		Expect(roleSettings(ctx, otherFixture, name, false)).To(ConsistOf("work_mem=9MB"))
	}, NodeTimeout(90*time.Second))

	It("retains managed grantor history across administrator changes and restart", func(ctx SpecContext) {
		name, parent, replacementAdmin := string(role.Spec.Name), roleName(fixture), roleName(fixture)
		replacementPassword, err := randomName("admin-password-")
		Expect(err).NotTo(HaveOccurred())
		roleSQL(ctx, fixture, "ALTER ROLE "+quoteIdentifier(fixture.Role)+" CREATEROLE")
		roleSQL(ctx, fixture, "CREATE ROLE "+quoteIdentifier(replacementAdmin)+
			" LOGIN CREATEROLE PASSWORD "+quoteLiteral(replacementPassword))
		roleSQL(ctx, fixture, "GRANT CONNECT ON DATABASE "+quoteIdentifier(fixture.Database)+
			" TO "+quoteIdentifier(replacementAdmin))
		for _, sqlRole := range []string{name, parent} {
			roleSQL(ctx, fixture, "CREATE ROLE "+quoteIdentifier(sqlRole))
			for _, admin := range []string{fixture.Role, replacementAdmin} {
				roleSQL(ctx, fixture, "GRANT "+quoteIdentifier(sqlRole)+
					" TO "+quoteIdentifier(admin)+" WITH ADMIN OPTION")
			}
		}
		setAdministrator := func(username, password string) {
			secret := &corev1.Secret{}
			Expect(environment.Client.Get(ctx,
				client.ObjectKey{Name: recoveryAdminSecret, Namespace: scope.Namespace.Name}, secret)).To(Succeed())
			secret.Data[corev1.BasicAuthUsernameKey] = []byte(username)
			secret.Data[corev1.BasicAuthPasswordKey] = []byte(password)
			Expect(environment.Client.Update(ctx, secret)).To(Succeed())
		}
		setAdministrator(fixture.Role, fixture.Password)
		role.Spec.InRoles = []epco.SQLName{epco.SQLName(parent)}
		Expect(scope.Create(ctx, role)).To(Succeed())
		reconciler := recoveryReconciler(environment.Client, pgroles.NewRoleFactory())
		current := recoverUntilReady(ctx, environment.Client, reconciler, role)
		binding := epco.RoleMembershipBinding{Role: parent, Member: name, Grantor: fixture.Role}
		Expect(current.Status.ManagedEndpoints).To(HaveLen(1))
		Expect(current.Status.ManagedEndpoints[0].Memberships).To(ContainElement(binding))
		setAdministrator(replacementAdmin, replacementPassword)
		current = recoverUntilReady(ctx, environment.Client, reconciler, role)
		Expect(roleMemberships(ctx, fixture, name)).To(ConsistOf(parent))
		var oldGrantorBindings int
		Expect(fixture.Admin.QueryRowContext(ctx, `SELECT count(*) FROM pg_auth_members
			WHERE roleid=(SELECT oid FROM pg_roles WHERE rolname=$1)
			AND member=(SELECT oid FROM pg_roles WHERE rolname=$2)
			AND grantor=(SELECT oid FROM pg_roles WHERE rolname=$3)`, parent, name, fixture.Role).
			Scan(&oldGrantorBindings)).To(Succeed())
		Expect(oldGrantorBindings).To(Equal(1))
		Expect(current.Status.ManagedEndpoints[0].Memberships).To(ContainElement(binding),
			"changing connection credentials must not forget a managed old-grantor binding")
		current.Spec.InRoles = nil
		Expect(environment.Client.Update(ctx, current)).To(Succeed())
		restarted := recoveryReconciler(environment.Client, pgroles.NewRoleFactory())
		_, err = restarted.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(role)})
		Expect(err).NotTo(HaveOccurred())
		current = recoveryRole(ctx, environment.Client, role)
		condition := meta.FindStatusCondition(current.Status.Conditions, "Ready")
		Expect(condition).NotTo(BeNil())
		Expect(condition.ObservedGeneration).To(Equal(current.Generation))
		if len(roleMemberships(ctx, fixture, name)) > 0 {
			Expect(condition.Status).To(Equal(metav1.ConditionFalse), "uncleaned old-grantor state must prevent readiness")
			Expect(current.Status.ManagedEndpoints[0].Memberships).To(ContainElement(binding))
		} else {
			Expect(condition.Status).To(Equal(metav1.ConditionTrue))
		}
		setAdministrator(fixture.Role, fixture.Password)
		current = recoverUntilReady(ctx, environment.Client,
			recoveryReconciler(environment.Client, pgroles.NewRoleFactory()), role)
		Expect(roleMemberships(ctx, fixture, name)).To(BeEmpty())
		Expect(current.Status.ManagedEndpoints[0].Memberships).NotTo(ContainElement(binding))
	}, NodeTimeout(35*time.Second))
})
