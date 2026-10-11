//go:build integration

package integration

import (
	"context"
	"database/sql"
	"errors"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	epco "github.com/vterdunov/external-postgres-control-operator/api/v1alpha1"
	pgroles "github.com/vterdunov/external-postgres-control-operator/internal/postgres"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	roleSQLAdmin               = "postgres"
	roleTLSMode                = "verify-full"
	roleCAKey                  = "ca.crt"
	roleWorkMemory             = "3MB"
	roleWorkMemorySetting      = "work_mem"
	roleUpperWorkMemorySetting = "WORK_MEM"
	roleOtherWorkMemory        = "4MB"
)

func roleSQL(ctx context.Context, f *Fixture, query string) {
	_, err := f.Admin.ExecContext(ctx, query)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
}

func roleName(f *Fixture) string {
	name, err := randomName("managed_")
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	DeferCleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		Expect(f.Server.withCleanupConnection(cleanup, f.Database, func(connection *sql.Conn) error {
			var exists bool
			queryErr := connection.QueryRowContext(cleanup,
				"SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname=$1)", name).Scan(&exists)
			if !exists && queryErr == nil {
				return nil
			}
			_, ownedErr := connection.ExecContext(cleanup, "DROP OWNED BY "+quoteIdentifier(name))
			_, roleErr := connection.ExecContext(cleanup, "DROP ROLE "+quoteIdentifier(name))
			return errors.Join(queryErr, ownedErr, roleErr)
		})).To(Succeed())
	})
	return name
}

func roleSession(ctx context.Context, f *Fixture, restricted bool) pgroles.RoleSession {
	config := productionConfig(f, roleTLSMode, false)
	if !restricted {
		config.Connection.User = roleSQLAdmin
		config.Connection.Password = f.Server.password
	}
	session, err := pgroles.NewRoleFactory().Open(ctx, config)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	DeferCleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		Expect(session.Close(cleanup)).To(Succeed())
	})
	return session
}

func roleExists(ctx context.Context, f *Fixture, name string) bool {
	exists, err := f.Server.RoleExists(ctx, name)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	return exists
}

func roleLogin(ctx context.Context, f *Fixture, name, password string) string {
	db, err := f.Server.Open(f.Database, name, password)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	defer func() { Expect(db.Close()).To(Succeed()) }()
	return SQLState(db.PingContext(ctx))
}

func roleMemberships(ctx context.Context, f *Fixture, name string) []string {
	rows, err := f.Admin.QueryContext(ctx, `SELECT parent.rolname FROM pg_auth_members binding
		JOIN pg_roles parent ON parent.oid=binding.roleid
		JOIN pg_roles member ON member.oid=binding.member WHERE member.rolname=$1 ORDER BY parent.rolname`, name)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	defer func() { Expect(rows.Close()).To(Succeed()) }()
	var names []string
	for rows.Next() {
		var parent string
		Expect(rows.Scan(&parent)).To(Succeed())
		names = append(names, parent)
	}
	Expect(rows.Err()).NotTo(HaveOccurred())
	return names
}

func roleSettings(ctx context.Context, f *Fixture, name string, databaseSpecific bool) []string {
	condition := "settings.setdatabase=0"
	if databaseSpecific {
		condition = "settings.setdatabase=(SELECT oid FROM pg_database WHERE datname=current_database())"
	}
	rows, err := f.Admin.QueryContext(ctx, `SELECT unnest(settings.setconfig) FROM pg_db_role_setting settings
		JOIN pg_roles role ON role.oid=settings.setrole WHERE role.rolname=$1 AND `+condition, name)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	defer func() { Expect(rows.Close()).To(Succeed()) }()
	var settings []string
	for rows.Next() {
		var setting string
		Expect(rows.Scan(&setting)).To(Succeed())
		settings = append(settings, setting)
	}
	Expect(rows.Err()).NotTo(HaveOccurred())
	return settings
}

