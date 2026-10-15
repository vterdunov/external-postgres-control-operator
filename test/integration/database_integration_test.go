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
	pgdatabase "github.com/vterdunov/external-postgres-control-operator/internal/postgres"
)

const (
	databaseInvalidLocale     = "invalid"
	databaseUnavailableLocale = "unavailable"
	databaseBuiltinProvider   = "builtin"
	databaseTemplateZero      = "template0"
	databaseICUProvider       = "icu"
	databaseExplicitSchema    = "explicit"
	databaseOwnedSchema       = "owned"
	databaseMemberCapability  = "MEMBER"
	databaseSetCapability     = "SET"
)

// All helpers and cleanup belong to this suite. FORCE is used only by fixture
// disposal, never by production reconciliation or its behavioral assertions.
type databaseSQLFixture struct {
	*Fixture
	databases, owners []string
	version           int
}

func (f *databaseSQLFixture) cleanup(ctx context.Context) error {
	err := f.Server.withCleanupConnection(ctx, roleSQLAdmin, func(connection *sql.Conn) error {
		var errs []error
		for _, name := range f.databases {
			var isTemplate bool
			queryErr := connection.QueryRowContext(ctx,
				"SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname=$1 AND datistemplate)", name).Scan(&isTemplate)
			errs = append(errs, queryErr)
			if isTemplate {
				_, alterErr := connection.ExecContext(ctx, "ALTER DATABASE "+quoteIdentifier(name)+" IS_TEMPLATE FALSE")
				errs = append(errs, alterErr)
			}
			_, dropErr := connection.ExecContext(ctx, "DROP DATABASE IF EXISTS "+quoteIdentifier(name)+" WITH (FORCE)")
			errs = append(errs, dropErr)
		}
		for _, owner := range f.owners {
			_, dropErr := connection.ExecContext(ctx, "DROP ROLE IF EXISTS "+quoteIdentifier(owner))
			errs = append(errs, dropErr)
		}
		return errors.Join(errs...)
	})
	return errors.Join(err, f.Close(ctx))
}

func (f *databaseSQLFixture) name() string {
	name, err := randomName("managed_db_")
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	f.databases = append(f.databases, name)
	return name
}

func (f *databaseSQLFixture) owner(ctx context.Context, member bool) string {
	name, err := randomName("db_owner_")
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	f.owners = append(f.owners, name)
	roleSQL(ctx, f.Fixture, "CREATE ROLE "+quoteIdentifier(name)+" NOLOGIN NOCREATEDB NOCREATEROLE")
	if member {
		f.grantOwner(ctx, name, true)
	}
	return name
}

func (f *databaseSQLFixture) grantOwner(ctx context.Context, owner string, set bool) {
	query := "GRANT " + quoteIdentifier(owner) + " TO " + quoteIdentifier(f.Role)
	if f.version >= 160000 {
		query += " WITH INHERIT FALSE, SET "
		if set {
			query += "TRUE"
		} else {
			query += "FALSE"
		}
	}
	roleSQL(ctx, f.Fixture, query)
}

func (f *databaseSQLFixture) inheritOwner(ctx context.Context, owner string) {
	roleSQL(ctx, f.Fixture, "ALTER ROLE "+quoteIdentifier(f.Role)+" INHERIT")
	if f.version >= 160000 {
		roleSQL(ctx, f.Fixture, "GRANT "+quoteIdentifier(owner)+" TO "+quoteIdentifier(f.Role)+
			" WITH INHERIT TRUE, SET TRUE")
	}
}

func (f *databaseSQLFixture) session(ctx context.Context, restricted bool,
	timeout time.Duration) pgdatabase.DatabaseSession {
	config := productionConfig(f.Fixture, roleTLSMode, false)
	if !restricted {
		config.Connection.User, config.Connection.Password = roleSQLAdmin, f.Server.password
	}
	config.StatementTimeout = timeout
	session, err := pgdatabase.NewDatabaseFactory().Open(ctx, config, pgdatabase.DatabaseSessionOptions{})
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	DeferCleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		Expect(session.Close(cleanup)).To(Succeed())
	})
	return session
}

func (f *databaseSQLFixture) target(name string) *sql.DB {
	db, err := f.Server.Open(name, roleSQLAdmin, f.Server.password)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	DeferCleanup(func() { Expect(db.Close()).To(Succeed()) })
	return db
}

func (f *databaseSQLFixture) exists(ctx context.Context, name string) bool {
	exists, err := f.Server.DatabaseExists(ctx, name)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	return exists
}

func databaseDesired(name, owner string) pgdatabase.DatabaseDesired {
	return pgdatabase.DatabaseDesired{Name: name, Spec: epco.DatabaseSpec{Owner: epco.SQLName(owner)}}
}

func schemaSpec(name, owner string, ensure epco.EnsureOption) epco.SchemaSpec {
	return epco.SchemaSpec{Name: epco.SQLName(name), Ensure: ensure, Owner: epco.SQLName(owner)}
}

func databaseApply(ctx context.Context, session pgdatabase.DatabaseSession, desired pgdatabase.DatabaseDesired) {
	result, err := session.Apply(ctx, desired)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	ExpectWithOffset(1, result).NotTo(BeNil(), "success requires a confirmation marker")
}

func databaseTable(ctx context.Context, db *sql.DB) (int, string) {
	var value int
	var owner string
	ExpectWithOffset(1, db.QueryRowContext(ctx, `SELECT value, pg_get_userbyid(relowner)
		FROM public.preserved JOIN pg_class ON oid='public.preserved'::regclass`).Scan(&value, &owner)).To(Succeed())
	return value, owner
}

