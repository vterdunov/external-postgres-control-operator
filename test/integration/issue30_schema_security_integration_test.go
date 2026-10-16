//go:build integration

package integration

import (
	"context"
	"errors"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	epco "github.com/vterdunov/external-postgres-control-operator/api/v1alpha1"
	pg "github.com/vterdunov/external-postgres-control-operator/internal/postgres"
)

type issue30ServiceBinding struct {
	Grantor             string
	Admin, Inherit, Set bool
}

func issue30ServiceBindings(ctx context.Context, f *databaseSQLFixture, owner string) []issue30ServiceBinding {
	rows, err := f.Admin.QueryContext(ctx, `SELECT pg_get_userbyid(grantor),admin_option,inherit_option,set_option
		FROM pg_auth_members WHERE roleid=$1::regrole AND member=$2::regrole ORDER BY grantor`, owner, f.Role)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	defer func() { ExpectWithOffset(1, rows.Close()).To(Succeed()) }()
	var result []issue30ServiceBinding
	for rows.Next() {
		var binding issue30ServiceBinding
		ExpectWithOffset(1, rows.Scan(&binding.Grantor, &binding.Admin, &binding.Inherit, &binding.Set)).To(Succeed())
		result = append(result, binding)
	}
	ExpectWithOffset(1, rows.Err()).NotTo(HaveOccurred())
	return result
}

