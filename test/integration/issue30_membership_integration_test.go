//go:build integration

package integration

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	epco "github.com/vterdunov/external-postgres-control-operator/api/v1alpha1"
	pg "github.com/vterdunov/external-postgres-control-operator/internal/postgres"
)

const issue30StatementTimeout = "statement_timeout"

var _ = Describe("Issue30 membership authority", func() {
	var f *Fixture
	BeforeEach(func(ctx SpecContext) {
		var err error
		f, err = postgres.NewFixture(ctx)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			cleanup, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			Expect(f.Close(cleanup)).To(Succeed())
		})
		var version int
		Expect(f.Admin.QueryRowContext(ctx, "SHOW server_version_num").Scan(&version)).To(Succeed())
		if version < 160000 {
			Skip("separate membership grantors and options require PostgreSQL 16")
		}
		roleSQL(ctx, f, "ALTER ROLE "+quoteIdentifier(f.Role)+" CREATEROLE NOINHERIT")
	}, NodeTimeout(25*time.Second))

	DescribeTable("uses existing legal parent authority without a direct administrator ADMIN row",
		func(ctx SpecContext, inherited bool, policy epco.MembershipPolicy) {
			parent, grantor, child := roleName(f), roleName(f), roleName(f)
			roleSQL(ctx, f, "CREATE ROLE "+quoteIdentifier(parent)+" NOLOGIN")
			roleSQL(ctx, f, "CREATE ROLE "+quoteIdentifier(grantor)+" NOLOGIN")
			roleSQL(ctx, f, "GRANT "+quoteIdentifier(parent)+" TO "+quoteIdentifier(grantor)+
				" WITH ADMIN TRUE, INHERIT FALSE, SET TRUE")
			options := " WITH INHERIT FALSE, SET TRUE"
			if inherited {
				options = " WITH INHERIT TRUE, SET FALSE"
			}
			roleSQL(ctx, f, "GRANT "+quoteIdentifier(grantor)+" TO "+quoteIdentifier(f.Role)+options)
			var directAdmin bool
			Expect(f.Admin.QueryRowContext(ctx, `SELECT EXISTS (
				SELECT 1 FROM pg_auth_members WHERE roleid=$1::regrole AND member=$2::regrole AND admin_option)`,
				parent, f.Role).Scan(&directAdmin)).To(Succeed())
			Expect(directAdmin).To(BeFalse())

			session := roleSession(ctx, f, true)
			password := "issue30-isolated-login-password"
			desired := pg.RoleDesired{Name: child, Password: &password, Spec: epco.DatabaseRoleSpec{
				Login: true, Inherit: new(false), InRoles: []epco.SQLName{epco.SQLName(parent)},
				MembershipPolicy: policy, Parameters: map[string]string{issue30StatementTimeout: "17s"},
			}}
			result, err := session.Apply(ctx, desired)
			Expect(err).NotTo(HaveOccurred(), "reachable parent ADMIN must be usable by the backend")
			Expect(result).NotTo(BeNil())
			Expect(result.Memberships).To(ContainElement(epco.RoleMembershipBinding{
				Role: parent, Member: child, Grantor: grantor,
			}))
			roleSQL(ctx, f, "GRANT CONNECT ON DATABASE "+quoteIdentifier(f.Database)+" TO "+quoteIdentifier(child))
			Expect(roleLogin(ctx, f, child, password)).To(BeEmpty())
			loginConfig := productionConfig(f, roleTLSMode, false).Connection.Copy()
			loginConfig.User, loginConfig.Password = child, password
			delete(loginConfig.RuntimeParams, issue30StatementTimeout)
			login, err := pgx.ConnectConfig(ctx, loginConfig)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func(ctx SpecContext) { Expect(login.Close(ctx)).To(Succeed()) })
			var timeout string
			var identity string
			Expect(login.QueryRow(ctx, "SELECT current_user").Scan(&identity)).To(Succeed())
			Expect(identity).To(Equal(child))
			Expect(login.QueryRow(ctx, "SHOW statement_timeout").Scan(&timeout)).To(Succeed())
			Expect(timeout).To(Equal("17s"), "membership execution must restore the operator before settings")

			// A restart must preserve the actual grantor when removing managed membership.
			Expect(session.Close(ctx)).To(Succeed())
			restarted := roleSession(ctx, f, true)
			desired.Password = nil
			desired.Spec.InRoles = nil
			desired.TrackedMemberships = result.Memberships
			_, err = restarted.Apply(ctx, desired)
			Expect(err).NotTo(HaveOccurred())
			Expect(roleMemberships(ctx, f, child)).NotTo(ContainElement(parent))
			var admin, inherit, set bool
			Expect(f.Admin.QueryRowContext(ctx, `SELECT admin_option,inherit_option,set_option
				FROM pg_auth_members WHERE roleid=$1::regrole AND member=$2::regrole`,
				parent, grantor).Scan(&admin, &inherit, &set)).To(Succeed())
			Expect([]bool{admin, inherit, set}).To(Equal([]bool{true, false, true}),
				"using an existing grantor must not alter its own parent membership")
		},
		Entry("inherited administrative grantor, managed", true, epco.MembershipManaged),
		Entry("SET-reachable administrative grantor, managed", false, epco.MembershipManaged),
		Entry("inherited administrative grantor, authoritative", true, epco.MembershipAuthoritative),
		Entry("SET-reachable administrative grantor, authoritative", false, epco.MembershipAuthoritative),
	)

	It("preserves exact distinct grantors across restart, omission and "+
		"administrative credential rotation", func(ctx SpecContext) {
		parents := []string{roleName(f), roleName(f)}
		grantors := []string{roleName(f), roleName(f)}
		child, rotated := roleName(f), roleName(f)
		for i, parent := range parents {
			roleSQL(ctx, f, "CREATE ROLE "+quoteIdentifier(parent)+
				" NOLOGIN; CREATE ROLE "+quoteIdentifier(grantors[i])+" NOLOGIN")
			roleSQL(ctx, f, "GRANT "+quoteIdentifier(parent)+" TO "+quoteIdentifier(grantors[i])+
				" WITH ADMIN TRUE, INHERIT FALSE, SET TRUE")
			roleSQL(ctx, f, "GRANT "+quoteIdentifier(grantors[i])+" TO "+quoteIdentifier(f.Role)+" WITH INHERIT FALSE, SET TRUE")
		}
		session := roleSession(ctx, f, true)
		desired := pg.RoleDesired{Name: child, Spec: epco.DatabaseRoleSpec{
			Login: true, MembershipPolicy: epco.MembershipManaged,
			InRoles: []epco.SQLName{epco.SQLName(parents[0]), epco.SQLName(parents[1])}}}
		pending := make(map[epco.RoleMembershipBinding]bool)
		rejected := 0
		desired.CheckpointMembership = func(_ context.Context, binding epco.RoleMembershipBinding, keep bool) error {
			Expect(binding.Member).To(Equal(child))
			if keep {
				pending[binding] = true
			} else {
				Expect(pending[binding]).To(BeTrue(), "only a previously checkpointed candidate can be retired")
				delete(pending, binding)
				rejected++
			}
			return nil
		}
		result, err := session.Apply(ctx, desired)
		Expect(err).NotTo(HaveOccurred())
		for i, parent := range parents {
			binding := epco.RoleMembershipBinding{Role: parent, Member: child, Grantor: grantors[i]}
			Expect(result.Memberships).To(ContainElement(binding))
			Expect(pending[binding]).To(BeTrue(), "the committed binding must retain its exact durable intent")
		}
		Expect(pending).To(HaveLen(2), "denied candidate intent must not become cleanup ownership")
		Expect(rejected).To(BeNumerically(">", 0), "the direct current-login candidate is genuinely denied in this fixture")
		// Add an independent row for the same role/member, with deliberately
		// different options. Only the two checkpointed grantors belong to EPCO.
		roleSQL(ctx, f, "GRANT "+quoteIdentifier(parents[0])+" TO "+quoteIdentifier(child)+
			" WITH ADMIN FALSE, INHERIT FALSE, SET FALSE GRANTED BY postgres")
		roleSQL(ctx, f, "CREATE ROLE "+quoteIdentifier(rotated)+
			" LOGIN CREATEROLE NOINHERIT PASSWORD "+quoteLiteral(f.Password))
		roleSQL(ctx, f, "GRANT CONNECT ON DATABASE "+quoteIdentifier(f.Database)+" TO "+quoteIdentifier(rotated))
		roleSQL(ctx, f, "GRANT "+quoteIdentifier(child)+" TO "+quoteIdentifier(rotated)+
			" WITH ADMIN TRUE, INHERIT FALSE, SET FALSE")
		for _, grantor := range grantors {
			roleSQL(ctx, f, "GRANT "+quoteIdentifier(grantor)+" TO "+quoteIdentifier(rotated)+" WITH INHERIT FALSE, SET TRUE")
		}
		config := productionConfig(f, roleTLSMode, false)
		config.Connection.User = rotated
		restarted, err := pg.NewRoleFactory().Open(ctx, config)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func(ctx SpecContext) { Expect(restarted.Close(ctx)).To(Succeed()) })
		desired.Spec.InRoles = nil
		desired.CheckpointMembership = nil
		desired.TrackedMemberships = result.Memberships
		_, err = restarted.Apply(ctx, desired)
		Expect(err).NotTo(HaveOccurred())
		var rows int
		var grantor string
		var admin, inherit, set bool
		Expect(f.Admin.QueryRowContext(ctx, "SELECT count(*) FROM pg_auth_members WHERE member=$1::regrole "+
			"AND roleid IN ($2::regrole,$3::regrole)", child, parents[0], parents[1]).Scan(&rows)).To(Succeed())
		Expect(rows).To(Equal(1), "only the independently granted row survives omission")
		Expect(f.Admin.QueryRowContext(ctx, `SELECT pg_get_userbyid(grantor),admin_option,inherit_option,set_option
			FROM pg_auth_members WHERE member=$1::regrole AND roleid=$2::regrole`, child, parents[0]).
			Scan(&grantor, &admin, &inherit, &set)).To(Succeed())
		Expect(grantor).To(Equal("postgres"))
		Expect([]bool{admin, inherit, set}).To(Equal([]bool{false, false, false}))
	}, NodeTimeout(30*time.Second))

	DescribeTable("preserves an independent satisfied membership without parent mutation authority",
		func(ctx SpecContext, policy epco.MembershipPolicy) {
			parent, child := roleName(f), roleName(f)
			roleSQL(ctx, f, "CREATE ROLE "+quoteIdentifier(parent)+" NOLOGIN")
			session := roleSession(ctx, f, true)
			desired := pg.RoleDesired{Name: child, Spec: epco.DatabaseRoleSpec{Login: true, MembershipPolicy: policy}}
			_, err := session.Apply(ctx, desired)
			Expect(err).NotTo(HaveOccurred())
			roleSQL(ctx, f, "GRANT "+quoteIdentifier(parent)+" TO "+quoteIdentifier(child)+
				" WITH ADMIN FALSE, INHERIT FALSE, SET FALSE")
			desired.Spec.InRoles = []epco.SQLName{epco.SQLName(parent)}
			result, err := session.Apply(ctx, desired)
			Expect(err).NotTo(HaveOccurred(), "no mutation is necessary for a satisfied names-only declaration")
			Expect(slices.ContainsFunc(result.Memberships, func(binding epco.RoleMembershipBinding) bool {
				return binding.Role == parent && binding.Member == child
			})).To(BeFalse(), "the independent binding must not become managed cleanup responsibility")
			var admin, inherit, set bool
			Expect(f.Admin.QueryRowContext(ctx, `SELECT admin_option,inherit_option,set_option
				FROM pg_auth_members WHERE roleid=$1::regrole AND member=$2::regrole`, parent, child).
				Scan(&admin, &inherit, &set)).To(Succeed())
			Expect([]bool{admin, inherit, set}).To(Equal([]bool{false, false, false}))
		},
		Entry("managed policy", epco.MembershipManaged),
		Entry("authoritative policy", epco.MembershipAuthoritative),
	)

	It("rolls back a fresh login and settings when no legal parent grantor exists", func(ctx SpecContext) {
		parent, child := roleName(f), roleName(f)
		roleSQL(ctx, f, "CREATE ROLE "+quoteIdentifier(parent)+" NOLOGIN")
		_, directErr := f.Limited.ExecContext(ctx, "GRANT "+quoteIdentifier(parent)+" TO "+
			quoteIdentifier(f.Role)+" GRANTED BY "+quoteIdentifier(f.Role))
		Expect(SQLState(directErr)).To(Equal("42501"), "prove actual SQL denial rather than a transport failure")
		session := roleSession(ctx, f, true)
		_, err := session.Apply(ctx, pg.RoleDesired{Name: child, Password: new("issue30-denied-password"),
			Spec: epco.DatabaseRoleSpec{Login: true, InRoles: []epco.SQLName{epco.SQLName(parent)},
				MembershipPolicy: epco.MembershipManaged, Parameters: map[string]string{issue30StatementTimeout: "17s"}}})
		failure, ok := errors.AsType[*pg.Failure](err)
		Expect(ok).To(BeTrue())
		Expect(failure.Reason).To(Equal(pg.InsufficientPrivileges))
		Expect(roleExists(ctx, f, child)).To(BeFalse(), "membership refusal must roll back CREATE ROLE/password/settings")
	}, NodeTimeout(25*time.Second))
})