var _ = Describe("Database PostgreSQL reconciliation", func() {
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
		roleSQL(ctx, fixture.Fixture, "ALTER ROLE "+quoteIdentifier(fixture.Role)+" CREATEDB NOINHERIT")
	}, NodeTimeout(30*time.Second))

	It("creates outside a transaction under a restricted CREATEDB administrator for a NOLOGIN owner",
		func(ctx SpecContext) {
			name, owner := fixture.name(), fixture.owner(ctx, true)
			session := fixture.session(ctx, true, 5*time.Second)
			state, err := session.Inspect(ctx, name)
			Expect(err).NotTo(HaveOccurred())
			Expect(state.Exists).To(BeFalse())
			Expect(state.AdminUser).To(Equal(fixture.Role))
			Expect(state.AdminDatabase).To(Equal(fixture.Database))
			Expect(state.ServerVersion).To(Equal(fixture.version))
			Expect(state.MaxIdentifierLength).To(Equal(63))
			databaseApply(ctx, session, databaseDesired(name, owner))
			state, err = session.Inspect(ctx, name)
			Expect(err).NotTo(HaveOccurred())
			Expect(state.Exists).To(BeTrue())
			Expect(state.Owner).To(Equal(owner))
			Expect(state.AllowConnections).To(BeTrue())
			Expect(state.IsTemplate).To(BeFalse())
			Expect(state.ConnectionLimit).To(Equal(int64(-1)))
			Expect(state.Tablespace).To(Equal("pg_default"))
			var superuser, createdb, createrole, login bool
			Expect(fixture.Admin.QueryRowContext(ctx,
				"SELECT rolsuper,rolcreatedb,rolcreaterole FROM pg_roles WHERE rolname=$1",
				fixture.Role).Scan(&superuser, &createdb, &createrole)).To(Succeed())
			Expect(superuser).To(BeFalse())
			Expect(createdb).To(BeTrue())
			Expect(createrole).To(BeFalse())
			Expect(fixture.Admin.QueryRowContext(ctx,
				"SELECT rolcanlogin,rolcreatedb FROM pg_roles WHERE rolname=$1", owner).Scan(&login, &createdb)).To(Succeed())
			Expect(login).To(BeFalse())
			Expect(createdb).To(BeFalse())
			databaseApply(ctx, session, databaseDesired(name, owner))
		}, NodeTimeout(30*time.Second))

	It("requires CREATEDB and owner access without granting either implicitly", func(ctx SpecContext) {
		name, owner := fixture.name(), fixture.owner(ctx, false)
		session := fixture.session(ctx, true, 5*time.Second)
		_, err := session.Apply(ctx, databaseDesired(name, owner))
		Expect(failureReason(err)).To(Equal(pgdatabase.InsufficientPrivileges))
		Expect(fixture.exists(ctx, name)).To(BeFalse())
		Expect(roleMemberships(ctx, fixture.Fixture, fixture.Role)).NotTo(ContainElement(owner))
		fixture.grantOwner(ctx, owner, true)
		roleSQL(ctx, fixture.Fixture, "ALTER ROLE "+quoteIdentifier(fixture.Role)+" NOCREATEDB")
		_, err = session.Apply(ctx, databaseDesired(name, owner))
		Expect(failureReason(err)).To(Equal(pgdatabase.InsufficientPrivileges))
		Expect(fixture.exists(ctx, name)).To(BeFalse())
		var createdb bool
		Expect(fixture.Admin.QueryRowContext(ctx, "SELECT rolcreatedb FROM pg_roles WHERE rolname=$1",
			fixture.Role).Scan(&createdb)).To(Succeed())
		Expect(createdb).To(BeFalse())
	}, NodeTimeout(30*time.Second))

	It("distinguishes PG16 SET authority from membership without elevating existing memberships", func(ctx SpecContext) {
		if fixture.version < 160000 {
			return
		} // PG15 memberships always permit SET ROLE.
		name, owner := fixture.name(), fixture.owner(ctx, false)
		fixture.grantOwner(ctx, owner, false)
		session := fixture.session(ctx, true, 5*time.Second)
		_, err := session.Apply(ctx, databaseDesired(name, owner))
		Expect(failureReason(err)).To(Equal(pgdatabase.InsufficientPrivileges))
		Expect(fixture.exists(ctx, name)).To(BeFalse())
		var set bool
		Expect(fixture.Admin.QueryRowContext(ctx, `SELECT set_option FROM pg_auth_members
			WHERE roleid=(SELECT oid FROM pg_roles WHERE rolname=$1)
			AND member=(SELECT oid FROM pg_roles WHERE rolname=$2)`, owner, fixture.Role).Scan(&set)).To(Succeed())
		Expect(set).To(BeFalse())
		fixture.grantOwner(ctx, owner, true)
		databaseApply(ctx, session, databaseDesired(name, owner))
	}, NodeTimeout(30*time.Second))

	It("updates mutable properties and database owner while preserving data and existing table owners",
		func(ctx SpecContext) {
			name, owner := fixture.name(), fixture.owner(ctx, true)
			other := fixture.owner(ctx, true)
			// OWNER transfer needs the administrator's own CREATEDB together with
			// inherited current-owner authority; SET ROLE to a NOCREATEDB owner
			// cannot supply that combination.
			roleSQL(ctx, fixture.Fixture, "ALTER ROLE "+quoteIdentifier(fixture.Role)+" INHERIT")
			if fixture.version >= 160000 {
				roleSQL(ctx, fixture.Fixture,
					"GRANT "+quoteIdentifier(owner)+" TO "+quoteIdentifier(fixture.Role)+" WITH INHERIT TRUE, SET TRUE")
			}
			session := fixture.session(ctx, true, 5*time.Second)
			desired := databaseDesired(name, owner)
			databaseApply(ctx, session, desired)
			target := fixture.target(name)
			_, err := target.ExecContext(ctx,
				"CREATE TABLE public.preserved(value int); INSERT INTO public.preserved VALUES(42); "+
					"ALTER TABLE public.preserved OWNER TO "+quoteIdentifier(owner))
			Expect(err).NotTo(HaveOccurred())
			desired.Spec.Owner, desired.Spec.IsTemplate, desired.Spec.ConnectionLimit = epco.SQLName(other),
				new(true), new(int64(7))
			databaseApply(ctx, session, desired)
			state, err := session.Inspect(ctx, name)
			Expect(err).NotTo(HaveOccurred())
			Expect(state.Owner).To(Equal(other))
			Expect(state.IsTemplate).To(BeTrue())
			Expect(state.ConnectionLimit).To(Equal(int64(7)))
			value, tableOwner := databaseTable(ctx, target)
			Expect(value).To(Equal(42))
			Expect(tableOwner).To(Equal(owner))
			// Omitted mutable properties preserve the existing server values.
			databaseApply(ctx, session, databaseDesired(name, other))
			state, err = session.Inspect(ctx, name)
			Expect(err).NotTo(HaveOccurred())
			Expect(state.IsTemplate).To(BeTrue())
			Expect(state.ConnectionLimit).To(Equal(int64(7)))
			desired.Spec.IsTemplate, desired.Spec.ConnectionLimit, desired.Spec.AllowConnections = new(false),
				new(int64(-1)), new(false)
			databaseApply(ctx, session, desired)
			state, err = session.Inspect(ctx, name)
			Expect(err).NotTo(HaveOccurred())
			Expect(state.IsTemplate).To(BeFalse())
			Expect(state.AllowConnections).To(BeFalse())
			Expect(state.ConnectionLimit).To(Equal(int64(-1)))
			databaseApply(ctx, session, databaseDesired(name, other))
			state, err = session.Inspect(ctx, name)
			Expect(err).NotTo(HaveOccurred())
			Expect(state.AllowConnections).To(BeFalse())
		}, NodeTimeout(40*time.Second))

	It("confirms unchanged explicit properties after administrative mutation authority is removed", func(ctx SpecContext) {
		name, owner := fixture.name(), fixture.owner(ctx, true)
		session := fixture.session(ctx, true, 5*time.Second)
		databaseApply(ctx, session, databaseDesired(name, owner))
		desired := databaseDesired(name, owner)
		desired.Spec.IsTemplate = new(false)
		desired.Spec.AllowConnections = new(true)
		desired.Spec.ConnectionLimit = new(int64(7))
		desired.Spec.Tablespace = "pg_default"
		databaseApply(ctx, session, desired)
		roleSQL(ctx, fixture.Fixture, "REVOKE "+quoteIdentifier(owner)+" FROM "+quoteIdentifier(fixture.Role))
		roleSQL(ctx, fixture.Fixture, "ALTER ROLE "+quoteIdentifier(fixture.Role)+" NOCREATEDB")
		databaseApply(ctx, session, desired)
		state, err := session.Inspect(ctx, name)
		Expect(err).NotTo(HaveOccurred())
		Expect(state.Owner).To(Equal(owner))
		Expect(state.ConnectionLimit).To(Equal(int64(7)))
	}, NodeTimeout(30*time.Second))

	It("ignores every creation-only declaration on an existing database without recreating data", func(ctx SpecContext) {
		name, owner := fixture.name(), fixture.owner(ctx, true)
		session := fixture.session(ctx, true, 5*time.Second)
		databaseApply(ctx, session, databaseDesired(name, owner))
		target := fixture.target(name)
		_, err := target.ExecContext(ctx,
			"CREATE TABLE public.preserved(value int); INSERT INTO public.preserved VALUES(42); "+
				"ALTER TABLE public.preserved OWNER TO "+quoteIdentifier(owner))
		Expect(err).NotTo(HaveOccurred())
		var oid int64
		Expect(fixture.Admin.QueryRowContext(ctx, "SELECT oid FROM pg_database WHERE datname=$1",
			name).Scan(&oid)).To(Succeed())
		desired := databaseDesired(name, owner)
		desired.Spec.Template = "missing_template"
		desired.Spec.Encoding, desired.Spec.Locale, desired.Spec.LocaleCollate,
			desired.Spec.LocaleCType = databaseInvalidLocale, databaseUnavailableLocale, databaseUnavailableLocale,
			databaseUnavailableLocale
		desired.Spec.LocaleProvider = databaseBuiltinProvider
		desired.Spec.ICULocale, desired.Spec.ICURules = databaseInvalidLocale, databaseInvalidLocale
		desired.Spec.BuiltinLocale, desired.Spec.CollationVersion = databaseInvalidLocale, "arbitrary-version"
		databaseApply(ctx, session, desired)
		var currentOID int64
		Expect(fixture.Admin.QueryRowContext(ctx, "SELECT oid FROM pg_database WHERE datname=$1",
			name).Scan(&currentOID)).To(Succeed())
		Expect(currentOID).To(Equal(oid))
		value, tableOwner := databaseTable(ctx, target)
		Expect(value).To(Equal(42))
		Expect(tableOwner).To(Equal(owner))
	}, NodeTimeout(30*time.Second))

	DescribeTable("applies supported creation-only locale options with versioned catalog confirmation",
		func(ctx SpecContext, provider string, rules bool) {
			name, owner := fixture.name(), fixture.owner(ctx, true)
			desired := databaseDesired(name, owner)
			desired.Spec.Template, desired.Spec.Encoding, desired.Spec.LocaleProvider = databaseTemplateZero, "UTF8", provider
			desired.Spec.Locale, desired.Spec.LocaleCollate, desired.Spec.LocaleCType = "C", "C", "C"
			if provider == databaseICUProvider {
				desired.Spec.ICULocale = "en-US"
				desired.Spec.CollationVersion = "epco-test-version"
			}
			if rules {
				desired.Spec.ICURules = "&a < b"
			}
			if provider == databaseBuiltinProvider {
				desired.Spec.BuiltinLocale = "C.UTF-8"
			}
			session := fixture.session(ctx, true, 5*time.Second)
			_, err := session.Apply(ctx, desired)
			if (rules && fixture.version < 160000) || (provider == databaseBuiltinProvider && fixture.version < 170000) {
				Expect(failureReason(err)).To(Equal(pgdatabase.UnsupportedFeature))
				Expect(fixture.exists(ctx, name)).To(BeFalse())
				return
			}
			Expect(err).NotTo(HaveOccurred())
			var encoding, collate, ctype, actualProvider, version string
			Expect(fixture.Admin.QueryRowContext(ctx, `SELECT pg_encoding_to_char(encoding), datcollate, datctype,
			datlocprovider::text, COALESCE(datcollversion,'')
			FROM pg_database WHERE datname=$1`, name).
				Scan(&encoding, &collate, &ctype, &actualProvider, &version)).To(Succeed())
			Expect(encoding).To(Equal("UTF8"))
			Expect(collate).To(Equal("C"))
			Expect(ctype).To(Equal("C"))
			providers := map[string]string{"libc": "c", databaseICUProvider: "i", databaseBuiltinProvider: "b"}
			Expect(actualProvider).To(Equal(providers[provider]))
			if provider == databaseICUProvider {
				Expect(version).To(Equal("epco-test-version"))
			}
			if provider != "libc" {
				column := "daticulocale"
				if fixture.version >= 170000 {
					column = "datlocale"
				}
				var locale string
				Expect(fixture.Admin.QueryRowContext(ctx, "SELECT "+column+" FROM pg_database WHERE datname=$1",
					name).Scan(&locale)).To(Succeed())
				if provider == databaseICUProvider {
					Expect(locale).To(Equal("en-US"))
				} else {
					Expect(locale).To(Equal("C.UTF-8"))
				}
			}
			if rules {
				var actualRules string
				Expect(fixture.Admin.QueryRowContext(ctx, "SELECT daticurules FROM pg_database WHERE datname=$1",
					name).Scan(&actualRules)).To(Succeed())
				Expect(actualRules).To(Equal(desired.Spec.ICURules))
			}
		}, Entry("libc on PG15-18", "libc", false), Entry("ICU locale on PG15-18", databaseICUProvider, false),
		Entry("ICU rules on PG16+", databaseICUProvider, true),
		Entry("builtin locale on PG17+", databaseBuiltinProvider, false))

	It("copies a selected template and rejects unavailable locales before CREATE", func(ctx SpecContext) {
		owner, template, name := fixture.owner(ctx, true), fixture.name(), fixture.name()
		_, err := fixture.Server.Admin.ExecContext(ctx,
			"CREATE DATABASE "+quoteIdentifier(template)+" OWNER "+quoteIdentifier(owner))
		Expect(err).NotTo(HaveOccurred())
		target := fixture.target(template)
		_, err = target.ExecContext(ctx,
			"CREATE TABLE public.template_data(value int); INSERT INTO public.template_data VALUES(73)")
		Expect(err).NotTo(HaveOccurred())
		Expect(target.Close()).To(Succeed())
		_, err = fixture.Server.Admin.ExecContext(ctx, "ALTER DATABASE "+quoteIdentifier(template)+" IS_TEMPLATE true")
		Expect(err).NotTo(HaveOccurred())
		session := fixture.session(ctx, true, 5*time.Second)
		desired := databaseDesired(name, owner)
		desired.Spec.Template = epco.SQLName(template)
		databaseApply(ctx, session, desired)
		var value int
		Expect(fixture.target(name).QueryRowContext(ctx, "SELECT value FROM public.template_data").Scan(&value)).To(Succeed())
		Expect(value).To(Equal(73))
		invalid := databaseDesired(fixture.name(), owner)
		invalid.Spec.Template, invalid.Spec.Locale = databaseTemplateZero, "epco_unavailable_locale"
		_, err = session.Apply(ctx, invalid)
		Expect(failureReason(err)).To(Equal(pgdatabase.InvalidConfiguration))
		Expect(fixture.exists(ctx, invalid.Name)).To(BeFalse())
	}, NodeTimeout(40*time.Second))

	It("quotes Unicode SQL names and enforces the server byte limit without truncation", func(ctx SpecContext) {
		owner := fixture.owner(ctx, true)
		session := fixture.session(ctx, true, 5*time.Second)
		name := fixture.name() + "'\"ю"
		fixture.databases = append(fixture.databases, name)
		desired := databaseDesired(name, owner)
		desired.Spec.Schemas = []epco.SchemaSpec{schemaSpec("'\"схема", "", "")}
		databaseApply(ctx, session, desired)
		state, err := session.Inspect(ctx, name)
		Expect(err).NotTo(HaveOccurred())
		Expect(state.Name).To(Equal(name))
		Expect(state.Exists).To(BeTrue())
		schemas, err := session.InspectSchemas(ctx, name, desired.Spec.Schemas)
		Expect(err).NotTo(HaveOccurred())
		Expect(schemas).To(ContainElement(pgdatabase.SchemaState{Name: "'\"схема", Owner: owner, Exists: true}))
		for _, invalid := range []string{strings.Repeat("я", 32), "bad\x00name", ""} {
			_, err = session.Apply(ctx, databaseDesired(invalid, owner))
			Expect(failureReason(err)).To(Equal(pgdatabase.InvalidConfiguration))
		}
		Expect(fixture.exists(ctx, strings.Repeat("я", 31))).To(BeFalse())
		tooLongSchema := databaseDesired(name, owner)
		tooLongSchema.Spec.Schemas = []epco.SchemaSpec{schemaSpec(strings.Repeat("я", 32), "", "")}
		_, err = session.Apply(ctx, tooLongSchema)
		Expect(failureReason(err)).To(Equal(pgdatabase.InvalidConfiguration))
	}, NodeTimeout(30*time.Second))

	It("protects standard and current administrative databases while permitting read-only inspection",
		func(ctx SpecContext) {
			session := fixture.session(ctx, false, 5*time.Second)
			for _, name := range []string{"postgres", databaseTemplateZero, "template1", fixture.Database} {
				before, err := session.Inspect(ctx, name)
				Expect(err).NotTo(HaveOccurred())
				Expect(before.Exists).To(BeTrue())
				desired := databaseDesired(name, fixture.owner(ctx, true))
				desired.Spec.ConnectionLimit = new(int64(2))
				_, err = session.Apply(ctx, desired)
				Expect(err).To(HaveOccurred())
				Expect(session.Drop(ctx, name)).NotTo(Succeed())
				after, err := session.Inspect(ctx, name)
				Expect(err).NotTo(HaveOccurred())
				Expect(after).To(Equal(before))
			}
		}, NodeTimeout(30*time.Second))

	It("drops with actual ownership and preserves connected application sessions", func(ctx SpecContext) {
		name, owner := fixture.name(), fixture.owner(ctx, true)
		session := fixture.session(ctx, true, 5*time.Second)
		databaseApply(ctx, session, databaseDesired(name, owner))
		target := fixture.target(name)
		Expect(target.PingContext(ctx)).To(Succeed())
		Expect(failureReason(session.Drop(ctx, name))).To(Equal(pgdatabase.DeletionBlocked))
		Expect(fixture.exists(ctx, name)).To(BeTrue())
		Expect(target.PingContext(ctx)).To(Succeed())
		Expect(target.Close()).To(Succeed())
		Expect(session.Drop(ctx, name)).To(Succeed())
		Expect(fixture.exists(ctx, name)).To(BeFalse())
		Expect(session.Drop(ctx, name)).To(Succeed())
	}, NodeTimeout(30*time.Second))

	It("refuses to drop a database owned by an inaccessible role without granting ownership", func(ctx SpecContext) {
		name, owner := fixture.name(), fixture.owner(ctx, false)
		_, err := fixture.Server.Admin.ExecContext(ctx,
			"CREATE DATABASE "+quoteIdentifier(name)+" OWNER "+quoteIdentifier(owner))
		Expect(err).NotTo(HaveOccurred())
		session := fixture.session(ctx, true, 5*time.Second)
		Expect(failureReason(session.Drop(ctx, name))).To(Equal(pgdatabase.InsufficientPrivileges))
		Expect(fixture.exists(ctx, name)).To(BeTrue())
		Expect(roleMemberships(ctx, fixture.Fixture, fixture.Role)).NotTo(ContainElement(owner))
		fixture.grantOwner(ctx, owner, true)
		Expect(session.Drop(ctx, name)).To(Succeed())
	}, NodeTimeout(30*time.Second))

	It("fails closed after session close or context cancellation without creating a database", func(ctx SpecContext) {
		owner := fixture.owner(ctx, true)
		session := fixture.session(ctx, true, 5*time.Second)
		Expect(session.Close(ctx)).To(Succeed())
		_, err := session.Inspect(ctx, fixture.name())
		Expect(err).To(HaveOccurred())
		name := fixture.name()
		_, err = session.Apply(ctx, databaseDesired(name, owner))
		Expect(err).To(HaveOccurred())
		Expect(fixture.exists(ctx, name)).To(BeFalse())
		Expect(session.Drop(ctx, name)).NotTo(Succeed())
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		fresh := fixture.session(ctx, true, 5*time.Second)
		name = fixture.name()
		_, err = fresh.Apply(canceled, databaseDesired(name, owner))
		Expect(err).To(HaveOccurred())
		Expect(fixture.exists(ctx, name)).To(BeFalse())
	}, NodeTimeout(30*time.Second))

	It("uses ordinary tablespace capability checks and blocks a move while sessions remain connected",
		func(ctx SpecContext) {
			name, owner := fixture.name(), fixture.owner(ctx, true)
			space, err := randomName("db_space_")
			Expect(err).NotTo(HaveOccurred())
			location := "/tmp/" + space
			command := exec.CommandContext(ctx, "docker", "exec", fixture.Server.Container, "sh", "-ec",
				"mkdir "+location+"; chown postgres:postgres "+location)
			Expect(command.Run()).To(Succeed())
			_, err = fixture.Server.Admin.ExecContext(ctx,
				"CREATE TABLESPACE "+quoteIdentifier(space)+" LOCATION "+quoteLiteral(location))
			Expect(err).NotTo(HaveOccurred())
			// Registered before target cleanup, so databases are removed before tablespace disposal.
			fixture.databases = append(fixture.databases, name)
			DeferCleanup(func() {
				cleanup, cancel := context.WithTimeout(context.Background(), 25*time.Second)
				defer cancel()
				Expect(fixture.Server.withCleanupConnection(cleanup, roleSQLAdmin, func(connection *sql.Conn) error {
					_, databaseErr := connection.ExecContext(cleanup,
						"DROP DATABASE IF EXISTS "+quoteIdentifier(name)+" WITH (FORCE)")
					_, spaceErr := connection.ExecContext(cleanup, "DROP TABLESPACE "+quoteIdentifier(space))
					return errors.Join(databaseErr, spaceErr)
				})).To(Succeed())
			})
			session := fixture.session(ctx, true, 5*time.Second)
			desired := databaseDesired(name, owner)
			databaseApply(ctx, session, desired)
			desired.Spec.Tablespace = epco.SQLName(space)
			_, err = session.Apply(ctx, desired)
			Expect(failureReason(err)).To(Equal(pgdatabase.InsufficientPrivileges))
			_, err = fixture.Server.Admin.ExecContext(ctx,
				"GRANT CREATE ON TABLESPACE "+quoteIdentifier(space)+" TO "+
					quoteIdentifier(owner)+", "+quoteIdentifier(fixture.Role))
			Expect(err).NotTo(HaveOccurred())
			target := fixture.target(name)
			Expect(target.PingContext(ctx)).To(Succeed())
			_, err = session.Apply(ctx, desired)
			Expect(err).To(HaveOccurred())
			Expect(target.PingContext(ctx)).To(Succeed())
			state, err := session.Inspect(ctx, name)
			Expect(err).NotTo(HaveOccurred())
			Expect(state.Tablespace).To(Equal("pg_default"))
			Expect(target.Close()).To(Succeed())
			databaseApply(ctx, session, desired)
			state, err = session.Inspect(ctx, name)
			Expect(err).NotTo(HaveOccurred())
			Expect(state.Tablespace).To(Equal(space))
			databaseApply(ctx, session, databaseDesired(name, owner))
			state, err = session.Inspect(ctx, name)
			Expect(err).NotTo(HaveOccurred())
			Expect(state.Tablespace).To(Equal(space))
		}, NodeTimeout(40*time.Second))

	It("defaults new schema owners and preserves omitted existing owners", func(ctx SpecContext) {
		name, owner := fixture.name(), fixture.owner(ctx, true)
		other := fixture.owner(ctx, true)
		session := fixture.session(ctx, true, 5*time.Second)
		desired := databaseDesired(name, owner)
		databaseApply(ctx, session, desired)
		// This SET-only administrator creates as the target schema owner.
		// Give that role CREATE explicitly instead of relying on an implicit grant.
		_, err := fixture.Server.Admin.ExecContext(ctx,
			"GRANT CREATE ON DATABASE "+quoteIdentifier(name)+" TO "+quoteIdentifier(other))
		Expect(err).NotTo(HaveOccurred())
		desired.Spec.Schemas = []epco.SchemaSpec{
			schemaSpec("new_default", "", ""), schemaSpec(databaseExplicitSchema, other, ""),
		}
		databaseApply(ctx, session, desired)
		schemas, err := session.InspectSchemas(ctx, name, desired.Spec.Schemas)
		Expect(err).NotTo(HaveOccurred())
		Expect(schemas).To(ConsistOf(pgdatabase.SchemaState{Name: "new_default", Owner: owner, Exists: true},
			pgdatabase.SchemaState{Name: databaseExplicitSchema, Owner: other, Exists: true}))
		desired.Spec.Schemas = []epco.SchemaSpec{schemaSpec(databaseExplicitSchema, "", "")}
		databaseApply(ctx, session, desired)
		schemas, err = session.InspectSchemas(ctx, name, []epco.SchemaSpec{schemaSpec("new_default", "", ""),
			schemaSpec(databaseExplicitSchema, "", "")})
		Expect(err).NotTo(HaveOccurred())
		Expect(schemas).To(ContainElement(pgdatabase.SchemaState{Name: databaseExplicitSchema, Owner: other, Exists: true}))
		Expect(schemas).To(ContainElement(pgdatabase.SchemaState{Name: "new_default", Owner: owner, Exists: true}))
		fixture.inheritOwner(ctx, other)
		desired.Spec.Schemas = []epco.SchemaSpec{schemaSpec(databaseExplicitSchema, owner, "")}
		databaseApply(ctx, session, desired)
		schemas, err = session.InspectSchemas(ctx, name, desired.Spec.Schemas)
		Expect(err).NotTo(HaveOccurred())
		Expect(schemas).To(ConsistOf(pgdatabase.SchemaState{Name: databaseExplicitSchema, Owner: owner, Exists: true}))
		state, err := session.Inspect(ctx, name)
		Expect(err).NotTo(HaveOccurred())
		Expect(state.AdminUser).To(Equal(fixture.Role), "SET LOCAL ROLE must not leak into the administrative session")
	}, NodeTimeout(30*time.Second))

	It("creates a schema for a SET-accessible owner using the administrator's existing CREATE authority",
		func(ctx SpecContext) {
			name, owner := fixture.name(), fixture.owner(ctx, true)
			other := fixture.owner(ctx, true)
			fixture.inheritOwner(ctx, owner)
			session := fixture.session(ctx, true, 5*time.Second)
			desired := databaseDesired(name, owner)
			databaseApply(ctx, session, desired)
			desired.Spec.Schemas = []epco.SchemaSpec{schemaSpec("delegated", other, "")}
			databaseApply(ctx, session, desired)
			schemas, err := session.InspectSchemas(ctx, name, desired.Spec.Schemas)
			Expect(err).NotTo(HaveOccurred())
			Expect(schemas).To(ConsistOf(pgdatabase.SchemaState{Name: "delegated", Owner: other, Exists: true}))
			var canCreate bool
			Expect(fixture.Server.Admin.QueryRowContext(ctx,
				"SELECT has_database_privilege($1,$2,'CREATE')", other, name).Scan(&canCreate)).To(Succeed())
			Expect(canCreate).To(BeFalse(), "creating with AUTHORIZATION must not grant the owner's missing CREATE")
		}, NodeTimeout(30*time.Second))

	DescribeTable("transfers an existing schema to a NOLOGIN owner without destination database CREATE",
		func(ctx SpecContext, restricted bool) {
			name, owner := fixture.name(), fixture.owner(ctx, true)
			other := fixture.owner(ctx, true)
			fixture.inheritOwner(ctx, owner)
			session := fixture.session(ctx, restricted, 5*time.Second)
			desired := databaseDesired(name, owner)
			desired.Spec.Schemas = []epco.SchemaSpec{schemaSpec(databaseOwnedSchema, "", "")}
			databaseApply(ctx, session, desired)
			target := fixture.target(name)
			_, err := target.ExecContext(ctx, "SET ROLE "+quoteIdentifier(owner)+
				"; CREATE TABLE owned.preserved(value int); INSERT INTO owned.preserved VALUES(42); RESET ROLE")
			Expect(err).NotTo(HaveOccurred())

			user, password := roleSQLAdmin, fixture.Server.password
			if restricted {
				user, password = fixture.Role, fixture.Password
			}
			executor, err := fixture.Server.Open(name, user, password)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { Expect(executor.Close()).To(Succeed()) })
			setPrivilege := databaseMemberCapability
			if fixture.version >= 160000 {
				setPrivilege = databaseSetCapability
			}
			var executingUser string
			var superuser, executorCreate, currentOwnerAccess, destinationAccess, destinationCreate, destinationLogin bool
			Expect(executor.QueryRowContext(ctx, `SELECT current_user,
				(SELECT rolsuper FROM pg_roles WHERE rolname=current_user),
				has_database_privilege(current_user,$1,'CREATE'),
				pg_has_role(current_user,$2,'USAGE'), pg_has_role(current_user,$3,$4),
				has_database_privilege($3,$1,'CREATE'),
				(SELECT rolcanlogin FROM pg_roles WHERE rolname=$3)`, name, owner, other, setPrivilege).
				Scan(&executingUser, &superuser, &executorCreate, &currentOwnerAccess,
					&destinationAccess, &destinationCreate, &destinationLogin)).To(Succeed())
			Expect(executingUser).To(Equal(user))
			Expect(superuser).To(Equal(!restricted))
			Expect(executorCreate).To(BeTrue())
			Expect(currentOwnerAccess).To(BeTrue())
			Expect(destinationAccess).To(BeTrue())
			Expect(destinationCreate).To(BeFalse())
			Expect(destinationLogin).To(BeFalse())
			schemas, err := session.InspectSchemas(ctx, name, desired.Spec.Schemas)
			Expect(err).NotTo(HaveOccurred())
			Expect(schemas).To(ConsistOf(pgdatabase.SchemaState{Name: databaseOwnedSchema, Owner: owner, Exists: true}))

			desired.Spec.Schemas = []epco.SchemaSpec{schemaSpec(databaseOwnedSchema, other, "")}
			databaseApply(ctx, session, desired)
			schemas, err = session.InspectSchemas(ctx, name, desired.Spec.Schemas)
			Expect(err).NotTo(HaveOccurred())
			Expect(schemas).To(ConsistOf(pgdatabase.SchemaState{Name: databaseOwnedSchema, Owner: other, Exists: true}))
			var schemaOwner, tableOwner string
			var value int
			Expect(target.QueryRowContext(ctx, `SELECT value, pg_get_userbyid(relowner), pg_get_userbyid(nspowner)
				FROM owned.preserved JOIN pg_class ON oid='owned.preserved'::regclass
				JOIN pg_namespace ON pg_namespace.oid=relnamespace`).Scan(&value, &tableOwner, &schemaOwner)).To(Succeed())
			Expect(schemaOwner).To(Equal(other), "catalog ownership must confirm the successful transfer")
			Expect(value).To(Equal(42))
			Expect(tableOwner).To(Equal(owner), "schema transfer must not reassign existing tables")
			var canCreate bool
			Expect(target.QueryRowContext(ctx,
				"SELECT has_database_privilege($1,$2,'CREATE')", other, name).Scan(&canCreate)).To(Succeed())
			Expect(canCreate).To(BeFalse(), "schema transfer must not grant missing destination CREATE")
		}, Entry("under a superuser executor", false, NodeTimeout(30*time.Second)),
		Entry("under a restricted executor", true, NodeTimeout(30*time.Second)))

	It("rejects schema owner transfer when the executing schema owner lacks database CREATE",
		func(ctx SpecContext) {
			name, owner := fixture.name(), fixture.owner(ctx, true)
			other := fixture.owner(ctx, true)
			session := fixture.session(ctx, true, 5*time.Second)
			desired := databaseDesired(name, owner)
			databaseApply(ctx, session, desired)
			target := fixture.target(name)
			_, err := target.ExecContext(ctx, "GRANT CREATE ON DATABASE "+quoteIdentifier(name)+
				" TO "+quoteIdentifier(fixture.Role)+", "+quoteIdentifier(other))
			Expect(err).NotTo(HaveOccurred())
			executor, err := fixture.Server.Open(name, fixture.Role, fixture.Password)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { Expect(executor.Close()).To(Succeed()) })
			_, err = executor.ExecContext(ctx,
				"CREATE SCHEMA owned; CREATE TABLE owned.preserved(value int); INSERT INTO owned.preserved VALUES(42)")
			Expect(err).NotTo(HaveOccurred())
			_, err = target.ExecContext(ctx, "REVOKE CREATE ON DATABASE "+quoteIdentifier(name)+
				" FROM "+quoteIdentifier(fixture.Role))
			Expect(err).NotTo(HaveOccurred())
			setPrivilege := databaseMemberCapability
			if fixture.version >= 160000 {
				setPrivilege = databaseSetCapability
			}
			var executorCreate, destinationCreate, inheritsDatabaseOwner, destinationAccess bool
			var schemaOwner string
			Expect(executor.QueryRowContext(ctx, `SELECT has_database_privilege(current_user,$1,'CREATE'),
				has_database_privilege($2,$1,'CREATE'), pg_has_role(current_user,$3,'USAGE'),
				pg_has_role(current_user,$2,$4),
				(SELECT pg_get_userbyid(nspowner) FROM pg_namespace WHERE nspname='owned')`,
				name, other, owner, setPrivilege).
				Scan(&executorCreate, &destinationCreate, &inheritsDatabaseOwner, &destinationAccess, &schemaOwner)).To(Succeed())
			Expect(executorCreate).To(BeFalse())
			Expect(destinationCreate).To(BeTrue(), "the destination CREATE check cannot explain this refusal")
			Expect(inheritsDatabaseOwner).To(BeFalse())
			Expect(destinationAccess).To(BeTrue())
			Expect(schemaOwner).To(Equal(fixture.Role))
			_, err = executor.ExecContext(ctx, "ALTER SCHEMA owned OWNER TO "+quoteIdentifier(other))
			Expect(SQLState(err)).To(Equal("42501"), "the actual PostgreSQL executor lacks required CREATE")

			desired.Spec.Schemas = []epco.SchemaSpec{schemaSpec(databaseOwnedSchema, other, "")}
			result, err := session.Apply(ctx, desired)
			Expect(failureReason(err)).To(Equal(pgdatabase.InsufficientPrivileges))
			Expect(result).NotTo(BeNil(), "the independently confirmed database core returns a partial receipt")
			Expect(result.BaseManaged).To(BeTrue(), "a denied child operation does not erase confirmed core management")
			schemas, err := session.InspectSchemas(ctx, name, desired.Spec.Schemas)
			Expect(err).NotTo(HaveOccurred())
			Expect(schemas).To(ConsistOf(pgdatabase.SchemaState{Name: databaseOwnedSchema, Owner: fixture.Role, Exists: true}))
			var value int
			var tableOwner string
			Expect(target.QueryRowContext(ctx, `SELECT value, pg_get_userbyid(relowner)
				FROM owned.preserved JOIN pg_class ON oid='owned.preserved'::regclass`).
				Scan(&value, &tableOwner)).To(Succeed())
			Expect(value).To(Equal(42))
			Expect(tableOwner).To(Equal(fixture.Role))
			Expect(target.QueryRowContext(ctx, `SELECT has_database_privilege($1,$3,'CREATE'),
				has_database_privilege($2,$3,'CREATE')`, fixture.Role, other, name).
				Scan(&executorCreate, &destinationCreate)).To(Succeed())
			Expect(executorCreate).To(BeFalse(), "failed transfer must not grant missing executor CREATE")
			Expect(destinationCreate).To(BeTrue())
		}, NodeTimeout(30*time.Second))

	It("uses RESTRICT for schema absence, keeps table data and retries after dependencies are removed",
		func(ctx SpecContext) {
			name, owner := fixture.name(), fixture.owner(ctx, true)
			session := fixture.session(ctx, true, 5*time.Second)
			desired := databaseDesired(name, owner)
			desired.Spec.Schemas = []epco.SchemaSpec{schemaSpec("empty", "", ""), schemaSpec("busy", "", "")}
			databaseApply(ctx, session, desired)
			target := fixture.target(name)
			_, err := target.ExecContext(ctx, "CREATE TABLE busy.values(value int); INSERT INTO busy.values VALUES(42)")
			Expect(err).NotTo(HaveOccurred())
			desired.Spec.Schemas = []epco.SchemaSpec{schemaSpec("empty", "", epco.EnsureAbsent)}
			databaseApply(ctx, session, desired)
			desired.Spec.Schemas = []epco.SchemaSpec{schemaSpec("busy", "", epco.EnsureAbsent)}
			_, err = session.Apply(ctx, desired)
			Expect(failureReason(err)).To(Equal(pgdatabase.DeletionBlocked))
			var value int
			Expect(target.QueryRowContext(ctx, "SELECT value FROM busy.values").Scan(&value)).To(Succeed())
			Expect(value).To(Equal(42))
			_, err = target.ExecContext(ctx, "DROP TABLE busy.values")
			Expect(err).NotTo(HaveOccurred())
			databaseApply(ctx, session, desired)
			schemas, err := session.InspectSchemas(ctx, name, []epco.SchemaSpec{schemaSpec("busy", "", ""),
				schemaSpec("empty", "", "")})
			Expect(err).NotTo(HaveOccurred())
			Expect(schemas).To(ConsistOf(pgdatabase.SchemaState{Name: "busy"}, pgdatabase.SchemaState{Name: "empty"}))
			databaseApply(ctx, session, desired)
		}, NodeTimeout(30*time.Second))

	It("does not require an absent schema's desired owner and refuses inaccessible explicit owner changes",
		func(ctx SpecContext) {
			name, owner := fixture.name(), fixture.owner(ctx, true)
			other := fixture.owner(ctx, false)
			session := fixture.session(ctx, true, 5*time.Second)
			desired := databaseDesired(name, owner)
			desired.Spec.Schemas = []epco.SchemaSpec{schemaSpec("application", "", "")}
			databaseApply(ctx, session, desired)
			desired.Spec.Schemas = []epco.SchemaSpec{schemaSpec("application", other, "")}
			_, err := session.Apply(ctx, desired)
			Expect(failureReason(err)).To(Equal(pgdatabase.InsufficientPrivileges))
			Expect(roleMemberships(ctx, fixture.Fixture, fixture.Role)).NotTo(ContainElement(other))
			schemas, err := session.InspectSchemas(ctx, name, desired.Spec.Schemas)
			Expect(err).NotTo(HaveOccurred())
			Expect(schemas).To(ConsistOf(pgdatabase.SchemaState{Name: "application", Owner: owner, Exists: true}))
			desired.Spec.Schemas = []epco.SchemaSpec{schemaSpec("application", "does_not_exist", epco.EnsureAbsent)}
			databaseApply(ctx, session, desired)
		}, NodeTimeout(30*time.Second))

	It("drops an empty default public schema through the actual database owner without SET pg_database_owner",
		func(ctx SpecContext) {
			name, owner := fixture.name(), fixture.owner(ctx, true)
			session := fixture.session(ctx, true, 5*time.Second)
			desired := databaseDesired(name, owner)
			databaseApply(ctx, session, desired)
			desired.Spec.Schemas = []epco.SchemaSpec{schemaSpec("public", "", epco.EnsureAbsent)}
			before, err := session.InspectSchemas(ctx, name, desired.Spec.Schemas)
			Expect(err).NotTo(HaveOccurred())
			Expect(before).To(ConsistOf(pgdatabase.SchemaState{Name: "public", Owner: "pg_database_owner", Exists: true}))
			databaseApply(ctx, session, desired)
			after, err := session.InspectSchemas(ctx, name, desired.Spec.Schemas)
			Expect(err).NotTo(HaveOccurred())
			Expect(after).To(ConsistOf(pgdatabase.SchemaState{Name: "public", Exists: false}))
		}, NodeTimeout(30*time.Second))

	It("allows public management and preflights protected schemas before any database or schema mutation",
		func(ctx SpecContext) {
			name, owner := fixture.name(), fixture.owner(ctx, true)
			session := fixture.session(ctx, true, 5*time.Second)
			desired := databaseDesired(name, owner)
			desired.Spec.Schemas = []epco.SchemaSpec{schemaSpec("public", owner, "")}
			databaseApply(ctx, session, desired)
			for _, protected := range []string{"pg_catalog", "information_schema", "pg_custom"} {
				desired.Spec.ConnectionLimit = new(int64(13))
				desired.Spec.Schemas = []epco.SchemaSpec{schemaSpec("must_not_create", "", ""), schemaSpec(protected,
					owner, epco.EnsureAbsent)}
				_, err := session.Apply(ctx, desired)
				Expect(failureReason(err)).To(Equal("ProtectedSchema"))
				state, err := session.Inspect(ctx, name)
				Expect(err).NotTo(HaveOccurred())
				Expect(state.ConnectionLimit).To(Equal(int64(-1)))
				schemas, err := session.InspectSchemas(ctx, name, []epco.SchemaSpec{schemaSpec("must_not_create", "", "")})
				Expect(err).NotTo(HaveOccurred())
				Expect(schemas).To(ConsistOf(pgdatabase.SchemaState{Name: "must_not_create"}))
			}
			missing := databaseDesired(fixture.name(), owner)
			missing.Spec.Schemas = []epco.SchemaSpec{schemaSpec("pg_catalog", owner, "")}
			_, err := session.Apply(ctx, missing)
			Expect(failureReason(err)).To(Equal("ProtectedSchema"))
			Expect(fixture.exists(ctx, missing.Name)).To(BeFalse())
		}, NodeTimeout(30*time.Second))

	It("repairs disabled admission before opening the target for schema work", func(ctx SpecContext) {
		name, owner := fixture.name(), fixture.owner(ctx, true)
		session := fixture.session(ctx, true, 5*time.Second)
		desired := databaseDesired(name, owner)
		desired.Spec.AllowConnections = new(false)
		databaseApply(ctx, session, desired)
		desired.Spec.Schemas = []epco.SchemaSpec{schemaSpec("application", "", "")}
		_, err := session.Apply(ctx, desired)
		Expect(err).To(HaveOccurred())
		state, err := session.Inspect(ctx, name)
		Expect(err).NotTo(HaveOccurred())
		Expect(state.AllowConnections).To(BeFalse())
		_, err = session.InspectSchemas(ctx, name, desired.Spec.Schemas)
		Expect(err).To(HaveOccurred())
		desired.Spec.AllowConnections = new(true)
		databaseApply(ctx, session, desired)
		schemas, err := session.InspectSchemas(ctx, name, desired.Spec.Schemas)
		Expect(err).NotTo(HaveOccurred())
		Expect(schemas).To(ConsistOf(pgdatabase.SchemaState{Name: "application", Owner: owner, Exists: true}))
	}, NodeTimeout(30*time.Second))

	It("requires target CONNECT independently of catalog inspection and never grants it implicitly",
		func(ctx SpecContext) {
			name, owner := fixture.name(), fixture.owner(ctx, true)
			session := fixture.session(ctx, true, 5*time.Second)
			desired := databaseDesired(name, owner)
			databaseApply(ctx, session, desired)
			_, err := fixture.Server.Admin.ExecContext(ctx, "REVOKE CONNECT ON DATABASE "+quoteIdentifier(name)+" FROM PUBLIC")
			Expect(err).NotTo(HaveOccurred())
			state, err := session.Inspect(ctx, name)
			Expect(err).NotTo(HaveOccurred())
			Expect(state.Exists).To(BeTrue())
			desired.Spec.Schemas = []epco.SchemaSpec{schemaSpec("application", "", "")}
			_, err = session.Apply(ctx, desired)
			Expect(failureReason(err)).To(Equal(pgdatabase.InsufficientPrivileges))
			var allowed bool
			Expect(fixture.Admin.QueryRowContext(ctx, "SELECT has_database_privilege($1,$2,'CONNECT')",
				fixture.Role, name).Scan(&allowed)).To(Succeed())
			Expect(allowed).To(BeFalse())
			_, err = fixture.Server.Admin.ExecContext(ctx,
				"GRANT CONNECT ON DATABASE "+quoteIdentifier(name)+" TO "+quoteIdentifier(fixture.Role))
			Expect(err).NotTo(HaveOccurred())
			databaseApply(ctx, session, desired)
		}, NodeTimeout(30*time.Second))

	It("bounds blocked target work and closes connections on in-flight cancellation", func(ctx SpecContext) {
		name, owner := fixture.name(), fixture.owner(ctx, true)
		session := fixture.session(ctx, true, 5*time.Second)
		desired := databaseDesired(name, owner)
		databaseApply(ctx, session, desired)
		target := fixture.target(name)
		locker, err := target.BeginTx(ctx, nil)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = locker.Rollback() })
		_, err = locker.ExecContext(ctx, "LOCK TABLE pg_catalog.pg_namespace IN ACCESS EXCLUSIVE MODE")
		Expect(err).NotTo(HaveOccurred())
		desired.Spec.Schemas = []epco.SchemaSpec{schemaSpec("cancelled", "", "")}
		canceled, cancel := context.WithCancel(ctx)
		defer cancel()
		finished := make(chan error, 1)
		go func() { _, err := session.Apply(canceled, desired); finished <- err }()
		Eventually(func() (int, error) {
			var waiting int
			err := fixture.Server.Admin.QueryRowContext(ctx, `SELECT count(*) FROM pg_stat_activity
				WHERE datname=$1 AND usename=$2 AND wait_event_type='Lock'`, name, fixture.Role).Scan(&waiting)
			return waiting, err
		}, 3*time.Second, 20*time.Millisecond).Should(BeNumerically(">", 0))
		cancel()
		var result error
		Eventually(finished, 2*time.Second).Should(Receive(&result))
		Expect(result).To(HaveOccurred())
		Eventually(func() (int, error) {
			var count int
			err := fixture.Server.Admin.QueryRowContext(ctx,
				"SELECT count(*) FROM pg_stat_activity WHERE datname=$1 AND usename=$2", name, fixture.Role).Scan(&count)
			return count, err
		}, 2*time.Second, 20*time.Millisecond).Should(Equal(0))
		Expect(locker.Rollback()).To(Succeed())
		var exists bool
		Expect(target.QueryRowContext(ctx,
			"SELECT EXISTS(SELECT 1 FROM pg_namespace WHERE nspname='cancelled')").Scan(&exists)).To(Succeed())
		Expect(exists).To(BeFalse())
		// Retry uses a fresh session after cancellation, never a leaked target role.
		fresh := fixture.session(ctx, true, 100*time.Millisecond)
		locker, err = target.BeginTx(ctx, nil)
		Expect(err).NotTo(HaveOccurred())
		_, err = locker.ExecContext(ctx, "LOCK TABLE pg_catalog.pg_namespace IN ACCESS EXCLUSIVE MODE")
		Expect(err).NotTo(HaveOccurred())
		started := time.Now()
		_, err = fresh.Apply(ctx, desired)
		Expect(err).To(HaveOccurred())
		Expect(time.Since(started)).To(BeNumerically("<", 2*time.Second))
		Expect(locker.Rollback()).To(Succeed())
		fresh = fixture.session(ctx, true, 5*time.Second)
		databaseApply(ctx, fresh, desired)
	}, NodeTimeout(30*time.Second))

	It("does not transfer database ownership when SET-only authority would lose administrator CREATEDB",
		func(ctx SpecContext) {
			name, owner := fixture.name(), fixture.owner(ctx, true)
			other := fixture.owner(ctx, true)
			session := fixture.session(ctx, true, 5*time.Second)
			databaseApply(ctx, session, databaseDesired(name, owner))
			_, err := session.Apply(ctx, databaseDesired(name, other))
			Expect(failureReason(err)).To(Equal(pgdatabase.InsufficientPrivileges))
			state, err := session.Inspect(ctx, name)
			Expect(err).NotTo(HaveOccurred())
			Expect(state.Owner).To(Equal(owner))
			var createdb bool
			Expect(fixture.Admin.QueryRowContext(ctx, "SELECT rolcreatedb FROM pg_roles WHERE rolname=$1",
				owner).Scan(&createdb)).To(Succeed())
			Expect(createdb).To(BeFalse())
		}, NodeTimeout(30*time.Second))

	It("does not grant CREATE to a requested schema owner with insufficient database authority", func(ctx SpecContext) {
		name, owner := fixture.name(), fixture.owner(ctx, true)
		other := fixture.owner(ctx, true)
		session := fixture.session(ctx, true, 5*time.Second)
		desired := databaseDesired(name, owner)
		databaseApply(ctx, session, desired)
		desired.Spec.Schemas = []epco.SchemaSpec{schemaSpec(databaseExplicitSchema, other, "")}
		_, err := session.Apply(ctx, desired)
		Expect(failureReason(err)).To(Equal(pgdatabase.InsufficientPrivileges))
		var allowed bool
		Expect(fixture.Admin.QueryRowContext(ctx, "SELECT has_database_privilege($1,$2,'CREATE')", other,
			name).Scan(&allowed)).To(Succeed())
		Expect(allowed).To(BeFalse())
		schemas, err := session.InspectSchemas(ctx, name, desired.Spec.Schemas)
		Expect(err).NotTo(HaveOccurred())
		Expect(schemas).To(ConsistOf(pgdatabase.SchemaState{Name: databaseExplicitSchema}))
	}, NodeTimeout(30*time.Second))
})
