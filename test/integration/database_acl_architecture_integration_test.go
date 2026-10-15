//go:build integration

package integration

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	epco "github.com/vterdunov/external-postgres-control-operator/api/v1alpha1"
	pg "github.com/vterdunov/external-postgres-control-operator/internal/postgres"
	kubeenv "github.com/vterdunov/external-postgres-control-operator/test/envtest"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	archRelation     = "architecture_target"
	archOldRelation  = "architecture_old"
	archPublicSchema = "public"
	archOwnerPause   = "injected interruption after confirmed owner SQL before ACL completion"
	archSelect       = "SELECT"
	archPendingGrant = "grant"
)

// Faults wrap public session phases and real checkpoint persistence. SQL and
// status assertion paths remain the production adapter and real API server.
type archACLFactory struct {
	delegate         pg.DatabaseFactory
	afterApply       func(context.Context, pg.DatabaseDesired) error
	beforeCheckpoint func(context.Context, []epco.ManagedACLTarget) error
	afterCheckpoint  func(context.Context, []epco.ManagedACLTarget) error
}

type archACLSession struct {
	pg.DatabaseSession
	pg.DatabaseACLSession
	factory *archACLFactory
}

func (f *archACLFactory) Open(ctx context.Context, config *pg.Config,
	options pg.DatabaseSessionOptions) (pg.DatabaseSession, error) {
	session, err := f.delegate.Open(ctx, config, options)
	if err != nil {
		return nil, err
	}
	return &archACLSession{DatabaseSession: session, DatabaseACLSession: session.(pg.DatabaseACLSession), factory: f}, nil
}

func (s *archACLSession) Apply(ctx context.Context, desired pg.DatabaseDesired) (*pg.DatabaseApplyResult, error) {
	result, err := s.DatabaseSession.Apply(ctx, desired)
	if err == nil && s.factory.afterApply != nil {
		err = s.factory.afterApply(ctx, desired)
	}
	return result, err
}

func (s *archACLSession) archDesired(desired pg.ACLDesired) pg.ACLDesired {
	persist := desired.Checkpoint
	desired.Checkpoint = func(ctx context.Context, targets []epco.ManagedACLTarget) error {
		if s.factory.beforeCheckpoint != nil {
			if err := s.factory.beforeCheckpoint(ctx, targets); err != nil {
				return err
			}
		}
		if err := persist(ctx, targets); err != nil {
			return err
		}
		if s.factory.afterCheckpoint != nil {
			return s.factory.afterCheckpoint(ctx, targets)
		}
		return nil
	}
	return desired
}

func (s *archACLSession) PrepareACL(ctx context.Context, desired pg.ACLDesired) (*pg.ACLResult, error) {
	return s.DatabaseACLSession.PrepareACL(ctx, s.archDesired(desired))
}

func (s *archACLSession) ReconcileACL(ctx context.Context, desired pg.ACLDesired) (*pg.ACLResult, error) {
	return s.DatabaseACLSession.ReconcileACL(ctx, s.archDesired(desired))
}

func archBindings(database *epco.Database) []epco.ACLBinding {
	var result []epco.ACLBinding
	for _, target := range database.Status.ManagedACLTargets {
		result = append(result, target.Bindings...)
	}
	return result
}

func archTargetBindings(targets []epco.ManagedACLTarget) []epco.ACLBinding {
	var result []epco.ACLBinding
	for _, target := range targets {
		result = append(result, target.Bindings...)
	}
	return result
}

func archCondition(database *epco.Database) *metav1.Condition {
	condition := meta.FindStatusCondition(database.Status.Conditions, "Ready")
	ExpectWithOffset(1, condition).NotTo(BeNil())
	return condition
}

func archOwner(ctx context.Context, f *r3SQLFixture) string {
	var owner string
	ExpectWithOffset(1, f.Server.Admin.QueryRowContext(ctx,
		"SELECT pg_get_userbyid(datdba) FROM pg_database WHERE datname=$1",
		string(f.database.Spec.Name)).Scan(&owner)).To(Succeed())
	return owner
}

