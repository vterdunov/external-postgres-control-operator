//go:build integration

package integration

import (
	"context"
	"errors"
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
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	validationGrantorTable      = "grantor_provenance"
	validationPublicSchema      = "public"
	validationDatabaseOwnerRole = "pg_database_owner"
	validationGranteeIdentity   = "grantee"
)

// The callback first persists real recovery intent, then rejects that semantic
// checkpoint's response. Its caller must not issue SQL with an uncertain result.
type validationCheckpointFactory struct {
	delegate pg.DatabaseFactory
	failed   atomic.Bool
}

type validationCheckpointSession struct {
	pg.DatabaseSession
	pg.DatabaseACLSession
	owner *validationCheckpointFactory
}

func (f *validationCheckpointFactory) Open(ctx context.Context, config *pg.Config,
	options pg.DatabaseSessionOptions) (pg.DatabaseSession, error) {
	session, err := f.delegate.Open(ctx, config, options)
	if err != nil {
		return nil, err
	}
	return &validationCheckpointSession{DatabaseSession: session,
		DatabaseACLSession: session.(pg.DatabaseACLSession), owner: f}, nil
}

func (s *validationCheckpointSession) ReconcileACL(ctx context.Context, desired pg.ACLDesired) (*pg.ACLResult, error) {
	persist := desired.Checkpoint
	desired.Checkpoint = func(writeCtx context.Context, targets []epco.ManagedACLTarget) error {
		if err := persist(writeCtx, targets); err != nil {
			return err
		}
		for _, target := range targets {
			for _, binding := range target.Bindings {
				if binding.Table == validationGrantorTable && binding.Pending == "grant" &&
					binding.GrantorOID == 0 && s.owner.failed.CompareAndSwap(false, true) {
					return errors.New("injected uncertain response after persisted grant intent before SQL")
				}
			}
		}
		return nil
	}
	return s.DatabaseACLSession.ReconcileACL(ctx, desired)
}

func validationGrantorRow(ctx context.Context, fixture *r3SQLFixture, grantor string) bool {
	var present bool
	ExpectWithOffset(1, fixture.target.QueryRowContext(ctx, `SELECT EXISTS (
	 SELECT 1 FROM pg_class c CROSS JOIN LATERAL aclexplode(c.relacl) a
	 WHERE c.oid='app.grantor_provenance'::regclass
	 AND a.grantee=(SELECT oid FROM pg_roles WHERE rolname=$1)
	 AND a.grantor=(SELECT oid FROM pg_roles WHERE rolname=$2)
	 AND a.privilege_type='SELECT')`, fixture.reader, grantor).Scan(&present)).To(Succeed())
	return present
}

