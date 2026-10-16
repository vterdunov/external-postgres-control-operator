//go:build integration

package integration

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	epco "github.com/vterdunov/external-postgres-control-operator/api/v1alpha1"
	pg "github.com/vterdunov/external-postgres-control-operator/internal/postgres"
)

func issue30SchemaSession(ctx context.Context, f *databaseSQLFixture, eligible map[string]bool) pg.DatabaseSession {
	config := productionConfig(f.Fixture, roleTLSMode, false)
	config.StatementTimeout = 5 * time.Second
	session, err := pg.NewDatabaseFactory().Open(ctx, config, pg.DatabaseSessionOptions{
		ResolveRoleCapability: func(_ context.Context, name string, _ pg.RoleCapabilityPurpose) (bool, error) {
			return eligible[name], nil
		},
	})
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	DeferCleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		Expect(session.Close(cleanup)).To(Succeed())
	})
	return session
}

func issue30ManagedOwner(ctx context.Context, f *databaseSQLFixture) string {
	name, err := randomName("issue30_owner_")
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	f.owners = append(f.owners, name)
	roles := roleSession(ctx, f.Fixture, true)
	_, err = roles.Apply(ctx, pg.RoleDesired{Name: name, Spec: epco.DatabaseRoleSpec{Login: false}})
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	return name
}

