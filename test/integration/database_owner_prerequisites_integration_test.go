//go:build integration

package integration

import (
	"context"
	"errors"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	epco "github.com/vterdunov/external-postgres-control-operator/api/v1alpha1"
	pgdatabase "github.com/vterdunov/external-postgres-control-operator/internal/postgres"
)

const databaseOwnerSetPrerequisite = "The administrative connection requires SET ROLE authority " +
	"for the requested database owner"

func ownerCapabilities(ctx context.Context, fixture *databaseSQLFixture, owner string) (bool, bool) {
	capability := databaseMemberCapability
	if fixture.version >= 160000 {
		capability = databaseSetCapability
	}
	var set, usage bool
	ExpectWithOffset(1, fixture.Admin.QueryRowContext(ctx,
		"SELECT pg_has_role($1,$2,$3),pg_has_role($1,$2,'USAGE')", fixture.Role, owner, capability).
		Scan(&set, &usage)).To(Succeed())
	return set, usage
}

func ownerRoleAttributes(ctx context.Context, fixture *databaseSQLFixture, owner string) {
	var superuser, createdb, createrole, login bool
	ExpectWithOffset(1, fixture.Admin.QueryRowContext(ctx,
		"SELECT rolsuper,rolcreatedb,rolcreaterole,rolcanlogin FROM pg_roles WHERE rolname=$1", owner).
		Scan(&superuser, &createdb, &createrole, &login)).To(Succeed())
	ExpectWithOffset(1, []bool{superuser, createdb, createrole, login}).To(Equal([]bool{false, false, false, false}))
	ExpectWithOffset(1, fixture.Admin.QueryRowContext(ctx,
		"SELECT rolsuper,rolcreatedb,rolcreaterole FROM pg_roles WHERE rolname=$1", fixture.Role).
		Scan(&superuser, &createdb, &createrole)).To(Succeed())
	ExpectWithOffset(1, []bool{superuser, createdb, createrole}).To(Equal([]bool{false, true, true}))
}

func requireOwnerSetPrerequisite(err error) {
	failure, ok := errors.AsType[*pgdatabase.Failure](err)
	ExpectWithOffset(1, ok).To(BeTrue())
	ExpectWithOffset(1, failure.Reason).To(Equal(pgdatabase.InsufficientPrivileges))
	ExpectWithOffset(1, failure.Message).To(Equal(databaseOwnerSetPrerequisite))
}