var _ = Describe("DatabaseRole PostgreSQL reconciliation", func() {
	var fixture *Fixture
	BeforeEach(func(ctx SpecContext) {
		var err error
		fixture, err = postgres.NewFixture(ctx)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			cleanup, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			Expect(fixture.Close(cleanup)).To(Succeed())
		})
	}, NodeTimeout(20*time.Second))

	It("creates all role attributes and converges existing comment and expiration changes", func(ctx SpecContext) {
		name := roleName(fixture)
		session := roleSession(ctx, fixture, false)
		expires := time.Date(2035, 1, 2, 3, 4, 5, 0, time.UTC)
		desired := pgroles.RoleDesired{Name: name, Spec: epco.DatabaseRoleSpec{
			Login: true, Inherit: new(false), Superuser: true, CreateDB: true,
			CreateRole: true, Replication: true, BypassRLS: true, ConnectionLimit: new(int64(7)),
			Comment: new("role's description"), ValidUntil: &metav1.Time{Time: expires},
		}}
		_, err := session.Apply(ctx, desired)
		Expect(err).NotTo(HaveOccurred())
		var login, inherit, superuser, createdb, createrole, replication, bypass bool
		var limit int64
		var comment string
		var validUntil time.Time
		Expect(fixture.Admin.QueryRowContext(ctx, `SELECT rolcanlogin,rolinherit,rolsuper,rolcreatedb,
			rolcreaterole,rolreplication,rolbypassrls,rolconnlimit,
			shobj_description(oid,'pg_authid'),rolvaliduntil FROM pg_roles WHERE rolname=$1`, name).
			Scan(&login, &inherit, &superuser, &createdb, &createrole,
				&replication, &bypass, &limit, &comment, &validUntil)).To(Succeed())
		Expect([]bool{login, inherit, superuser, createdb, createrole, replication, bypass}).
			To(Equal([]bool{true, false, true, true, true, true, true}))
		Expect(limit).To(Equal(int64(7)))
		Expect(comment).To(Equal("role's description"))
		Expect(validUntil.Equal(expires)).To(BeTrue())
		desired.Spec = epco.DatabaseRoleSpec{Comment: new("")}
		_, err = session.Apply(ctx, desired)
		Expect(err).NotTo(HaveOccurred())
		state, err := session.Inspect(ctx, name)
		Expect(err).NotTo(HaveOccurred())
		Expect(state.Exists).To(BeTrue())
		Expect(state.Login || state.Superuser || state.CreateDB ||
			state.CreateRole || state.Replication || state.BypassRLS).To(BeFalse())
		Expect(state.Inherit).To(BeTrue())
		Expect(state.ConnectionLimit).To(Equal(int64(-1)))
		Expect(state.Comment).To(BeEmpty())
		Expect(state.ValidUntil).To(BeNil())
	}, NodeTimeout(25*time.Second))

	It("uses pg_roles for restricted administration and alters only changed attributes", func(ctx SpecContext) {
		name := roleName(fixture)
		roleSQL(ctx, fixture, "ALTER ROLE "+quoteIdentifier(fixture.Role)+" CREATEROLE")
		roleSQL(ctx, fixture, "CREATE ROLE "+quoteIdentifier(name)+" NOLOGIN")
		roleSQL(ctx, fixture, "GRANT "+quoteIdentifier(name)+" TO "+quoteIdentifier(fixture.Role)+" WITH ADMIN OPTION")
		var secret string
		err := fixture.Limited.QueryRowContext(ctx,
			"SELECT rolpassword FROM pg_authid WHERE rolname=$1", name).Scan(&secret)
		Expect(SQLState(err)).To(Equal("42501"))
		session := roleSession(ctx, fixture, true)
		state, err := session.Inspect(ctx, name)
		Expect(err).NotTo(HaveOccurred())
		Expect(state.Exists).To(BeTrue())
		_, err = session.Apply(ctx, pgroles.RoleDesired{Name: name, Spec: epco.DatabaseRoleSpec{Login: true}})
		Expect(err).NotTo(HaveOccurred(), "unchanged NOSUPERUSER/NOREPLICATION/NOBYPASSRLS must not be emitted")
		var login, superuser, replication, bypass bool
		Expect(fixture.Admin.QueryRowContext(ctx,
			"SELECT rolcanlogin,rolsuper,rolreplication,rolbypassrls FROM pg_roles WHERE rolname=$1", name).
			Scan(&login, &superuser, &replication, &bypass)).To(Succeed())
		Expect(login).To(BeTrue())
		Expect(superuser || replication || bypass).To(BeFalse())
	}, NodeTimeout(25*time.Second))

	DescribeTable("replaces negative infinity password expiration with desired state",
		func(ctx SpecContext, finite bool) {
			name := roleName(fixture)
			roleSQL(ctx, fixture, "CREATE ROLE "+quoteIdentifier(name)+" VALID UNTIL '-infinity'")
			session := roleSession(ctx, fixture, false)
			spec := epco.DatabaseRoleSpec{}
			expires := time.Date(2035, 2, 3, 4, 5, 6, 0, time.UTC)
			if finite {
				spec.ValidUntil = &metav1.Time{Time: expires}
			}
			_, err := session.Apply(ctx, pgroles.RoleDesired{Name: name, Spec: spec})
			Expect(err).NotTo(HaveOccurred())
			if finite {
				var actual time.Time
				Expect(fixture.Admin.QueryRowContext(ctx,
					"SELECT rolvaliduntil FROM pg_roles WHERE rolname=$1", name).Scan(&actual)).To(Succeed())
				Expect(actual.Equal(expires)).To(BeTrue())
			} else {
				var actual string
				Expect(fixture.Admin.QueryRowContext(ctx,
					"SELECT rolvaliduntil::text FROM pg_roles WHERE rolname=$1", name).Scan(&actual)).To(Succeed())
				Expect(actual).To(Equal("infinity"), "omitted validUntil must remove expiration rather than keep '-infinity'")
			}
		},
		Entry("omitted validUntil means no expiration", false),
		Entry("explicit finite validUntil restores the requested expiration", true),
	)

	It("requires parent authorization and retries missing dependencies", func(ctx SpecContext) {
		name, parent := roleName(fixture), roleName(fixture)
		roleSQL(ctx, fixture, "ALTER ROLE "+quoteIdentifier(fixture.Role)+" CREATEROLE")
		roleSQL(ctx, fixture, "CREATE ROLE "+quoteIdentifier(name))
		roleSQL(ctx, fixture, "GRANT "+quoteIdentifier(name)+" TO "+quoteIdentifier(fixture.Role)+" WITH ADMIN OPTION")
		session := roleSession(ctx, fixture, true)
		desired := pgroles.RoleDesired{Name: name, Spec: epco.DatabaseRoleSpec{InRoles: []epco.SQLName{epco.SQLName(parent)}}}
		_, err := session.Apply(ctx, desired)
		Expect(err).To(HaveOccurred())
		Expect(roleMemberships(ctx, fixture, name)).To(BeEmpty())
		roleSQL(ctx, fixture, "CREATE ROLE "+quoteIdentifier(parent))
		var version int
		Expect(fixture.Admin.QueryRowContext(ctx, "SHOW server_version_num").Scan(&version)).To(Succeed())
		if version >= 160000 {
			_, err = session.Apply(ctx, desired)
			Expect(err).To(HaveOccurred(), "target ADMIN OPTION must not imply authority over a parent")
			Expect(roleMemberships(ctx, fixture, name)).To(BeEmpty())
		}
		roleSQL(ctx, fixture, "GRANT "+quoteIdentifier(parent)+" TO "+quoteIdentifier(fixture.Role)+" WITH ADMIN OPTION")
		_, err = session.Apply(ctx, desired)
		Expect(err).NotTo(HaveOccurred())
		Expect(roleMemberships(ctx, fixture, name)).To(ConsistOf(parent))
		// Remove the operator-issued application grant before dropping its grantor.
		roleSQL(ctx, fixture, "REVOKE "+quoteIdentifier(parent)+" FROM "+quoteIdentifier(name)+" CASCADE")
	}, NodeTimeout(25*time.Second))

	It("removes undeclared authoritative memberships while preserving administrative access", func(ctx SpecContext) {
		name, declared, independent := roleName(fixture), roleName(fixture), roleName(fixture)
		for _, role := range []string{name, declared, independent} {
			roleSQL(ctx, fixture, "CREATE ROLE "+quoteIdentifier(role))
		}
		roleSQL(ctx, fixture, "GRANT "+quoteIdentifier(independent)+" TO "+quoteIdentifier(name))
		roleSQL(ctx, fixture, "GRANT "+quoteIdentifier(name)+" TO "+quoteIdentifier(fixture.Role)+" WITH ADMIN OPTION")
		session := roleSession(ctx, fixture, false)
		_, err := session.Apply(ctx, pgroles.RoleDesired{Name: name, Spec: epco.DatabaseRoleSpec{
			InRoles: []epco.SQLName{epco.SQLName(declared)}, MembershipPolicy: epco.MembershipAuthoritative}})
		Expect(err).NotTo(HaveOccurred())
		Expect(roleMemberships(ctx, fixture, name)).To(ConsistOf(declared))
		Expect(roleMemberships(ctx, fixture, fixture.Role)).To(ContainElement(name))
	}, NodeTimeout(25*time.Second))

	It("cleans removed managed bindings after reopening while preserving unrelated grants", func(ctx SpecContext) {
		name, declared, independent := roleName(fixture), roleName(fixture), roleName(fixture)
		for _, role := range []string{name, declared, independent} {
			roleSQL(ctx, fixture, "CREATE ROLE "+quoteIdentifier(role))
		}
		roleSQL(ctx, fixture, "GRANT "+quoteIdentifier(independent)+" TO "+quoteIdentifier(name))
		session := roleSession(ctx, fixture, false)
		result, err := session.Apply(ctx, pgroles.RoleDesired{Name: name, Spec: epco.DatabaseRoleSpec{
			InRoles: []epco.SQLName{epco.SQLName(declared)}, MembershipPolicy: epco.MembershipManaged}})
		Expect(err).NotTo(HaveOccurred())
		Expect(roleMemberships(ctx, fixture, name)).To(ConsistOf(declared, independent))
		Expect(result.Memberships).NotTo(BeEmpty())
		Expect(session.Close(ctx)).To(Succeed())
		restarted := roleSession(ctx, fixture, false)
		_, err = restarted.Apply(ctx, pgroles.RoleDesired{Name: name,
			Spec:               epco.DatabaseRoleSpec{MembershipPolicy: epco.MembershipManaged},
			TrackedMemberships: result.Memberships})
		Expect(err).NotTo(HaveOccurred())
		Expect(roleMemberships(ctx, fixture, name)).To(ConsistOf(independent))
	}, NodeTimeout(25*time.Second))

	It("keeps existing PG16 membership INHERIT and SET options when the role attribute changes", func(ctx SpecContext) {
		var version int
		Expect(fixture.Admin.QueryRowContext(ctx, "SHOW server_version_num").Scan(&version)).To(Succeed())
		if version < 160000 {
			Skip("membership options were introduced in PostgreSQL 16")
		}
		name, parent := roleName(fixture), roleName(fixture)
		roleSQL(ctx, fixture, "CREATE ROLE "+quoteIdentifier(name)+" INHERIT")
		roleSQL(ctx, fixture, "CREATE ROLE "+quoteIdentifier(parent))
		roleSQL(ctx, fixture, "GRANT "+quoteIdentifier(parent)+" TO "+quoteIdentifier(name)+" WITH INHERIT FALSE, SET TRUE")
		session := roleSession(ctx, fixture, false)
		_, err := session.Apply(ctx, pgroles.RoleDesired{Name: name, Spec: epco.DatabaseRoleSpec{
			Inherit: new(false), InRoles: []epco.SQLName{epco.SQLName(parent)}}})
		Expect(err).NotTo(HaveOccurred())
		var inheritOption, setOption, adminOption bool
		Expect(fixture.Admin.QueryRowContext(ctx, `SELECT inherit_option,set_option,admin_option FROM pg_auth_members
			WHERE roleid=(SELECT oid FROM pg_roles WHERE rolname=$1)
			AND member=(SELECT oid FROM pg_roles WHERE rolname=$2)`, parent, name).
			Scan(&inheritOption, &setOption, &adminOption)).To(Succeed())
		Expect(inheritOption).To(BeFalse())
		Expect(setOption).To(BeTrue())
		Expect(adminOption).To(BeFalse())
	}, NodeTimeout(25*time.Second))

	It("preserves independent PG16 grantors and reports unrevokeable grants", func(ctx SpecContext) {
		var version int
		Expect(fixture.Admin.QueryRowContext(ctx, "SHOW server_version_num").Scan(&version)).To(Succeed())
		if version < 160000 {
			Skip("independent grantor bindings were introduced in PostgreSQL 16")
		}
		name, parent := roleName(fixture), roleName(fixture)
		roleSQL(ctx, fixture, "ALTER ROLE "+quoteIdentifier(fixture.Role)+" CREATEROLE")
		for _, role := range []string{name, parent} {
			roleSQL(ctx, fixture, "CREATE ROLE "+quoteIdentifier(role))
			roleSQL(ctx, fixture, "GRANT "+quoteIdentifier(role)+" TO "+quoteIdentifier(fixture.Role)+" WITH ADMIN OPTION")
		}
		session := roleSession(ctx, fixture, true)
		result, err := session.Apply(ctx, pgroles.RoleDesired{Name: name, Spec: epco.DatabaseRoleSpec{
			MembershipPolicy: epco.MembershipManaged, InRoles: []epco.SQLName{epco.SQLName(parent)}}})
		Expect(err).NotTo(HaveOccurred())
		roleSQL(ctx, fixture, "GRANT "+quoteIdentifier(parent)+" TO "+quoteIdentifier(name)+" GRANTED BY postgres")
		_, err = session.Apply(ctx, pgroles.RoleDesired{Name: name,
			Spec: epco.DatabaseRoleSpec{MembershipPolicy: epco.MembershipManaged}, TrackedMemberships: result.Memberships})
		Expect(err).NotTo(HaveOccurred())
		var count int
		Expect(fixture.Admin.QueryRowContext(ctx, `SELECT count(*) FROM pg_auth_members
			WHERE roleid=(SELECT oid FROM pg_roles WHERE rolname=$1) AND member=(SELECT oid FROM pg_roles WHERE rolname=$2)
			AND grantor=(SELECT oid FROM pg_roles WHERE rolname='postgres')`, parent, name).Scan(&count)).To(Succeed())
		Expect(count).To(Equal(1), "independent grantor must survive managed cleanup")
		Expect(fixture.Admin.QueryRowContext(ctx, `SELECT count(*) FROM pg_auth_members
			WHERE roleid=(SELECT oid FROM pg_roles WHERE rolname=$1) AND member=(SELECT oid FROM pg_roles WHERE rolname=$2)
			AND grantor=(SELECT oid FROM pg_roles WHERE rolname=$3)`, parent, name, fixture.Role).Scan(&count)).To(Succeed())
		Expect(count).To(BeZero())
		_, err = session.Apply(ctx, pgroles.RoleDesired{Name: name,
			Spec: epco.DatabaseRoleSpec{MembershipPolicy: epco.MembershipAuthoritative}})
		Expect(err).To(HaveOccurred(), "unchanged independent grants cannot indicate successful authoritative cleanup")
		Expect(roleMemberships(ctx, fixture, name)).To(ConsistOf(parent))
	}, NodeTimeout(25*time.Second))

	It("preserves independent bindings after managed declaration and removal", func(ctx SpecContext) {
		name, parent := roleName(fixture), roleName(fixture)
		roleSQL(ctx, fixture, "ALTER ROLE "+quoteIdentifier(fixture.Role)+" CREATEROLE")
		for _, role := range []string{name, parent} {
			roleSQL(ctx, fixture, "CREATE ROLE "+quoteIdentifier(role))
			roleSQL(ctx, fixture, "GRANT "+quoteIdentifier(role)+" TO "+quoteIdentifier(fixture.Role)+" WITH ADMIN OPTION")
		}
		roleSQL(ctx, fixture, "GRANT "+quoteIdentifier(parent)+" TO "+quoteIdentifier(name))
		session := roleSession(ctx, fixture, true)
		result, err := session.Apply(ctx, pgroles.RoleDesired{Name: name, Spec: epco.DatabaseRoleSpec{
			MembershipPolicy: epco.MembershipManaged, InRoles: []epco.SQLName{epco.SQLName(parent)}}})
		Expect(err).NotTo(HaveOccurred())
		_, err = session.Apply(ctx, pgroles.RoleDesired{Name: name,
			Spec: epco.DatabaseRoleSpec{MembershipPolicy: epco.MembershipManaged}, TrackedMemberships: result.Memberships})
		Expect(err).NotTo(HaveOccurred())
		Expect(roleMemberships(ctx, fixture, name)).To(ConsistOf(parent),
			"declaration must not adopt and remove an independent grant")
		var grantor string
		Expect(fixture.Admin.QueryRowContext(ctx, `SELECT grantor.rolname FROM pg_auth_members binding
			JOIN pg_roles grantor ON grantor.oid=binding.grantor
			WHERE binding.roleid=(SELECT oid FROM pg_roles WHERE rolname=$1)
			AND binding.member=(SELECT oid FROM pg_roles WHERE rolname=$2)`, parent, name).Scan(&grantor)).To(Succeed())
		Expect(grantor).To(Equal(roleSQLAdmin))
	}, NodeTimeout(25*time.Second))

	It("resets only removed tracked role-wide parameters and preserves independent settings", func(ctx SpecContext) {
		name := roleName(fixture)
		roleSQL(ctx, fixture, "CREATE ROLE "+quoteIdentifier(name))
		roleSQL(ctx, fixture, "ALTER ROLE "+quoteIdentifier(name)+" SET search_path TO 'public'")
		roleSQL(ctx, fixture, "ALTER ROLE "+quoteIdentifier(name)+
			" IN DATABASE "+quoteIdentifier(fixture.Database)+" SET work_mem TO '9MB'")
		session := roleSession(ctx, fixture, false)
		result, err := session.Apply(ctx, pgroles.RoleDesired{Name: name, Spec: epco.DatabaseRoleSpec{
			Parameters: map[string]string{roleWorkMemorySetting: roleWorkMemory, "statement_timeout": "5s"}}})
		Expect(err).NotTo(HaveOccurred())
		Expect(result.Parameters).To(ConsistOf(roleWorkMemorySetting, "statement_timeout"))
		Expect(roleSettings(ctx, fixture, name, false)).
			To(ConsistOf("search_path=public", "work_mem=3MB", "statement_timeout=5s"))
		_, err = session.Apply(ctx, pgroles.RoleDesired{Name: name, Spec: epco.DatabaseRoleSpec{
			Parameters: map[string]string{"statement_timeout": "2s"}}, TrackedParameters: result.Parameters})
		Expect(err).NotTo(HaveOccurred())
		Expect(roleSettings(ctx, fixture, name, false)).To(ConsistOf("search_path=public", "statement_timeout=2s"))
		Expect(roleSettings(ctx, fixture, name, true)).To(ConsistOf("work_mem=9MB"))
	}, NodeTimeout(25*time.Second))

	It("rolls back partial mutations on invalid settings and retries with corrected input", func(ctx SpecContext) {
		name := roleName(fixture)
		roleSQL(ctx, fixture, "CREATE ROLE "+quoteIdentifier(name)+" NOLOGIN")
		session := roleSession(ctx, fixture, false)
		desired := pgroles.RoleDesired{Name: name, Spec: epco.DatabaseRoleSpec{Login: true,
			Parameters: map[string]string{roleWorkMemorySetting: "not-a-size"}}}
		_, err := session.Apply(ctx, desired)
		Expect(err).To(HaveOccurred())
		var login bool
		Expect(fixture.Admin.QueryRowContext(ctx,
			"SELECT rolcanlogin FROM pg_roles WHERE rolname=$1", name).Scan(&login)).To(Succeed())
		Expect(login).To(BeFalse(), "attribute mutation must not survive a failed transactional apply")
		Expect(roleSettings(ctx, fixture, name, false)).To(BeEmpty())
		desired.Spec.Parameters[roleWorkMemorySetting] = roleOtherWorkMemory
		_, err = session.Apply(ctx, desired)
		Expect(err).NotTo(HaveOccurred())
		Expect(roleSettings(ctx, fixture, name, false)).To(ConsistOf("work_mem=4MB"))
		_, err = session.Apply(ctx, desired)
		Expect(err).NotTo(HaveOccurred(), "retry must remain idempotent")
	}, NodeTimeout(25*time.Second))

	It("cleans uppercase managed parameter names after declaration removal", func(ctx SpecContext) {
		name := roleName(fixture)
		session := roleSession(ctx, fixture, false)
		result, err := session.Apply(ctx, pgroles.RoleDesired{Name: name, Spec: epco.DatabaseRoleSpec{
			Parameters: map[string]string{roleUpperWorkMemorySetting: roleWorkMemory}}})
		Expect(err).NotTo(HaveOccurred())
		Expect(roleSettings(ctx, fixture, name, false)).To(ConsistOf("work_mem=3MB"))
		_, err = session.Apply(ctx, pgroles.RoleDesired{Name: name, TrackedParameters: result.Parameters})
		Expect(err).NotTo(HaveOccurred())
		Expect(roleSettings(ctx, fixture, name, false)).To(BeEmpty(),
			"PostgreSQL normalizes setting names; removed uppercase declarations must still RESET the actual setting")
	}, NodeTimeout(25*time.Second))

	It("rejects conflicting parameter aliases without mutating SQL", func(ctx SpecContext) {
		name := roleName(fixture)
		roleSQL(ctx, fixture, "CREATE ROLE "+quoteIdentifier(name)+" NOLOGIN")
		session := roleSession(ctx, fixture, false)
		_, err := session.Apply(ctx, pgroles.RoleDesired{Name: name, Spec: epco.DatabaseRoleSpec{
			Login: true, Parameters: map[string]string{roleUpperWorkMemorySetting: roleWorkMemory,
				roleWorkMemorySetting: roleOtherWorkMemory}}})
		Expect(failureReason(err)).To(Equal(pgroles.InvalidConfiguration))
		Expect(roleSettings(ctx, fixture, name, false)).To(BeEmpty())
		var login bool
		Expect(fixture.Admin.QueryRowContext(ctx,
			"SELECT rolcanlogin FROM pg_roles WHERE rolname=$1", name).Scan(&login)).To(Succeed())
		Expect(login).To(BeFalse())
	}, NodeTimeout(25*time.Second))

	DescribeTable("cleans legacy parameter aliases without changing independent settings",
		func(ctx SpecContext, alias, canonical string) {
			name := roleName(fixture)
			roleSQL(ctx, fixture, "CREATE ROLE "+quoteIdentifier(name))
			roleSQL(ctx, fixture, "ALTER ROLE "+quoteIdentifier(name)+" SET search_path TO 'public'")
			roleSQL(ctx, fixture, "ALTER ROLE "+quoteIdentifier(name)+
				" IN DATABASE "+quoteIdentifier(fixture.Database)+" SET "+quoteIdentifier(canonical)+" TO '9MB'")
			session := roleSession(ctx, fixture, false)
			result, err := session.Apply(ctx, pgroles.RoleDesired{Name: name,
				Spec: epco.DatabaseRoleSpec{Parameters: map[string]string{alias: roleWorkMemory}}})
			Expect(err).NotTo(HaveOccurred())
			Expect(roleSettings(ctx, fixture, name, false)).To(ConsistOf("search_path=public", canonical+"=3MB"))
			_, err = session.Apply(ctx, pgroles.RoleDesired{Name: name, TrackedParameters: result.Parameters})
			Expect(err).NotTo(HaveOccurred())
			Expect(roleSettings(ctx, fixture, name, false)).To(ConsistOf("search_path=public"))
			Expect(roleSettings(ctx, fixture, name, true)).To(ConsistOf(canonical + "=9MB"))
		},
		Entry("sort_mem maps to work_mem", "sort_mem", roleWorkMemorySetting),
		Entry("vacuum_mem maps to maintenance_work_mem", "vacuum_mem", "maintenance_work_mem"),
	)

	DescribeTable("rejects conflicting legacy and canonical parameter declarations without mutation",
		func(ctx SpecContext, alias, canonical string) {
			name := roleName(fixture)
			roleSQL(ctx, fixture, "CREATE ROLE "+quoteIdentifier(name)+" NOLOGIN")
			session := roleSession(ctx, fixture, false)
			_, err := session.Apply(ctx, pgroles.RoleDesired{Name: name,
				Spec: epco.DatabaseRoleSpec{Login: true,
					Parameters: map[string]string{alias: roleWorkMemory, canonical: roleOtherWorkMemory}}})
			Expect(failureReason(err)).To(Equal(pgroles.InvalidConfiguration))
			Expect(roleSettings(ctx, fixture, name, false)).To(BeEmpty())
			var login bool
			Expect(fixture.Admin.QueryRowContext(ctx,
				"SELECT rolcanlogin FROM pg_roles WHERE rolname=$1", name).Scan(&login)).To(Succeed())
			Expect(login).To(BeFalse())
		},
		Entry("sort_mem conflicts with work_mem", "sort_mem", roleWorkMemorySetting),
		Entry("vacuum_mem conflicts with maintenance_work_mem", "vacuum_mem", "maintenance_work_mem"),
	)

	It("authenticates quoted passwords, rotates, preserves unmanaged passwords and disables them", func(ctx SpecContext) {
		name := roleName(fixture)
		session := roleSession(ctx, fixture, false)
		desired := pgroles.RoleDesired{Name: name,
			Spec: epco.DatabaseRoleSpec{Login: true}, Password: new("first ' quoted \\ password")}
		_, err := session.Apply(ctx, desired)
		Expect(err).NotTo(HaveOccurred())
		roleSQL(ctx, fixture, "GRANT CONNECT ON DATABASE "+quoteIdentifier(fixture.Database)+" TO "+quoteIdentifier(name))
		Expect(roleLogin(ctx, fixture, name, *desired.Password)).To(BeEmpty())
		old := *desired.Password
		desired.Password = new("second:password/with?symbols")
		_, err = session.Apply(ctx, desired)
		Expect(err).NotTo(HaveOccurred())
		Expect(roleLogin(ctx, fixture, name, old)).To(Equal("28P01"))
		Expect(roleLogin(ctx, fixture, name, *desired.Password)).To(BeEmpty())
		current := *desired.Password
		desired.Password = nil
		desired.Spec.Comment = new("unmanaged password")
		_, err = session.Apply(ctx, desired)
		Expect(err).NotTo(HaveOccurred())
		Expect(roleLogin(ctx, fixture, name, current)).To(BeEmpty())
		desired.Spec.DisablePassword = true
		_, err = session.Apply(ctx, desired)
		Expect(err).NotTo(HaveOccurred())
		Expect(roleLogin(ctx, fixture, name, current)).To(Equal("28P01"))
		var password sql.NullString
		Expect(fixture.Admin.QueryRowContext(ctx,
			"SELECT rolpassword FROM pg_authid WHERE rolname=$1", name).Scan(&password)).To(Succeed())
		Expect(password.Valid).To(BeFalse())
	}, NodeTimeout(25*time.Second))

	It("never implicitly enables login for a password-bearing or source-free group", func(ctx SpecContext) {
		session := roleSession(ctx, fixture, false)
		for _, password := range []*string{nil, new("group-password")} {
			name := roleName(fixture)
			_, err := session.Apply(ctx, pgroles.RoleDesired{Name: name, Password: password})
			Expect(err).NotTo(HaveOccurred())
			var login bool
			Expect(fixture.Admin.QueryRowContext(ctx,
				"SELECT rolcanlogin FROM pg_roles WHERE rolname=$1", name).Scan(&login)).To(Succeed())
			Expect(login).To(BeFalse())
		}
	}, NodeTimeout(25*time.Second))

	It("accepts Unicode password normalization through pgx and independent libpq authentication", func(ctx SpecContext) {
		name := roleName(fixture)
		session := roleSession(ctx, fixture, false)
		password := "unicode\u00adpassword"
		_, err := session.Apply(ctx, pgroles.RoleDesired{Name: name,
			Spec: epco.DatabaseRoleSpec{Login: true}, Password: &password})
		Expect(err).NotTo(HaveOccurred())
		roleSQL(ctx, fixture, "GRANT CONNECT ON DATABASE "+quoteIdentifier(fixture.Database)+" TO "+quoteIdentifier(name))
		// Pinned pgx uses PRECIS rather than SASLprep and falls back to the raw
		// password on U+00AD. Its normalized spelling still authenticates.
		Expect(roleLogin(ctx, fixture, name, password)).To(Equal("28P01"))
		Expect(roleLogin(ctx, fixture, name, "unicodepassword")).To(BeEmpty())
		// Feed the password over stdin so neither Docker arguments nor fixture logs contain it.
		for _, candidate := range []string{password, "unicodepassword"} {
			command := exec.CommandContext(ctx, "docker", "exec", "-i", fixture.Server.Container,
				"sh", "-c", `IFS= read -r PGPASSWORD; export PGPASSWORD; export PGSSLMODE=require; exec psql "$@"`,
				"epco-libpq-test", "-h", loopbackHost, "-U", name, "-d", fixture.Database,
				"-X", "-A", "-t", "-c", "SELECT 1")
			command.Stdin = strings.NewReader(candidate + "\n")
			output, err := command.Output()
			Expect(err).NotTo(HaveOccurred())
			Expect(strings.TrimSpace(string(output))).To(Equal("1"))
		}
	}, NodeTimeout(25*time.Second))

	It("protects predefined, service and administrative roles from mutation and drop", func(ctx SpecContext) {
		service := "rds_" + strings.TrimPrefix(roleName(fixture), "managed_")
		roleSQL(ctx, fixture, "CREATE ROLE "+quoteIdentifier(service))
		DeferCleanup(func() {
			cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			Expect(fixture.Server.cleanupOwnedSQL(cleanup, nil, []string{service})).To(Succeed())
		})
		session := roleSession(ctx, fixture, false)
		for _, name := range []string{roleSQLAdmin, "pg_monitor", service} {
			_, err := session.Apply(ctx, pgroles.RoleDesired{Name: name,
				Spec: epco.DatabaseRoleSpec{Comment: new("must not apply")}})
			Expect(err).To(HaveOccurred())
			Expect(session.Drop(ctx, name)).NotTo(Succeed())
			Expect(roleExists(ctx, fixture, name)).To(BeTrue())
			var comment sql.NullString
			Expect(fixture.Admin.QueryRowContext(ctx,
				"SELECT shobj_description(oid,'pg_authid') FROM pg_roles WHERE rolname=$1", name).Scan(&comment)).To(Succeed())
			Expect(comment.String).NotTo(Equal("must not apply"))
		}
	}, NodeTimeout(25*time.Second))

	It("blocks DROP dependencies, preserves sessions and retries safely", func(ctx SpecContext) {
		name := roleName(fixture)
		roleSQL(ctx, fixture, "CREATE ROLE "+quoteIdentifier(name)+" LOGIN PASSWORD 'owner-password'")
		roleSQL(ctx, fixture, "GRANT CONNECT ON DATABASE "+quoteIdentifier(fixture.Database)+" TO "+quoteIdentifier(name))
		roleSQL(ctx, fixture, "CREATE TABLE public.role_owned(value int); INSERT INTO public.role_owned VALUES(42)")
		roleSQL(ctx, fixture, "ALTER TABLE public.role_owned OWNER TO "+quoteIdentifier(name))
		connected, err := fixture.Server.Open(fixture.Database, name, "owner-password")
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { Expect(connected.Close()).To(Succeed()) })
		Expect(connected.PingContext(ctx)).To(Succeed())
		session := roleSession(ctx, fixture, false)
		Expect(session.Drop(ctx, name)).NotTo(Succeed())
		Expect(roleExists(ctx, fixture, name)).To(BeTrue())
		var value int
		Expect(fixture.Admin.QueryRowContext(ctx, "SELECT value FROM public.role_owned").Scan(&value)).To(Succeed())
		Expect(value).To(Equal(42))
		Expect(connected.PingContext(ctx)).To(Succeed())
		roleSQL(ctx, fixture, "DROP TABLE public.role_owned")
		roleSQL(ctx, fixture, "REVOKE CONNECT ON DATABASE "+quoteIdentifier(fixture.Database)+" FROM "+quoteIdentifier(name))
		Expect(session.Drop(ctx, name)).To(Succeed())
		Expect(roleExists(ctx, fixture, name)).To(BeFalse())
		Expect(session.Drop(ctx, name)).To(Succeed())
	}, NodeTimeout(25*time.Second))
})