func archFreshConnect(ctx context.Context, f *r3SQLFixture, role string) error {
	connection, err := f.Server.Open(string(f.database.Spec.Name), role, f.Password)
	if err != nil {
		return err
	}
	defer func() { ExpectWithOffset(1, connection.Close()).To(Succeed()) }()
	if err = connection.PingContext(ctx); err != nil {
		return fmt.Errorf("fresh fixture connection failed (SQLSTATE %s)", SQLState(err))
	}
	return nil
}

func archAdminSecret(ctx context.Context, f *r3SQLFixture, role string) {
	secret := &corev1.Secret{}
	key := client.ObjectKey{Namespace: f.scope.Namespace.Name, Name: recoveryAdminSecret}
	ExpectWithOffset(1, f.scope.Client.Get(ctx, key, secret)).To(Succeed())
	secret.Data[corev1.BasicAuthUsernameKey] = []byte(role)
	secret.Data[corev1.BasicAuthPasswordKey] = []byte(f.Password)
	ExpectWithOffset(1, f.scope.Client.Update(ctx, secret)).To(Succeed())
}

func archDefaultPrivilege(ctx context.Context, f *r3SQLFixture) bool {
	return f.defaultGrant(ctx, f.Role, "", "r", f.reader, archSelect)
}

var _ = Describe("R3 architecture SQL regression boundaries", Ordered, ContinueOnFailure, func() {
	var environment *kubeenv.Environment
	var fixture *r3SQLFixture

	BeforeAll(func(ctx SpecContext) {
		_, source, _, ok := runtime.Caller(0)
		Expect(ok).To(BeTrue())
		var err error
		environment, err = kubeenv.Start(ctx, kubeenv.Options{
			CRDDirectory:          filepath.Join(filepath.Dir(source), "..", "..", "config", "crd", "bases"),
			BinaryAssetsDirectory: os.Getenv("KUBEBUILDER_ASSETS"), Timeout: 30 * time.Second})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			Expect(environment.Stop(cleanup)).To(Succeed())
		})
	}, NodeTimeout(60*time.Second))

	BeforeEach(func(ctx SpecContext) {
		base, err := postgres.NewFixture(ctx)
		Expect(err).NotTo(HaveOccurred())
		scope, _, err := environment.NewScope(ctx, "r3-architecture-")
		Expect(err).NotTo(HaveOccurred())
		fixture = &r3SQLFixture{Fixture: base, scope: scope}
		DeferCleanup(func() {
			cleanup, cancel := context.WithTimeout(context.Background(), 25*time.Second)
			defer cancel()
			Expect(fixture.cleanup(cleanup)).To(Succeed())
		})
		Expect(base.Admin.QueryRowContext(ctx, "SHOW server_version_num").Scan(&fixture.version)).To(Succeed())
		roleSQL(ctx, base, "ALTER ROLE "+quoteIdentifier(base.Role)+" CREATEDB")
		fixture.reader = fixture.role(ctx)
		cluster := recoveryCluster(ctx, scope, base)
		archAdminSecret(ctx, fixture, base.Role)
		name, err := randomName("r3_architecture_")
		Expect(err).NotTo(HaveOccurred())
		fixture.database = &epco.Database{Name: "architecture", Namespace: scope.Namespace.Name,
			Spec: epco.DatabaseSpec{Name: epco.SQLName(name), Owner: epco.SQLName(base.Role),
				Cluster: epco.ClusterReference{Name: cluster.Name}, ReclaimPolicy: epco.ReclaimRetain,
				Schemas: []epco.SchemaSpec{{Name: r3Schema}}}}
		roleSQL(ctx, base, "CREATE DATABASE "+quoteIdentifier(name)+" OWNER "+quoteIdentifier(base.Role))
		fixture.target, err = base.Server.Open(name, roleSQLAdmin, base.Server.password)
		Expect(err).NotTo(HaveOccurred())
		fixture.exec(ctx, "CREATE SCHEMA app AUTHORIZATION "+quoteIdentifier(base.Role))
		Expect(scope.Create(ctx, fixture.database)).To(Succeed())
		fixture.ready(ctx)
	}, NodeTimeout(30*time.Second))

	archInterruptedOwner := func(ctx context.Context) string {
		fixture.update(ctx, func(spec *epco.DatabaseSpec) {
			spec.Permissions = &epco.DatabasePermissions{Create: r3Permissions(fixture.reader, epco.EnsurePresent)}
		})
		fixture.ready(ctx)
		ownerB := fixture.role(ctx)
		roleSQL(ctx, fixture.Fixture, "GRANT "+quoteIdentifier(ownerB)+" TO "+quoteIdentifier(fixture.Role))
		fixture.update(ctx, func(spec *epco.DatabaseSpec) { spec.Owner = epco.SQLName(ownerB) })
		var interrupted atomic.Bool
		factory := &archACLFactory{
			delegate: pg.NewDatabaseFactory(),
			afterApply: func(_ context.Context, desired pg.DatabaseDesired) error {
				if string(desired.Spec.Owner) == ownerB && interrupted.CompareAndSwap(false, true) {
					return &pg.Failure{Reason: pg.ConnectionUnavailable, Message: archOwnerPause}
				}
				return nil
			},
		}
		current := fixture.reconcile(ctx, databaseRecoveryReconciler(fixture.scope.Client, factory))
		Expect(interrupted.Load()).To(BeTrue())
		Expect(archOwner(ctx, fixture)).To(Equal(ownerB), "the first owner SQL really committed")
		Expect(permissionDatabaseGrant(ctx, fixture.Fixture,
			string(fixture.database.Spec.Name), fixture.reader, "CREATE")).To(BeTrue())
		Expect(slices.ContainsFunc(archBindings(current), func(binding epco.ACLBinding) bool {
			return binding.PreviousOwner != nil && binding.PreviousOwner.Name == fixture.Role &&
				binding.DesiredOwner != nil && binding.DesiredOwner.Name == ownerB
		})).To(BeTrue(), "the unresolved A-to-B checkpoint survives the interruption")
		return ownerB
	}

	It("AR1 resolves committed A-to-B history before C and current permission withdrawal", func(ctx SpecContext) {
		archInterruptedOwner(ctx)
		ownerC := fixture.role(ctx)
		roleSQL(ctx, fixture.Fixture, "GRANT "+quoteIdentifier(ownerC)+" TO "+quoteIdentifier(fixture.Role))
		fixture.update(ctx, func(spec *epco.DatabaseSpec) { spec.Owner = epco.SQLName(ownerC); spec.Permissions = nil })
		fixture.ready(ctx)
		current := recoveredDatabase(ctx, fixture.scope.Client, fixture.database)
		Expect(archOwner(ctx, fixture)).To(Equal(ownerC))
		Expect(permissionDatabaseGrant(ctx, fixture.Fixture,
			string(fixture.database.Spec.Name), fixture.reader, "CREATE")).To(BeFalse(),
			"known A-to-B-to-C rewrite must remove the withdrawn current-owner grant before Ready")
		Expect(archCondition(current).Status).To(Equal(metav1.ConditionTrue))
		Expect(archCondition(current).ObservedGeneration).To(Equal(current.Generation))
		Expect(current.Status.AppliedGeneration).To(Equal(current.Generation))
	}, NodeTimeout(35*time.Second))

	It("AR1 reconciles canceled desired ownership back to A without losing omission cleanup", func(ctx SpecContext) {
		archInterruptedOwner(ctx)
		fixture.update(ctx, func(spec *epco.DatabaseSpec) { spec.Owner = epco.SQLName(fixture.Role); spec.Permissions = nil })
		fixture.ready(ctx)
		Expect(archOwner(ctx, fixture)).To(Equal(fixture.Role))
		Expect(permissionDatabaseGrant(ctx, fixture.Fixture,
			string(fixture.database.Spec.Name), fixture.reader, "CREATE")).To(BeFalse())
	}, NodeTimeout(35*time.Second))

	It("AR1 prevents the next owner SQL when recovery-history persistence fails", func(ctx SpecContext) {
		ownerB := archInterruptedOwner(ctx)
		ownerC := fixture.role(ctx)
		roleSQL(ctx, fixture.Fixture, "GRANT "+quoteIdentifier(ownerC)+" TO "+quoteIdentifier(fixture.Role))
		fixture.update(ctx, func(spec *epco.DatabaseSpec) { spec.Owner = epco.SQLName(ownerC); spec.Permissions = nil })
		var rejected atomic.Bool
		factory := &archACLFactory{
			delegate: pg.NewDatabaseFactory(),
			beforeCheckpoint: func(_ context.Context, _ []epco.ManagedACLTarget) error {
				rejected.Store(true)
				return &pg.Failure{
					Reason: pg.ConnectionUnavailable, Message: "injected refusal to persist resolved owner responsibility",
				}
			},
		}
		current := fixture.reconcile(ctx, databaseRecoveryReconciler(fixture.scope.Client, factory))
		Expect(rejected.Load()).To(BeTrue())
		Expect(archOwner(ctx, fixture)).To(Equal(ownerB), "failed durable recovery prevents B-to-C SQL")
		Expect(archBindings(current)).NotTo(BeEmpty())
		Expect(archCondition(current).Status).To(Equal(metav1.ConditionFalse))
	}, NodeTimeout(35*time.Second))

	It("AR2 retires old database incarnation without revoking independent replacement ACLs", func(ctx SpecContext) {
		fixture.update(ctx, func(spec *epco.DatabaseSpec) {
			spec.Schemas = []epco.SchemaSpec{{Name: archPublicSchema,
				Permissions: &epco.SchemaPermissions{Usage: r3Permissions(fixture.reader, epco.EnsurePresent)}}}
			spec.DefaultPrivileges = []epco.DefaultPrivileges{{ForRole: epco.SQLName(fixture.Role),
				Tables: &epco.TablePermissions{Select: r3Permissions(fixture.reader, epco.EnsurePresent)}}}
		})
		fixture.ready(ctx)
		before := recoveredDatabase(ctx, fixture.scope.Client, fixture.database)
		Expect(archBindings(before)).To(HaveLen(2))
		var oldDatabaseOID, newDatabaseOID, oldSchemaOID, newSchemaOID uint32
		Expect(fixture.Server.Admin.QueryRowContext(ctx, "SELECT oid FROM pg_database WHERE datname=$1",
			string(fixture.database.Spec.Name)).Scan(&oldDatabaseOID)).To(Succeed())
		Expect(fixture.target.QueryRowContext(ctx, "SELECT oid FROM pg_namespace WHERE nspname='public'").
			Scan(&oldSchemaOID)).To(Succeed())
		Expect(fixture.target.Close()).To(Succeed())
		fixture.target = nil
		roleSQL(ctx, fixture.Fixture, "DROP DATABASE "+quoteIdentifier(string(fixture.database.Spec.Name)))
		roleSQL(ctx, fixture.Fixture, "CREATE DATABASE "+quoteIdentifier(string(fixture.database.Spec.Name))+
			" OWNER "+quoteIdentifier(fixture.Role))
		var err error
		fixture.target, err = fixture.Server.Open(string(fixture.database.Spec.Name), roleSQLAdmin, fixture.Server.password)
		Expect(err).NotTo(HaveOccurred())
		Expect(fixture.Server.Admin.QueryRowContext(ctx, "SELECT oid FROM pg_database WHERE datname=$1",
			string(fixture.database.Spec.Name)).Scan(&newDatabaseOID)).To(Succeed())
		Expect(fixture.target.QueryRowContext(ctx, "SELECT oid FROM pg_namespace WHERE nspname='public'").
			Scan(&newSchemaOID)).To(Succeed())
		Expect(newDatabaseOID).NotTo(Equal(oldDatabaseOID))
		Expect(oldSchemaOID).To(Equal(uint32(2200)))
		Expect(newSchemaOID).To(Equal(oldSchemaOID), "child OID reuse is real, not an injected catalog")
		fixture.exec(ctx, "SET ROLE "+quoteIdentifier(fixture.Role)+"; GRANT USAGE ON SCHEMA public TO "+
			quoteIdentifier(fixture.reader)+"; ALTER DEFAULT PRIVILEGES FOR ROLE "+quoteIdentifier(fixture.Role)+
			" GRANT SELECT ON TABLES TO "+quoteIdentifier(fixture.reader)+"; RESET ROLE")
		Expect(fixture.direct(ctx, "schema", archPublicSchema, fixture.reader, "USAGE")).To(BeTrue())
		Expect(archDefaultPrivilege(ctx, fixture)).To(BeTrue())
		fixture.update(ctx, func(spec *epco.DatabaseSpec) { spec.Schemas[0].Permissions = nil; spec.DefaultPrivileges = nil })
		fixture.ready(ctx)
		Expect(fixture.direct(ctx, "schema", archPublicSchema, fixture.reader, "USAGE")).To(BeTrue(),
			"replacement schema grant was not acquired by the retired binding")
		Expect(archDefaultPrivilege(ctx, fixture)).To(BeTrue(), "replacement creator defaults are independent of old history")
		Expect(archBindings(recoveredDatabase(ctx, fixture.scope.Client, fixture.database))).To(BeEmpty())
		fixture.update(ctx, func(spec *epco.DatabaseSpec) {
			spec.Schemas[0].Permissions = &epco.SchemaPermissions{Usage: r3Permissions(fixture.reader, epco.EnsurePresent)}
		})
		fixture.ready(ctx)
		Expect(archBindings(recoveredDatabase(ctx, fixture.scope.Client, fixture.database))).To(HaveLen(1),
			"current declarations may independently acquire replacement rights")
	}, NodeTimeout(40*time.Second))

	DescribeTable("AR3 rolls back a replacement relation resolved after the semantic checkpoint",
		func(ctx SpecContext, sequence, revoke bool) {
			kind, privilege, create := "TABLE", archSelect, "CREATE TABLE app."+quoteIdentifier(archRelation)+"(value int)"
			if sequence {
				kind, create = "SEQUENCE", "CREATE SEQUENCE app."+quoteIdentifier(archRelation)
			}
			fixture.exec(ctx, "SET ROLE "+quoteIdentifier(fixture.Role)+"; "+create+"; RESET ROLE")
			fixture.update(ctx, func(spec *epco.DatabaseSpec) {
				if sequence {
					spec.Schemas[0].Permissions = &epco.SchemaPermissions{
						Sequences: &epco.SequencePermissions{Select: r3Permissions(fixture.reader, epco.EnsurePresent)},
					}
				} else {
					spec.Schemas[0].TablePermissions = []epco.NamedTablePermissions{{Name: archRelation,
						Permissions: epco.TablePermissions{Select: r3Permissions(fixture.reader, epco.EnsurePresent)}}}
				}
			})
			pending := archPendingGrant
			if revoke {
				fixture.ready(ctx)
				pending = "revoke"
				fixture.update(ctx, func(spec *epco.DatabaseSpec) {
					spec.Schemas[0].TablePermissions = nil
					spec.Schemas[0].Permissions = nil
				})
			}
			var switched atomic.Bool
			var oldOID, replacementOID uint32
			Expect(fixture.target.QueryRowContext(ctx, "SELECT 'app.architecture_target'::regclass::oid").
				Scan(&oldOID)).To(Succeed())
			factory := &archACLFactory{
				delegate: pg.NewDatabaseFactory(),
				afterCheckpoint: func(barrierCtx context.Context, targets []epco.ManagedACLTarget) error {
					if !slices.ContainsFunc(archTargetBindings(targets), func(binding epco.ACLBinding) bool {
						return binding.Table == archRelation && binding.Pending == pending
					}) || !switched.CompareAndSwap(false, true) {
						return nil
					}
					fixture.exec(barrierCtx, "ALTER "+kind+" app."+quoteIdentifier(archRelation)+" RENAME TO "+
						quoteIdentifier(archOldRelation)+"; SET ROLE "+quoteIdentifier(fixture.Role)+"; "+create+"; RESET ROLE")
					if revoke {
						fixture.exec(barrierCtx, "SET ROLE "+quoteIdentifier(fixture.Role)+"; GRANT "+privilege+" ON "+kind+" app."+
							quoteIdentifier(archRelation)+" TO "+quoteIdentifier(fixture.reader)+"; RESET ROLE")
					}
					Expect(fixture.target.QueryRowContext(barrierCtx, "SELECT 'app.architecture_target'::regclass::oid").
						Scan(&replacementOID)).To(Succeed())
					Expect(replacementOID).NotTo(Equal(oldOID))
					return nil
				},
			}
			current := fixture.reconcile(ctx, databaseRecoveryReconciler(fixture.scope.Client, factory))
			Expect(switched.Load()).To(BeTrue(), "DDL takes place after actual durable checkpoint and before SQL")
			Expect(fixture.direct(ctx, "relation", "app."+archRelation, fixture.reader, privilege)).To(Equal(revoke),
				"the replacement relation's independent ACL is unchanged")
			Expect(archCondition(current).Status).To(Equal(metav1.ConditionFalse))
			Expect(archBindings(current)).NotTo(BeEmpty(), "identity mismatch cannot discard possibly issued history")
			fixture.update(ctx, func(spec *epco.DatabaseSpec) {
				spec.Schemas[0].TablePermissions = nil
				spec.Schemas[0].Permissions = nil
			})
			fixture.ready(ctx)
			Expect(fixture.direct(ctx, "relation", "app."+archOldRelation, fixture.reader, privilege)).To(BeFalse())
			Expect(fixture.direct(ctx, "relation", "app."+archRelation, fixture.reader, privilege)).To(Equal(revoke))
		},
		Entry("table GRANT", false, false, NodeTimeout(35*time.Second)),
		Entry("table REVOKE", false, true, NodeTimeout(35*time.Second)),
		Entry("sequence GRANT", true, false, NodeTimeout(35*time.Second)),
		Entry("sequence REVOKE", true, true, NodeTimeout(35*time.Second)))

	DescribeTable("AR5 preserves new administrator access before old inherited CONNECT pin cleanup",
		func(ctx SpecContext, rejectPin, independent bool) {
			if fixture.version < 160000 {
				Skip("PostgreSQL 16 adds separate INHERIT and SET membership options")
			}
			owner, nextAdmin := fixture.role(ctx), fixture.role(ctx)
			roleSQL(ctx, fixture.Fixture, "ALTER ROLE "+quoteIdentifier(owner)+" NOLOGIN; GRANT "+quoteIdentifier(owner)+" TO "+
				quoteIdentifier(fixture.Role)+" WITH INHERIT FALSE, SET TRUE; GRANT "+quoteIdentifier(fixture.Role)+" TO "+
				quoteIdentifier(nextAdmin)+" WITH INHERIT TRUE, SET TRUE")
			fixture.update(ctx, func(spec *epco.DatabaseSpec) {
				spec.Owner = epco.SQLName(owner)
				spec.Permissions = &epco.DatabasePermissions{Connect: r3Permissions("public", epco.EnsureAbsent)}
			})
			fixture.ready(ctx)
			before := recoveredDatabase(ctx, fixture.scope.Client, fixture.database)
			Expect(slices.ContainsFunc(archBindings(before), func(binding epco.ACLBinding) bool {
				return binding.Administrative && binding.Grantee == fixture.Role && binding.Privilege == "CONNECT"
			})).To(BeTrue(), "A acquired its pin after PUBLIC cleanup")
			if independent {
				fixture.exec(ctx, "SET ROLE "+quoteIdentifier(owner)+"; GRANT CONNECT ON DATABASE "+
					quoteIdentifier(string(fixture.database.Spec.Name))+" TO "+quoteIdentifier(nextAdmin)+"; RESET ROLE")
			}
			Expect(fixture.direct(ctx, r3DatabaseScope, string(fixture.database.Spec.Name), "public", "CONNECT")).To(BeFalse())
			Expect(fixture.direct(ctx, r3DatabaseScope, string(fixture.database.Spec.Name), nextAdmin, "CONNECT")).
				To(Equal(independent))
			var inheritedOwner, setOwner bool
			Expect(fixture.target.QueryRowContext(ctx, "SELECT pg_has_role($1,$2,'USAGE'),pg_has_role($1,$2,'SET')",
				nextAdmin, owner).
				Scan(&inheritedOwner, &setOwner)).To(Succeed())
			Expect(inheritedOwner).To(BeFalse())
			Expect(setOwner).To(BeTrue())
			Expect(archFreshConnect(ctx, fixture, nextAdmin)).To(Succeed(), "B initially has effective target CONNECT")
			archAdminSecret(ctx, fixture, nextAdmin)
			factory := &archACLFactory{delegate: pg.NewDatabaseFactory()}
			var rejected atomic.Bool
			if rejectPin {
				factory.beforeCheckpoint = func(_ context.Context, targets []epco.ManagedACLTarget) error {
					if slices.ContainsFunc(archTargetBindings(targets), func(binding epco.ACLBinding) bool {
						return binding.Administrative && binding.Grantee == nextAdmin && binding.Pending == archPendingGrant
					}) {
						rejected.Store(true)
						return &pg.Failure{
							Reason: pg.ConnectionUnavailable, Message: "injected refusal of necessary new administrator pin",
						}
					}
					return nil
				}
			}
			current := fixture.reconcile(ctx, databaseRecoveryReconciler(fixture.scope.Client, factory))
			Expect(archFreshConnect(ctx, fixture, nextAdmin)).To(Succeed(),
				"old pin cleanup never removes B's sole effective CONNECT")
			if rejectPin {
				Expect(fixture.direct(ctx, r3DatabaseScope, string(fixture.database.Spec.Name), fixture.Role, "CONNECT")).
					To(BeTrue(), "failed new-pin checkpoint prohibits old pin revocation")
				Expect(rejected.Load()).To(BeTrue())
				Expect(archCondition(current).Status).To(Equal(metav1.ConditionFalse))
			} else {
				fixture.ready(ctx)
				current = recoveredDatabase(ctx, fixture.scope.Client, fixture.database)
				Expect(fixture.direct(ctx, r3DatabaseScope, string(fixture.database.Spec.Name), nextAdmin, "CONNECT")).To(BeTrue())
				Expect(fixture.direct(ctx, r3DatabaseScope, string(fixture.database.Spec.Name), fixture.Role, "CONNECT")).
					To(BeFalse())
				Expect(slices.ContainsFunc(archBindings(current), func(binding epco.ACLBinding) bool {
					return binding.Administrative && binding.Grantee == nextAdmin && (independent || binding.Pending == "")
				})).To(Equal(!independent), "an independent B path must not be claimed as an operator-owned pin")
			}
		},
		Entry("confirmed new pin", false, false, NodeTimeout(40*time.Second)),
		Entry("failed new-pin persistence", true, false, NodeTimeout(40*time.Second)),
		Entry("independent new administrator path", false, true, NodeTimeout(40*time.Second)))
})