var _ = Describe("Issue30 schema owner authority", func() {
	var f *databaseSQLFixture
	BeforeEach(func(ctx SpecContext) {
		base, err := postgres.NewFixture(ctx)
		Expect(err).NotTo(HaveOccurred())
		f = &databaseSQLFixture{Fixture: base}
		DeferCleanup(func() {
			cleanup, cancel := context.WithTimeout(context.Background(), 25*time.Second)
			defer cancel()
			Expect(f.cleanup(cleanup)).To(Succeed())
		})
		Expect(f.Admin.QueryRowContext(ctx, "SHOW server_version_num").Scan(&f.version)).To(Succeed())
		if f.version < 160000 {
			Skip("the missing joint authority repair uses PG16 per-membership INHERIT")
		}
		roleSQL(ctx, f.Fixture, "ALTER ROLE "+quoteIdentifier(f.Role)+" CREATEDB CREATEROLE NOINHERIT")
		roleSQL(ctx, f.Fixture, "ALTER ROLE "+quoteIdentifier(f.Role)+" SET createrole_self_grant=''")
	}, NodeTimeout(30*time.Second))

	DescribeTable("transfers an existing schema after database ownership has moved",
		func(ctx SpecContext, operatorHasCreate bool) {
			oldOwner, newOwner := issue30ManagedOwner(ctx, f), issue30ManagedOwner(ctx, f)
			name := f.name()
			session := issue30SchemaSession(ctx, f, map[string]bool{oldOwner: true, newOwner: true})
			desired := databaseDesired(name, oldOwner)
			desired.Spec.Schemas = []epco.SchemaSpec{schemaSpec("issue30_schema", oldOwner, epco.EnsurePresent)}
			databaseApply(ctx, session, desired)
			target := f.target(name)
			_, err := target.ExecContext(ctx, "SET ROLE "+quoteIdentifier(oldOwner)+
				"; CREATE TABLE issue30_schema.payload(value int); INSERT INTO issue30_schema.payload VALUES (42); "+
				"ALTER TABLE issue30_schema.payload ENABLE ROW LEVEL SECURITY; RESET ROLE")
			Expect(err).NotTo(HaveOccurred())
			// Keep schema lookup available throughout, so table authority and RLS
			// are measured independently of the schema's later owner change.
			_, err = target.ExecContext(ctx, "GRANT USAGE ON SCHEMA issue30_schema TO "+quoteIdentifier(f.Role)+
				"; CREATE FUNCTION public.issue30_invoker() RETURNS bigint LANGUAGE SQL SECURITY INVOKER "+
				"AS 'SELECT count(*) FROM issue30_schema.payload'; "+
				"CREATE FUNCTION public.issue30_definer() RETURNS bigint LANGUAGE SQL SECURITY DEFINER "+
				"AS 'SELECT count(*) FROM issue30_schema.payload'; "+
				"ALTER FUNCTION public.issue30_definer() OWNER TO "+quoteIdentifier(f.Role))
			Expect(err).NotTo(HaveOccurred())
			var databaseOID, schemaOID, tableOID int64
			Expect(target.QueryRowContext(ctx, `SELECT
				(SELECT oid FROM pg_database WHERE datname=current_database()),
				'issue30_schema'::regnamespace::oid,'issue30_schema.payload'::regclass::oid`).
				Scan(&databaseOID, &schemaOID, &tableOID)).To(Succeed())

			// Vanilla PostgreSQL cannot reproduce the RDS owner-transfer privilege
			// hook. Prepare its observed successful outcome without application grants;
			// the schema transition below is performed only by the restricted operator.
			roleSQL(ctx, f.Fixture, "ALTER DATABASE "+quoteIdentifier(name)+" OWNER TO "+quoteIdentifier(newOwner))
			if operatorHasCreate {
				roleSQL(ctx, f.Fixture, "GRANT CREATE ON DATABASE "+quoteIdentifier(name)+" TO "+quoteIdentifier(f.Role))
			}
			desired.Spec.Owner = epco.SQLName(newOwner)
			databaseApply(ctx, session, desired)
			var canSet, inherited, canCreate bool
			Expect(target.QueryRowContext(ctx, "SELECT pg_has_role($1,$2,'SET'),pg_has_role($1,$2,'USAGE')",
				f.Role, oldOwner).Scan(&canSet, &inherited)).To(Succeed())
			Expect(canSet).To(BeTrue())
			Expect(inherited).To(BeFalse())
			Expect(target.QueryRowContext(ctx, `SELECT pg_has_role($1,$2,'SET'),
				has_database_privilege($1,current_database(),'CREATE')`,
				oldOwner, newOwner).Scan(&canSet, &canCreate)).To(Succeed())
			Expect(canSet || canCreate).To(BeFalse(), "old owner must not accidentally combine the needed capabilities")

			operator, err := f.Server.Open(name, f.Role, f.Password)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { Expect(operator.Close()).To(Succeed()) })
			var rlsActive bool
			Expect(operator.QueryRowContext(ctx, "SELECT pg_catalog.row_security_active($1::oid)", tableOID).
				Scan(&rlsActive)).To(Succeed())
			Expect(rlsActive).To(BeTrue(), "SET-only access must not inherit table ownership")
			var count int
			Expect(SQLState(operator.QueryRowContext(ctx, "SELECT count(*) FROM issue30_schema.payload").Scan(&count))).
				To(Equal("42501"), "unchanged administrator initially lacks table access")
			Expect(SQLState(operator.QueryRowContext(ctx, "SELECT public.issue30_invoker()").Scan(&count))).To(Equal("42501"))
			Expect(SQLState(target.QueryRowContext(ctx, "SELECT public.issue30_definer()").Scan(&count))).
				To(Equal("42501"), "existing operator-owned SECURITY DEFINER uses that operator's ambient authority")

			desired.Spec.Schemas[0].Owner = epco.SQLName(newOwner)
			databaseApply(ctx, session, desired)
			var actualDatabaseOID, actualSchemaOID, actualTableOID int64
			var schemaOwner, tableOwner string
			var value int
			Expect(target.QueryRowContext(ctx, `SELECT
				(SELECT oid FROM pg_database WHERE datname=current_database()),n.oid,c.oid,
				pg_get_userbyid(n.nspowner),pg_get_userbyid(c.relowner),p.value
				FROM pg_namespace n JOIN pg_class c ON c.relnamespace=n.oid
				JOIN issue30_schema.payload p ON true
				WHERE n.nspname='issue30_schema' AND c.relname='payload'`).
				Scan(&actualDatabaseOID, &actualSchemaOID, &actualTableOID, &schemaOwner, &tableOwner, &value)).To(Succeed())
			Expect([]int64{actualDatabaseOID, actualSchemaOID, actualTableOID}).
				To(Equal([]int64{databaseOID, schemaOID, tableOID}))
			Expect(schemaOwner).To(Equal(newOwner))
			Expect(tableOwner).To(Equal(oldOwner))
			Expect(value).To(Equal(42))
			Expect(target.QueryRowContext(ctx, `SELECT pg_has_role($1,$2,'SET'),
				has_database_privilege($1,current_database(),'CREATE')`,
				oldOwner, newOwner).Scan(&canSet, &canCreate)).To(Succeed())
			Expect(canSet || canCreate).To(BeFalse(), "repair must not grant CREATE or the new owner to the old owner")
			for _, owner := range []string{oldOwner, newOwner} {
				Expect(target.QueryRowContext(ctx, `SELECT rolsuper OR rolcreatedb OR rolcreaterole OR rolbypassrls
					FROM pg_roles WHERE rolname=$1`, owner).Scan(&canCreate)).To(Succeed())
				Expect(canCreate).To(BeFalse(), "application role attributes must not be elevated")
			}
			var globalInherit bool
			Expect(target.QueryRowContext(ctx, "SELECT rolinherit FROM pg_roles WHERE rolname=$1", f.Role).
				Scan(&globalInherit)).To(Succeed())
			Expect(globalInherit).To(BeFalse())
			Expect(operator.QueryRowContext(ctx, "SELECT pg_catalog.row_security_active($1::oid)", tableOID).
				Scan(&rlsActive)).To(Succeed())
			Expect(rlsActive).To(BeFalse(), "INHERIT exposes owner/RLS authority to the unchanged administrator")
			for _, query := range []string{"SELECT count(*) FROM issue30_schema.payload", "SELECT public.issue30_invoker()"} {
				Expect(operator.QueryRowContext(ctx, query).Scan(&count)).To(Succeed())
				Expect(count).To(Equal(1), "direct and invoker execution inherit table-owner authority")
			}
			Expect(target.QueryRowContext(ctx, "SELECT public.issue30_definer()").Scan(&count)).To(Succeed())
			Expect(count).To(Equal(1), "the preexisting operator-owned definer also sees expanded authority")
			_, err = target.ExecContext(ctx, "ALTER TABLE issue30_schema.payload FORCE ROW LEVEL SECURITY")
			Expect(err).NotTo(HaveOccurred())
			Expect(operator.QueryRowContext(ctx, "SELECT pg_catalog.row_security_active($1::oid)", tableOID).
				Scan(&rlsActive)).To(Succeed())
			Expect(rlsActive).To(BeTrue(), "service repair must not add BYPASSRLS")
			for _, query := range []string{"SELECT count(*) FROM issue30_schema.payload", "SELECT public.issue30_invoker()"} {
				Expect(operator.QueryRowContext(ctx, query).Scan(&count)).To(Succeed())
				Expect(count).To(BeZero(), "FORCE RLS still applies without BYPASSRLS")
			}
			Expect(target.QueryRowContext(ctx, "SELECT public.issue30_definer()").Scan(&count)).To(Succeed())
			Expect(count).To(BeZero(), "FORCE RLS also applies inside the existing definer")

			// Bootstrap-created ADMIN rows and the existing service SET bit survive.
			for _, owner := range []string{oldOwner, newOwner} {
				var admin, inherit, set bool
				Expect(target.QueryRowContext(ctx, `SELECT admin_option,inherit_option,set_option
					FROM pg_auth_members WHERE roleid=$1::regrole AND member=$2::regrole AND grantor=10`, owner, f.Role).
					Scan(&admin, &inherit, &set)).To(Succeed())
				Expect([]bool{admin, inherit, set}).To(Equal([]bool{true, false, false}))
			}
		},
		Entry("operator already has database CREATE, as in the Aurora report", true),
		Entry("operator also needs concrete database-owner inheritance for CREATE", false),
	)

	It("preserves an already-sufficient SET path through an external schema owner", func(ctx SpecContext) {
		oldOwner, newOwner := f.owner(ctx, true), f.owner(ctx, true)
		// Existing application authority is fixture input, not a repair performed by EPCO.
		roleSQL(ctx, f.Fixture, "GRANT "+quoteIdentifier(newOwner)+" TO "+quoteIdentifier(oldOwner)+
			" WITH INHERIT FALSE, SET TRUE")
		session := issue30SchemaSession(ctx, f, nil)
		name := f.name()
		desired := databaseDesired(name, oldOwner)
		desired.Spec.Schemas = []epco.SchemaSpec{schemaSpec("external_schema", oldOwner, epco.EnsurePresent)}
		databaseApply(ctx, session, desired)
		target := f.target(name)
		_, err := target.ExecContext(ctx, "SET ROLE "+quoteIdentifier(oldOwner)+
			"; CREATE TABLE external_schema.payload(value int); INSERT INTO external_schema.payload VALUES (42); RESET ROLE")
		Expect(err).NotTo(HaveOccurred())
		desired.Spec.Schemas[0].Owner = epco.SQLName(newOwner)
		databaseApply(ctx, session, desired)
		var schemaOwner, tableOwner string
		var value int
		Expect(target.QueryRowContext(ctx, `SELECT pg_get_userbyid(n.nspowner),pg_get_userbyid(c.relowner),p.value
			FROM pg_namespace n JOIN pg_class c ON c.relnamespace=n.oid
			JOIN external_schema.payload p ON true
			WHERE n.nspname='external_schema' AND c.relname='payload'`).Scan(&schemaOwner, &tableOwner, &value)).To(Succeed())
		Expect(schemaOwner).To(Equal(newOwner))
		Expect(tableOwner).To(Equal(oldOwner))
		Expect(value).To(Equal(42))
		var inherited bool
		Expect(target.QueryRowContext(ctx, "SELECT pg_has_role($1,$2,'USAGE')", f.Role, oldOwner).
			Scan(&inherited)).To(Succeed())
		Expect(inherited).To(BeFalse(), "existing sufficient SET authority must not acquire an INHERIT edge")
	}, NodeTimeout(30*time.Second))

	It("resolves public ownership without granting pg_database_owner", func(ctx SpecContext) {
		oldOwner, newOwner := issue30ManagedOwner(ctx, f), issue30ManagedOwner(ctx, f)
		session := issue30SchemaSession(ctx, f, map[string]bool{oldOwner: true, newOwner: true})
		name := f.name()
		desired := databaseDesired(name, oldOwner)
		databaseApply(ctx, session, desired)
		target := f.target(name)
		var originalOwner string
		var originalOID int64
		Expect(target.QueryRowContext(ctx, "SELECT pg_get_userbyid(nspowner),oid FROM pg_namespace WHERE nspname='public'").
			Scan(&originalOwner, &originalOID)).To(Succeed())
		Expect(originalOwner).To(Equal("pg_database_owner"))
		_, err := target.ExecContext(ctx, "SET ROLE "+quoteIdentifier(oldOwner)+
			"; CREATE TABLE public.payload(value int); INSERT INTO public.payload VALUES (42); RESET ROLE")
		Expect(err).NotTo(HaveOccurred())
		desired.Spec.Schemas = []epco.SchemaSpec{schemaSpec("public", newOwner, epco.EnsurePresent)}
		databaseApply(ctx, session, desired)
		var owner string
		var oid int64
		var value, explicitMembers int
		Expect(target.QueryRowContext(ctx, "SELECT pg_get_userbyid(nspowner),oid FROM pg_namespace WHERE nspname='public'").
			Scan(&owner, &oid)).To(Succeed())
		Expect(owner).To(Equal(newOwner))
		Expect(oid).To(Equal(originalOID))
		Expect(target.QueryRowContext(ctx, "SELECT value FROM public.payload").Scan(&value)).To(Succeed())
		Expect(value).To(Equal(42))
		Expect(target.QueryRowContext(ctx, "SELECT count(*) FROM pg_auth_members WHERE roleid='pg_database_owner'::regrole").
			Scan(&explicitMembers)).To(Succeed())
		Expect(explicitMembers).To(BeZero(), "the implicit-only pseudo-role must never receive explicit members")
	}, NodeTimeout(30*time.Second))
})