var _ = Describe("Database owner SET ROLE prerequisites", func() {
	var fixture *databaseSQLFixture
	BeforeEach(func(ctx SpecContext) {
		base, err := postgres.NewFixture(ctx)
		Expect(err).NotTo(HaveOccurred())
		fixture = &databaseSQLFixture{Fixture: base}
		DeferCleanup(func() {
			cleanup, cancel := context.WithTimeout(context.Background(), 25*time.Second)
			defer cancel()
			Expect(fixture.cleanup(cleanup)).To(Succeed())
		})
		Expect(fixture.Admin.QueryRowContext(ctx, "SHOW server_version_num").Scan(&fixture.version)).To(Succeed())
		roleSQL(ctx, base, "ALTER ROLE "+quoteIdentifier(fixture.Role)+" CREATEDB CREATEROLE NOINHERIT")
	}, NodeTimeout(30*time.Second))

	It("creates a database owned by the supplied administrative role", func(ctx SpecContext) {
		name := fixture.name()
		session := fixture.session(ctx, true, 5*time.Second)
		databaseApply(ctx, session, databaseDesired(name, fixture.Role))
		state, err := session.Inspect(ctx, name)
		Expect(err).NotTo(HaveOccurred())
		Expect(state.Exists).To(BeTrue())
		Expect(state.Owner).To(Equal(fixture.Role))
	}, NodeTimeout(30*time.Second))

	It("keeps a missing SQL owner pending without creating the database", func(ctx SpecContext) {
		name := fixture.name()
		owner, err := randomName("missing_owner_")
		Expect(err).NotTo(HaveOccurred())
		session := fixture.session(ctx, true, 5*time.Second)
		_, err = session.Apply(ctx, databaseDesired(name, owner))
		Expect(failureReason(err)).To(Equal(pgdatabase.DependenciesNotReady))
		Expect(fixture.exists(ctx, name)).To(BeFalse())
		Expect(roleExists(ctx, fixture.Fixture, owner)).To(BeFalse())
	}, NodeTimeout(30*time.Second))

	It("reports missing existing-owner SET authority and recovers only after explicit bootstrap", func(ctx SpecContext) {
		name := fixture.name()
		owner, err := randomName("ready_owner_")
		Expect(err).NotTo(HaveOccurred())
		fixture.owners = append(fixture.owners, owner)
		if fixture.version >= 160000 {
			roleSQL(ctx, fixture.Fixture,
				"ALTER ROLE "+quoteIdentifier(fixture.Role)+" SET createrole_self_grant='set'")
		}
		roles := roleSession(ctx, fixture.Fixture, true)
		desiredRole := pgdatabase.RoleDesired{Name: owner, Spec: epco.DatabaseRoleSpec{}}
		_, err = roles.Apply(ctx, desiredRole)
		Expect(err).NotTo(HaveOccurred())
		// Model the reported existing-role state after an external grant removal.
		// Native CREATE ROLE with self_grant=set is not its original cause.
		if fixture.version >= 160000 {
			_, err = fixture.Limited.ExecContext(ctx, "REVOKE "+quoteIdentifier(owner)+" FROM "+
				quoteIdentifier(fixture.Role)+" GRANTED BY "+quoteIdentifier(fixture.Role))
			Expect(err).NotTo(HaveOccurred())
		}
		roleSQL(ctx, fixture.Fixture, "REVOKE "+quoteIdentifier(owner)+" FROM "+quoteIdentifier(fixture.Role))
		_, err = roles.Apply(ctx, desiredRole)
		Expect(err).NotTo(HaveOccurred(), "existing role management does not retrofit self_grant")
		set, usage := ownerCapabilities(ctx, fixture, owner)
		Expect(set || usage).To(BeFalse())
		Expect(roleMemberships(ctx, fixture.Fixture, fixture.Role)).NotTo(ContainElement(owner))
		ownerRoleAttributes(ctx, fixture, owner)
		session := fixture.session(ctx, true, 5*time.Second)
		_, err = session.Apply(ctx, databaseDesired(name, owner))
		Expect(fixture.exists(ctx, name)).To(BeFalse())
		set, usage = ownerCapabilities(ctx, fixture, owner)
		Expect(set || usage).To(BeFalse(), "database reconciliation must not elevate administrative membership")
		Expect(roleMemberships(ctx, fixture.Fixture, fixture.Role)).NotTo(ContainElement(owner))
		ownerRoleAttributes(ctx, fixture, owner)
		requireOwnerSetPrerequisite(err)
		fixture.grantOwner(ctx, owner, true)
		set, usage = ownerCapabilities(ctx, fixture, owner)
		Expect(set).To(BeTrue())
		Expect(usage).To(BeFalse())
		databaseApply(ctx, session, databaseDesired(name, owner))
		databaseApply(ctx, session, databaseDesired(name, owner))
		state, err := session.Inspect(ctx, name)
		Expect(err).NotTo(HaveOccurred())
		Expect(state.Owner).To(Equal(owner))
		ownerRoleAttributes(ctx, fixture, owner)
	}, NodeTimeout(30*time.Second))

	It("rejects PG16 inherited-only owner authority without changing membership options", func(ctx SpecContext) {
		if fixture.version < 160000 {
			Skip("independent INHERIT and SET membership options require PostgreSQL 16")
		}
		name, owner := fixture.name(), fixture.owner(ctx, false)
		roleSQL(ctx, fixture.Fixture, "ALTER ROLE "+quoteIdentifier(fixture.Role)+" INHERIT")
		roleSQL(ctx, fixture.Fixture, "GRANT "+quoteIdentifier(owner)+" TO "+quoteIdentifier(fixture.Role)+
			" WITH ADMIN FALSE, INHERIT TRUE, SET FALSE")
		set, usage := ownerCapabilities(ctx, fixture, owner)
		Expect(set).To(BeFalse())
		Expect(usage).To(BeTrue())
		session := fixture.session(ctx, true, 5*time.Second)
		_, err := session.Apply(ctx, databaseDesired(name, owner))
		Expect(fixture.exists(ctx, name)).To(BeFalse())
		set, usage = ownerCapabilities(ctx, fixture, owner)
		Expect(set).To(BeFalse())
		Expect(usage).To(BeTrue())
		var admin, inherit, canSet bool
		Expect(fixture.Admin.QueryRowContext(ctx, `SELECT admin_option,inherit_option,set_option
			FROM pg_auth_members WHERE roleid=(SELECT oid FROM pg_roles WHERE rolname=$1)
			AND member=(SELECT oid FROM pg_roles WHERE rolname=$2)`, owner, fixture.Role).
			Scan(&admin, &inherit, &canSet)).To(Succeed())
		Expect([]bool{admin, inherit, canSet}).To(Equal([]bool{false, true, false}))
		ownerRoleAttributes(ctx, fixture, owner)
		requireOwnerSetPrerequisite(err)
		fixture.grantOwner(ctx, owner, true)
		set, usage = ownerCapabilities(ctx, fixture, owner)
		Expect(set).To(BeTrue())
		Expect(usage).To(BeFalse())
		databaseApply(ctx, session, databaseDesired(name, owner))
		ownerRoleAttributes(ctx, fixture, owner)
	}, NodeTimeout(30*time.Second))
})