var _ = Describe("Issue30 schema service capability boundaries", func() {
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
			Skip("these boundaries concern PG16 per-membership INHERIT and SET options")
		}
		roleSQL(ctx, f.Fixture, "ALTER ROLE "+quoteIdentifier(f.Role)+" CREATEDB CREATEROLE NOINHERIT")
		roleSQL(ctx, f.Fixture, "ALTER ROLE "+quoteIdentifier(f.Role)+" SET createrole_self_grant=''")
	}, NodeTimeout(30*time.Second))

	It("refuses transfer when the actual database owner has explicitly lost CREATE "+
		"without inheriting unrelated roles", func(ctx SpecContext) {
		oldOwner, newOwner := issue30ManagedOwner(ctx, f), issue30ManagedOwner(ctx, f)
		unrelated := issue30ManagedOwner(ctx, f)
		session := issue30SchemaSession(ctx, f, map[string]bool{oldOwner: true, newOwner: true, unrelated: true})
		name := f.name()
		desired := databaseDesired(name, oldOwner)
		desired.Spec.Schemas = []epco.SchemaSpec{schemaSpec("transfer_schema", oldOwner, epco.EnsurePresent)}
		databaseApply(ctx, session, desired)
		target := f.target(name)
		_, err := target.ExecContext(ctx, "CREATE SCHEMA unchanged_schema AUTHORIZATION "+quoteIdentifier(unrelated))
		Expect(err).NotTo(HaveOccurred())
		desired.Spec.Schemas = append(desired.Spec.Schemas, schemaSpec("unchanged_schema", unrelated, epco.EnsurePresent))
		var originalOID int64
		Expect(target.QueryRowContext(ctx, "SELECT oid FROM pg_namespace WHERE nspname='transfer_schema'").
			Scan(&originalOID)).To(Succeed())
		roleSQL(ctx, f.Fixture, "ALTER DATABASE "+quoteIdentifier(name)+" OWNER TO "+quoteIdentifier(newOwner))
		roleSQL(ctx, f.Fixture, "REVOKE CREATE ON DATABASE "+quoteIdentifier(name)+" FROM "+
			quoteIdentifier(newOwner)+", "+quoteIdentifier(f.Role)+", PUBLIC")
		desired.Spec.Owner = epco.SQLName(newOwner)
		databaseApply(ctx, session, desired)
		for _, role := range []string{oldOwner, newOwner, f.Role} {
			var create bool
			Expect(target.QueryRowContext(ctx, "SELECT has_database_privilege($1,current_database(),'CREATE')", role).
				Scan(&create)).To(Succeed())
			Expect(create).To(BeFalse(), "fixture must deny actual CREATE despite database ownership")
		}

		desired.Spec.Schemas[0].Owner = epco.SQLName(newOwner)
		_, err = session.Apply(ctx, desired)
		Expect(err).To(HaveOccurred())
		failure, ok := errors.AsType[*pg.Failure](err)
		Expect(ok).To(BeTrue())
		Expect(failure.Reason).To(Equal(pg.InsufficientPrivileges))
		var actualOID int64
		var owner string
		Expect(target.QueryRowContext(ctx, "SELECT oid,pg_get_userbyid(nspowner) FROM pg_namespace "+
			"WHERE nspname='transfer_schema'").Scan(&actualOID, &owner)).To(Succeed())
		Expect(actualOID).To(Equal(originalOID))
		Expect(owner).To(Equal(oldOwner))
		for _, role := range []string{newOwner, unrelated} {
			var inherited bool
			Expect(target.QueryRowContext(ctx, "SELECT pg_has_role($1,$2,'USAGE')", f.Role, role).Scan(&inherited)).To(Succeed())
			Expect(inherited).To(BeFalse(), "only concrete roles supplying missing authority may acquire INHERIT")
		}
	}, NodeTimeout(30*time.Second))

	It("preserves acquired INHERIT and exact independent options during later ordinary SET repair", func(ctx SpecContext) {
		oldOwner, newOwner := issue30ManagedOwner(ctx, f), issue30ManagedOwner(ctx, f)
		independent := f.owner(ctx, false)
		session := issue30SchemaSession(ctx, f, map[string]bool{oldOwner: true, newOwner: true})
		name := f.name()
		desired := databaseDesired(name, oldOwner)
		desired.Spec.Schemas = []epco.SchemaSpec{schemaSpec("transfer_schema", oldOwner, epco.EnsurePresent)}
		databaseApply(ctx, session, desired)
		// Distinct grantors retain their own options. Only the selected service
		// row needs an INHERIT change during the concrete schema transition.
		roleSQL(ctx, f.Fixture, "GRANT "+quoteIdentifier(oldOwner)+" TO "+quoteIdentifier(independent)+
			" WITH ADMIN TRUE, INHERIT FALSE, SET FALSE")
		roleSQL(ctx, f.Fixture, "GRANT "+quoteIdentifier(oldOwner)+" TO "+quoteIdentifier(f.Role)+
			" WITH ADMIN FALSE, INHERIT FALSE, SET FALSE GRANTED BY "+quoteIdentifier(independent))
		expected := issue30ServiceBindings(ctx, f, oldOwner)
		for i := range expected {
			if expected[i].Grantor == f.Role {
				expected[i].Inherit = true
			}
		}
		roleSQL(ctx, f.Fixture, "ALTER DATABASE "+quoteIdentifier(name)+" OWNER TO "+quoteIdentifier(newOwner))
		roleSQL(ctx, f.Fixture, "GRANT CREATE ON DATABASE "+quoteIdentifier(name)+" TO "+quoteIdentifier(f.Role))
		desired.Spec.Owner = epco.SQLName(newOwner)
		databaseApply(ctx, session, desired)
		desired.Spec.Schemas[0].Owner = epco.SQLName(newOwner)
		databaseApply(ctx, session, desired)
		Expect(issue30ServiceBindings(ctx, f, oldOwner)).To(Equal(expected))

		// External drift removes only SET, leaving the retained INHERIT bit.
		// Creating another ordinary managed database needs SET on its owner and
		// therefore exercises the existing SET-only capability repair path.
		roleSQL(ctx, f.Fixture, "GRANT "+quoteIdentifier(oldOwner)+" TO "+quoteIdentifier(f.Role)+
			" WITH SET FALSE GRANTED BY "+quoteIdentifier(f.Role))
		var canSet, inherited bool
		Expect(f.Admin.QueryRowContext(ctx, "SELECT pg_has_role($1,$2,'SET'),pg_has_role($1,$2,'USAGE')", f.Role, oldOwner).
			Scan(&canSet, &inherited)).To(Succeed())
		Expect(canSet).To(BeFalse())
		Expect(inherited).To(BeTrue())
		repairedDatabase := f.name()
		databaseApply(ctx, session, databaseDesired(repairedDatabase, oldOwner))
		Expect(issue30ServiceBindings(ctx, f, oldOwner)).To(Equal(expected),
			"SET repair must preserve ADMIN, acquired INHERIT and every independent grantor row")
		var actualOwner string
		Expect(f.Admin.QueryRowContext(ctx, "SELECT pg_get_userbyid(datdba) FROM pg_database WHERE datname=$1",
			repairedDatabase).
			Scan(&actualOwner)).To(Succeed())
		Expect(actualOwner).To(Equal(oldOwner))
	}, NodeTimeout(30*time.Second))
})