var _ = Describe("R3 independent grantor provenance validation", Ordered, ContinueOnFailure, func() {
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
		scope, _, err := environment.NewScope(ctx, "r3-validation-")
		Expect(err).NotTo(HaveOccurred())
		fixture = &r3SQLFixture{Fixture: base, scope: scope}
		DeferCleanup(func() {
			cleanup, cancel := context.WithTimeout(context.Background(), 25*time.Second)
			defer cancel()
			Expect(fixture.cleanup(cleanup)).To(Succeed())
		})
		Expect(base.Admin.QueryRowContext(ctx, "SHOW server_version_num").Scan(&fixture.version)).To(Succeed())
		fixture.reader, fixture.other = fixture.role(ctx), fixture.role(ctx)
		cluster := recoveryCluster(ctx, scope, base)
		secret := &corev1.Secret{}
		secretKey := client.ObjectKey{Namespace: scope.Namespace.Name, Name: recoveryAdminSecret}
		Expect(environment.Client.Get(ctx, secretKey, secret)).To(Succeed())
		secret.Data[corev1.BasicAuthUsernameKey] = []byte(base.Role)
		secret.Data[corev1.BasicAuthPasswordKey] = []byte(base.Password)
		Expect(environment.Client.Update(ctx, secret)).To(Succeed())
		name, err := randomName("r3_validation_")
		Expect(err).NotTo(HaveOccurred())
		fixture.database = &epco.Database{Name: "permissions", Namespace: scope.Namespace.Name,
			Spec: epco.DatabaseSpec{Name: epco.SQLName(name), Owner: epco.SQLName(base.Role),
				Cluster: epco.ClusterReference{Name: cluster.Name}, ReclaimPolicy: epco.ReclaimRetain,
				Schemas: []epco.SchemaSpec{{Name: r3Schema}}}}
		roleSQL(ctx, base, "CREATE DATABASE "+quoteIdentifier(name)+" OWNER "+quoteIdentifier(base.Role))
		fixture.target, err = base.Server.Open(name, roleSQLAdmin, base.Server.password)
		Expect(err).NotTo(HaveOccurred())
		fixture.exec(ctx, "CREATE SCHEMA app AUTHORIZATION "+quoteIdentifier(base.Role))
		fixture.table(ctx, validationGrantorTable)
		Expect(scope.Create(ctx, fixture.database)).To(Succeed())
		fixture.ready(ctx)
	}, NodeTimeout(30*time.Second))

	declareReader := func(ctx context.Context) {
		fixture.update(ctx, func(spec *epco.DatabaseSpec) {
			spec.Schemas[0].TablePermissions = []epco.NamedTablePermissions{{Name: validationGrantorTable,
				Permissions: epco.TablePermissions{Select: r3Permissions(fixture.reader, epco.EnsurePresent)}}}
		})
	}

	It("rejects duplicate broad and named tuples before lifecycle or ACL mutation", func(ctx SpecContext) {
		const limitQuery = "SELECT datconnlimit FROM pg_database WHERE datname=$1"
		before := recoveredDatabase(ctx, environment.Client, fixture.database)
		var originalLimit int64
		Expect(fixture.Admin.QueryRowContext(ctx, limitQuery,
			string(fixture.database.Spec.Name)).Scan(&originalLimit)).To(Succeed())
		cases := []struct {
			table  epco.SQLName
			ensure epco.EnsureOption
		}{
			{validationGrantorTable, epco.EnsurePresent},
			{validationGrantorTable, epco.EnsureAbsent},
			{"missing_duplicate", epco.EnsurePresent},
		}
		for _, variation := range cases {
			fixture.update(ctx, func(spec *epco.DatabaseSpec) {
				spec.ConnectionLimit = new(originalLimit + 7)
				spec.Permissions = &epco.DatabasePermissions{Create: r3Permissions(fixture.reader, epco.EnsurePresent)}
				spec.Schemas[0].Permissions = &epco.SchemaPermissions{
					Tables: &epco.TablePermissions{Select: r3Permissions(fixture.reader, epco.EnsurePresent)}}
				spec.Schemas[0].TablePermissions = []epco.NamedTablePermissions{{Name: variation.table,
					Permissions: epco.TablePermissions{Select: r3Permissions(fixture.reader, variation.ensure)}}}
			})
			current := fixture.pending(ctx, pg.InvalidConfiguration)
			Expect(current.Status.AppliedGeneration).To(Equal(before.Status.AppliedGeneration))
			Expect(current.Status.ManagedACLTargets).To(Equal(before.Status.ManagedACLTargets),
				"invalid declarations must not acquire new recovery intent")
			var limit int64
			Expect(fixture.Admin.QueryRowContext(ctx, limitQuery,
				string(fixture.database.Spec.Name)).Scan(&limit)).To(Succeed())
			Expect(limit).To(Equal(originalLimit), "preflight must precede the requested lifecycle change")
			Expect(fixture.direct(ctx, r3DatabaseScope, string(fixture.database.Spec.Name), fixture.reader, "CREATE")).
				To(BeFalse())
			Expect(fixture.direct(ctx, "table", "app.grantor_provenance", fixture.reader, "SELECT")).To(BeFalse())
		}
	}, NodeTimeout(30*time.Second))

	It("does not adopt an independent late grant outside pending grantor candidates", func(ctx SpecContext) {
		fixture.exec(ctx, "GRANT USAGE ON SCHEMA app TO "+quoteIdentifier(fixture.other)+
			"; SET ROLE "+quoteIdentifier(fixture.Role)+
			"; GRANT SELECT ON app.grantor_provenance TO "+quoteIdentifier(fixture.other)+
			" WITH GRANT OPTION; RESET ROLE")
		declareReader(ctx)
		fault := &validationCheckpointFactory{delegate: pg.NewDatabaseFactory()}
		reconciler := databaseRecoveryReconciler(environment.Client, fault)
		Eventually(func() bool {
			_, _ = reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(fixture.database)})
			return fault.failed.Load()
		}).WithContext(ctx).WithTimeout(5 * time.Second).Should(BeTrue())
		Expect(fixture.direct(ctx, "table", "app.grantor_provenance", fixture.reader, "SELECT")).To(BeFalse(),
			"uncertain persisted checkpoint must prevent SQL")
		current := recoveredDatabase(ctx, environment.Client, fixture.database)
		Expect(current.Status.ManagedACLTargets).NotTo(BeEmpty())
		pending := current.Status.ManagedACLTargets[0].Bindings
		Expect(pending).To(HaveLen(1))
		Expect(pending[0].GrantorCandidates).To(ConsistOf(epco.ACLRoleIdentity{Name: fixture.Role, OID: pending[0].OwnerOID}))
		fixture.exec(ctx, "SET ROLE "+quoteIdentifier(fixture.other)+
			"; GRANT SELECT ON app.grantor_provenance TO "+quoteIdentifier(fixture.reader)+"; RESET ROLE")
		roleSQL(ctx, fixture.Fixture, "GRANT "+quoteIdentifier(fixture.other)+" TO "+quoteIdentifier(fixture.Role))
		fixture.update(ctx, func(spec *epco.DatabaseSpec) { spec.Schemas[0].TablePermissions = nil })
		fixture.reconcile(ctx, databaseRecoveryReconciler(environment.Client, pg.NewDatabaseFactory()))
		Expect(validationGrantorRow(ctx, fixture, fixture.other)).To(BeTrue(),
			"a late grant from outside checkpointed candidates is independent and must survive withdrawal")
		Expect(validationGrantorRow(ctx, fixture, fixture.Role)).To(BeFalse())
	}, NodeTimeout(30*time.Second))

	It("blocks ambiguous inherited-only revoke before touching another grantor row", func(ctx SpecContext) {
		if fixture.version < 160000 {
			Skip("SET FALSE membership authority requires PostgreSQL 16 or later")
		}
		fixture.exec(ctx, "GRANT USAGE ON SCHEMA app TO "+quoteIdentifier(fixture.other)+
			"; SET ROLE "+quoteIdentifier(fixture.Role)+
			"; GRANT SELECT ON app.grantor_provenance TO "+quoteIdentifier(fixture.other)+
			" WITH GRANT OPTION; RESET ROLE; SET ROLE "+quoteIdentifier(fixture.other)+
			"; GRANT SELECT ON app.grantor_provenance TO "+quoteIdentifier(fixture.reader)+"; RESET ROLE")
		declareReader(ctx)
		fixture.ready(ctx)
		roleSQL(ctx, fixture.Fixture, "GRANT "+quoteIdentifier(fixture.other)+" TO "+quoteIdentifier(fixture.Role)+
			" WITH INHERIT TRUE, SET FALSE")
		fixture.exec(ctx, "SET ROLE "+quoteIdentifier(fixture.Role)+
			"; GRANT SELECT ON app.grantor_provenance TO "+quoteIdentifier(fixture.reader)+"; RESET ROLE")
		Expect(validationGrantorRow(ctx, fixture, fixture.Role)).To(BeTrue())
		Expect(validationGrantorRow(ctx, fixture, fixture.other)).To(BeTrue())
		fixture.update(ctx, func(spec *epco.DatabaseSpec) { spec.Schemas[0].TablePermissions = nil })
		current := fixture.reconcile(ctx, databaseRecoveryReconciler(environment.Client, pg.NewDatabaseFactory()))
		Expect(validationGrantorRow(ctx, fixture, fixture.Role)).To(BeTrue(),
			"the fallback inherited grantor must not revoke an independent owner's row")
		Expect(validationGrantorRow(ctx, fixture, fixture.other)).To(BeTrue())
		condition := meta.FindStatusCondition(current.Status.Conditions, "Ready")
		Expect(condition).NotTo(BeNil())
		Expect(condition.Status).To(Equal(metav1.ConditionFalse))
		Expect(current.Status.ManagedACLTargets).NotTo(BeEmpty())
	}, NodeTimeout(30*time.Second))

	It("keeps explicit empty ACLs distinct from NULL and missing default layers", func(ctx SpecContext) {
		fixture.update(ctx, func(spec *epco.DatabaseSpec) {
			spec.ManagementPolicy = epco.ManagementObserve
			spec.Schemas = nil
			spec.Permissions = &epco.DatabasePermissions{Connect: r3Permissions(validationPublicSchema, epco.EnsurePresent)}
		})
		fixture.ready(ctx)
		var initiallyNull bool
		Expect(fixture.target.QueryRowContext(ctx, "SELECT datacl IS NULL FROM pg_database WHERE datname=$1",
			string(fixture.database.Spec.Name)).Scan(&initiallyNull)).To(Succeed())
		Expect(initiallyNull).To(BeTrue(), "NULL database ACL expands its built-in PUBLIC CONNECT")
		fixture.exec(ctx, "REVOKE ALL ON DATABASE "+quoteIdentifier(string(fixture.database.Spec.Name))+
			" FROM PUBLIC, "+quoteIdentifier(fixture.Role))
		var emptyBefore bool
		var aclBefore string
		var dimsBefore string
		const emptyDatabaseQuery = `SELECT datacl IS NOT NULL AND cardinality(datacl)=0,
		 COALESCE(datacl::text,'NULL'),COALESCE(array_dims(datacl),'NULL') FROM pg_database WHERE datname=$1`
		Expect(fixture.target.QueryRowContext(ctx, emptyDatabaseQuery, string(fixture.database.Spec.Name)).
			Scan(&emptyBefore, &aclBefore, &dimsBefore)).To(Succeed())
		Expect(emptyBefore).To(BeTrue(), "fixture must establish zero direct rows before observation: %s", aclBefore)
		fixture.update(ctx, func(spec *epco.DatabaseSpec) {
			spec.Permissions = &epco.DatabasePermissions{
				Connect:   r3Permissions(validationPublicSchema, epco.EnsureAbsent),
				Temporary: r3Permissions(validationPublicSchema, epco.EnsureAbsent),
				Create:    r3Permissions(fixture.Role, epco.EnsureAbsent)}
		})
		fixture.ready(ctx)
		var databaseEmpty bool
		var aclAfter string
		var dimsAfter string
		Expect(fixture.target.QueryRowContext(ctx, emptyDatabaseQuery, string(fixture.database.Spec.Name)).
			Scan(&databaseEmpty, &aclAfter, &dimsAfter)).To(Succeed())
		Expect(databaseEmpty).To(BeTrue(), "observation must not restore built-ins: before %s, after %s", aclBefore, aclAfter)
		GinkgoWriter.Printf("empty database ACL before=%s dims=%s, after=%s dims=%s; both cardinality=0\n",
			aclBefore, dimsBefore, aclAfter, dimsAfter)
		fixture.exec(ctx, "GRANT CONNECT ON DATABASE "+quoteIdentifier(string(fixture.database.Spec.Name))+
			" TO "+quoteIdentifier(fixture.Role)+
			"; REVOKE ALL ON SCHEMA app FROM "+quoteIdentifier(fixture.Role)+
			"; REVOKE ALL ON TABLE app.grantor_provenance FROM "+quoteIdentifier(fixture.Role)+
			"; ALTER DEFAULT PRIVILEGES FOR ROLE "+quoteIdentifier(fixture.Role)+
			" REVOKE ALL ON TABLES FROM "+quoteIdentifier(fixture.Role))
		fixture.update(ctx, func(spec *epco.DatabaseSpec) {
			spec.Permissions = nil
			spec.DefaultPrivileges = []epco.DefaultPrivileges{{ForRole: epco.SQLName(fixture.Role),
				Tables: &epco.TablePermissions{Select: r3Permissions(fixture.Role, epco.EnsureAbsent)}}}
			spec.Schemas = []epco.SchemaSpec{{Name: r3Schema,
				Permissions: &epco.SchemaPermissions{Usage: r3Permissions(fixture.Role, epco.EnsureAbsent)},
				TablePermissions: []epco.NamedTablePermissions{{Name: validationGrantorTable,
					Permissions: epco.TablePermissions{Select: r3Permissions(fixture.Role, epco.EnsureAbsent)}}},
				DefaultPrivileges: []epco.DefaultPrivileges{{ForRole: epco.SQLName(fixture.Role),
					Tables: &epco.TablePermissions{Select: r3Permissions(fixture.reader, epco.EnsureAbsent)}}}}}
		})
		fixture.ready(ctx)
		var schemaEmpty, tableEmpty, defaultEmpty bool
		const schemaEmptyQuery = `SELECT nspacl IS NOT NULL AND cardinality(nspacl)=0
		 FROM pg_namespace WHERE nspname='app'`
		Expect(fixture.target.QueryRowContext(ctx, schemaEmptyQuery).
			Scan(&schemaEmpty)).To(Succeed())
		const tableEmptyQuery = `SELECT relacl IS NOT NULL AND cardinality(relacl)=0
		 FROM pg_class WHERE oid='app.grantor_provenance'::regclass`
		Expect(fixture.target.QueryRowContext(ctx, tableEmptyQuery).
			Scan(&tableEmpty)).To(Succeed())
		Expect(fixture.target.QueryRowContext(ctx, `SELECT defaclacl IS NOT NULL AND cardinality(defaclacl)=0
		 FROM pg_default_acl
		 WHERE defaclrole=(SELECT oid FROM pg_roles WHERE rolname=$1) AND defaclnamespace=0 AND defaclobjtype='r'`,
			fixture.Role).Scan(&defaultEmpty)).To(Succeed())
		Expect(schemaEmpty && tableEmpty && defaultEmpty).To(BeTrue(),
			"explicit empty object/global ACLs stay empty while a missing schema-default layer has no rows")
	}, NodeTimeout(30*time.Second))

	It("uses target database owner SET authority for public-schema pseudo-role grants", func(ctx SpecContext) {
		roleSQL(ctx, fixture.Fixture, "ALTER ROLE "+quoteIdentifier(fixture.Role)+" NOINHERIT")
		membership := "GRANT " + quoteIdentifier(fixture.other) + " TO " + quoteIdentifier(fixture.Role)
		if fixture.version >= 160000 {
			membership += " WITH INHERIT FALSE, SET TRUE"
		}
		roleSQL(ctx, fixture.Fixture, membership)
		fixture.exec(ctx, "ALTER DATABASE "+quoteIdentifier(string(fixture.database.Spec.Name))+
			" OWNER TO "+quoteIdentifier(fixture.other))
		var schemaOwner string
		const publicOwnerQuery = "SELECT pg_get_userbyid(nspowner) FROM pg_namespace WHERE nspname='public'"
		Expect(fixture.target.QueryRowContext(ctx, publicOwnerQuery).
			Scan(&schemaOwner)).To(Succeed())
		Expect(schemaOwner).To(Equal(validationDatabaseOwnerRole))
		fixture.update(ctx, func(spec *epco.DatabaseSpec) {
			spec.Owner = epco.SQLName(fixture.other)
			spec.Schemas = []epco.SchemaSpec{{Name: epco.SQLName(validationPublicSchema), Permissions: &epco.SchemaPermissions{
				Usage: r3Permissions(fixture.reader, epco.EnsurePresent)}}}
		})
		fixture.ready(ctx)
		Expect(fixture.direct(ctx, "schema", validationPublicSchema, fixture.reader, "USAGE")).To(BeTrue())
		current := recoveredDatabase(ctx, environment.Client, fixture.database)
		var recordedGrantor string
		for _, target := range current.Status.ManagedACLTargets {
			for _, binding := range target.Bindings {
				if binding.Schema == validationPublicSchema && binding.Grantee == fixture.reader && binding.Privilege == "USAGE" {
					recordedGrantor = binding.Grantor
				}
			}
		}
		Expect(recordedGrantor).To(Equal(validationDatabaseOwnerRole),
			"existing target-owner SET authority must preserve the actual pseudo-role ACL grantor")
	}, NodeTimeout(30*time.Second))

	It("preserves a late independent grant merged by an unobserved external owner transfer", func(ctx SpecContext) {
		declareReader(ctx)
		fixture.ready(ctx)
		Expect(validationGrantorRow(ctx, fixture, fixture.Role)).To(BeTrue())
		fixture.exec(ctx, "GRANT USAGE ON SCHEMA app TO "+quoteIdentifier(fixture.other)+
			"; SET ROLE "+quoteIdentifier(fixture.Role)+
			"; GRANT SELECT ON app.grantor_provenance TO "+quoteIdentifier(fixture.other)+
			" WITH GRANT OPTION; RESET ROLE; SET ROLE "+quoteIdentifier(fixture.other)+
			"; GRANT SELECT ON app.grantor_provenance TO "+quoteIdentifier(fixture.reader)+"; RESET ROLE")
		Expect(validationGrantorRow(ctx, fixture, fixture.other)).To(BeTrue(),
			"independent future-owner row appears only after the last successful management")
		roleSQL(ctx, fixture.Fixture, "GRANT "+quoteIdentifier(fixture.other)+" TO "+quoteIdentifier(fixture.Role))
		fixture.exec(ctx, "ALTER TABLE app.grantor_provenance OWNER TO "+quoteIdentifier(fixture.other))
		Expect(validationGrantorRow(ctx, fixture, fixture.Role)).To(BeFalse(), "PostgreSQL rewrites the old grantor")
		Expect(validationGrantorRow(ctx, fixture, fixture.other)).To(BeTrue(), "PostgreSQL merges the two grantor rows")
		for range 2 {
			present := fixture.reconcile(ctx, databaseRecoveryReconciler(environment.Client, pg.NewDatabaseFactory()))
			condition := meta.FindStatusCondition(present.Status.Conditions, "Ready")
			Expect(condition).NotTo(BeNil())
			Expect(condition.Status).To(Equal(metav1.ConditionFalse),
				"unchanged present retries cannot adopt the merged independent row and erase unresolved provenance")
			Expect(condition.Reason).To(Equal(pg.ACLBindingUnresolved))
			Expect(validationGrantorRow(ctx, fixture, fixture.other)).To(BeTrue())
		}
		fixture.update(ctx, func(spec *epco.DatabaseSpec) { spec.Schemas[0].TablePermissions = nil })
		current := fixture.reconcile(ctx, databaseRecoveryReconciler(environment.Client, pg.NewDatabaseFactory()))
		Expect(validationGrantorRow(ctx, fixture, fixture.other)).To(BeTrue(),
			"stale pre-transfer history cannot authorize deletion of the independent merged row")
		condition := meta.FindStatusCondition(current.Status.Conditions, "Ready")
		Expect(condition).NotTo(BeNil())
		Expect(condition.Status).To(Equal(metav1.ConditionFalse))
		Expect(condition.Reason).To(Equal(pg.ACLBindingUnresolved))
		Expect(current.Status.ManagedACLTargets).NotTo(BeEmpty())
		fixture.update(ctx, func(spec *epco.DatabaseSpec) {
			spec.Schemas[0].TablePermissions = []epco.NamedTablePermissions{{Name: validationGrantorTable,
				Permissions: epco.TablePermissions{Select: r3Permissions(fixture.reader, epco.EnsureAbsent)}}}
		})
		fixture.ready(ctx)
		Expect(validationGrantorRow(ctx, fixture, fixture.other)).To(BeFalse(),
			"explicit absent authorizes direct historical cleanup without needing lost grant provenance")
		current = recoveredDatabase(ctx, environment.Client, fixture.database)
		for _, target := range current.Status.ManagedACLTargets {
			Expect(target.Bindings).To(BeEmpty(), "confirmed direct absence must clear the unresolved history")
		}
	}, NodeTimeout(30*time.Second))

	DescribeTable("keeps unproven role identity pending after descriptor recreation",
		func(ctx SpecContext, identity string) {
			roleSQL(ctx, fixture.Fixture, "GRANT "+quoteIdentifier(fixture.other)+" TO "+quoteIdentifier(fixture.Role))
			switch identity {
			case "grantor":
				fixture.exec(ctx, "GRANT USAGE ON SCHEMA app TO "+quoteIdentifier(fixture.other)+
					"; SET ROLE "+quoteIdentifier(fixture.Role)+
					"; GRANT SELECT ON app.grantor_provenance TO "+quoteIdentifier(fixture.other)+
					" WITH GRANT OPTION; RESET ROLE; SET ROLE "+quoteIdentifier(fixture.other)+
					"; GRANT SELECT ON app.grantor_provenance TO "+quoteIdentifier(fixture.reader)+"; RESET ROLE")
				declareReader(ctx)
			case validationGranteeIdentity:
				declareReader(ctx)
			case "creator":
				fixture.update(ctx, func(spec *epco.DatabaseSpec) {
					spec.DefaultPrivileges = []epco.DefaultPrivileges{{ForRole: epco.SQLName(fixture.other),
						Tables: &epco.TablePermissions{Select: r3Permissions(fixture.reader, epco.EnsurePresent)}}}
				})
			}
			fixture.ready(ctx)
			before := recoveredDatabase(ctx, environment.Client, fixture.database)
			Expect(before.Status.ManagedACLTargets).To(HaveLen(1))
			Expect(before.Status.ManagedACLTargets[0].Bindings).To(HaveLen(1))
			revision := before.Status.ManagedACLTargets[0].Bindings[0].BindingRevision
			Expect(revision).NotTo(BeEmpty())
			oldName := fixture.other
			if identity == validationGranteeIdentity {
				oldName = fixture.reader
			}
			newName, err := randomName("r3_renamed_")
			Expect(err).NotTo(HaveOccurred())
			roleSQL(ctx, fixture.Fixture, "ALTER ROLE "+quoteIdentifier(oldName)+" RENAME TO "+quoteIdentifier(newName))
			index := slices.Index(fixture.roles, oldName)
			Expect(index).To(BeNumerically(">=", 0))
			fixture.roles[index] = newName
			if identity == validationGranteeIdentity {
				fixture.reader = newName
			} else {
				fixture.other = newName
			}
			previous := &epco.PostgresCluster{}
			key := client.ObjectKey{Name: fixture.database.Spec.Cluster.Name, Namespace: fixture.database.Namespace}
			Expect(environment.Client.Get(ctx, key, previous)).To(Succeed())
			Expect(environment.Client.Delete(ctx, previous)).To(Succeed())
			replacement := &epco.PostgresCluster{Name: previous.Name, Namespace: previous.Namespace, Spec: previous.Spec}
			Expect(fixture.scope.Create(ctx, replacement)).To(Succeed())
			Expect(replacement.UID).NotTo(Equal(previous.UID))
			fixture.update(ctx, func(spec *epco.DatabaseSpec) {
				spec.Schemas[0].TablePermissions = nil
				spec.DefaultPrivileges = nil
			})
			for range 3 {
				current := fixture.reconcile(ctx, databaseRecoveryReconciler(environment.Client, pg.NewDatabaseFactory()))
				condition := meta.FindStatusCondition(current.Status.Conditions, "Ready")
				Expect(condition).NotTo(BeNil())
				Expect(condition.Status).To(Equal(metav1.ConditionFalse))
				Expect(condition.Reason).To(Equal(pg.ACLBindingUnresolved))
				Expect(current.Status.ManagedACLTargets).To(HaveLen(1))
				Expect(current.Status.ManagedACLTargets[0].Bindings).To(HaveLen(1),
					"an unproven renamed role must not cause durable cleanup history to be forgotten")
				Expect(current.Status.ManagedACLTargets[0].Bindings[0].BindingRevision).To(Equal(revision),
					"an unresolved old role OID must not be stamped as belonging to the new catalog epoch")
				if identity == "creator" {
					Expect(fixture.defaultGrant(ctx, fixture.other, "", "r", fixture.reader, "SELECT")).To(BeTrue())
				} else {
					Expect(fixture.direct(ctx, "table", "app.grantor_provenance", fixture.reader, "SELECT")).To(BeTrue())
				}
			}
		}, Entry("historical grantor rename", "grantor", NodeTimeout(30*time.Second)),
		Entry("historical grantee rename", validationGranteeIdentity, NodeTimeout(30*time.Second)),
		Entry("historical default creator rename", "creator", NodeTimeout(30*time.Second)))
})
