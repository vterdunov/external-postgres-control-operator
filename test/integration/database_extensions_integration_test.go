//go:build integration

package integration

import (
	"context"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	epco "github.com/vterdunov/external-postgres-control-operator/api/v1alpha1"
	pgdatabase "github.com/vterdunov/external-postgres-control-operator/internal/postgres"
)

const issue33SetMembershipOptions = " WITH INHERIT FALSE, SET TRUE"

var _ = Describe("Database extension PostgreSQL reconciliation", func() {
	var fixture *extensionSQLFixture
	BeforeEach(func(ctx SpecContext) { fixture = newExtensionSQLFixture(ctx) }, NodeTimeout(30*time.Second))

	It("installs the server default extension into the selected target database and confirms SQL usability",
		func(ctx SpecContext) {
			session := fixture.session(ctx, false, 5*time.Second)
			var version string
			Expect(fixture.Admin.QueryRowContext(ctx,
				"SELECT default_version FROM pg_catalog.pg_available_extensions WHERE name='hstore'").Scan(&version)).To(Succeed())
			Expect(extensionCatalog(ctx, fixture.Admin, "hstore").Exists).To(BeFalse())
			databaseApply(ctx, session, fixture.desired(extensionSpec("hstore", "", "", "")))
			state := extensionCatalog(ctx, fixture.Admin, "hstore")
			Expect(state.Exists).To(BeTrue())
			Expect(state.Version).To(Equal(version))
			Expect(state.Schema).To(Equal("public"))
			Expect(state.Owner).To(Equal(roleSQLAdmin))
			var value string
			Expect(fixture.Admin.QueryRowContext(ctx,
				"SELECT 'answer=>42'::public.hstore -> 'answer'").Scan(&value)).To(Succeed())
			Expect(value).To(Equal("42"))
			Expect(extensionCatalog(ctx, fixture.Server.Admin, "hstore").Exists).To(BeFalse())
		}, NodeTimeout(30*time.Second))

	It("installs an explicitly older opaque version and preserves unspecified version and schema",
		func(ctx SpecContext) {
			pkg := fixture.packageFiles(ctx, "", nil)
			session := fixture.session(ctx, false, 5*time.Second)
			fixture.sql(ctx, `CREATE SCHEMA "extension's space"`)
			databaseApply(ctx, session, fixture.desired(extensionSpec(pkg.Name, "seed", "extension's space", "")))
			before := extensionCatalog(ctx, fixture.Admin, pkg.Name)
			Expect(before.Version).To(Equal("seed"))
			Expect(fixture.memberValue(ctx, pkg, "extension's space")).To(Equal(1))
			databaseApply(ctx, session, fixture.desired(extensionSpec(pkg.Name, "", "", "")))
			Expect(extensionCatalog(ctx, fixture.Admin, pkg.Name)).To(Equal(before))
			databaseApply(ctx, session, fixture.desired())
			Expect(extensionCatalog(ctx, fixture.Admin, pkg.Name)).To(Equal(before), "omission retains the SQL object")
		}, NodeTimeout(30*time.Second))

	It("quotes extension names schemas and version labels longer than SQL identifiers "+
		"without interpreting semver", func(ctx SpecContext) {
		version := "release's " + strings.Repeat("opaque", 15)
		name, err := randomName("quoted\"_")
		Expect(err).NotTo(HaveOccurred())
		pkg := fixture.namedPackageFiles(ctx, name, "", map[string]string{"--" + version +
			".sql": "CREATE TABLE @members@ (value integer); INSERT INTO @members@ VALUES (73);"})
		fixture.sql(ctx, `CREATE SCHEMA "tenant""; DROP SCHEMA public; --"`)
		session := fixture.session(ctx, false, 5*time.Second)
		databaseApply(ctx, session, fixture.desired(extensionSpec(pkg.Name, version,
			`tenant"; DROP SCHEMA public; --`, "")))
		state := extensionCatalog(ctx, fixture.Admin, pkg.Name)
		Expect(state.Version).To(Equal(version))
		Expect(state.Schema).To(Equal(`tenant"; DROP SCHEMA public; --`))
		Expect(fixture.memberValue(ctx, pkg, state.Schema)).To(Equal(73))
		var exists bool
		Expect(fixture.Admin.QueryRowContext(ctx,
			"SELECT EXISTS(SELECT 1 FROM pg_catalog.pg_namespace WHERE nspname='public')").Scan(&exists)).To(Succeed())
		Expect(exists).To(BeTrue())
	}, NodeTimeout(30*time.Second))

	It("uses a supported multi-step update path and retains extension and member identity", func(ctx SpecContext) {
		pkg := fixture.packageFiles(ctx, "", nil)
		fixture.create(ctx, pkg, "seed")
		before := extensionCatalog(ctx, fixture.Admin, pkg.Name)
		var member uint32
		Expect(fixture.Admin.QueryRowContext(ctx, "SELECT pg_catalog.to_regclass($1)::oid", "public."+
			quoteIdentifier(pkg.Name+"_members")).Scan(&member)).To(Succeed())
		databaseApply(ctx, fixture.session(ctx, false, 5*time.Second), fixture.desired(extensionSpec(pkg.Name,
			"omega", "", "")))
		after := extensionCatalog(ctx, fixture.Admin, pkg.Name)
		Expect(after.Version).To(Equal("omega"))
		Expect(after.OID).To(Equal(before.OID))
		Expect(fixture.memberValue(ctx, pkg, "public")).To(Equal(3), "both scripts must execute")
		var afterMember uint32
		Expect(fixture.Admin.QueryRowContext(ctx, "SELECT pg_catalog.to_regclass($1)::oid", "public."+
			quoteIdentifier(pkg.Name+"_members")).Scan(&afterMember)).To(Succeed())
		Expect(afterMember).To(Equal(member), "update must not drop and recreate script members")
	}, NodeTimeout(30*time.Second))

	DescribeTable("rejects unavailable versions and absent update paths without changing installed data",
		func(ctx SpecContext, version string) {
			pkg := fixture.packageFiles(ctx, "", nil)
			fixture.create(ctx, pkg, "omega")
			before := extensionCatalog(ctx, fixture.Admin, pkg.Name)
			_, err := fixture.session(ctx, false, 5*time.Second).Apply(ctx, fixture.desired(extensionSpec(pkg.Name,
				version, "", "")))
			Expect(failureReason(err)).To(Equal(pgdatabase.UnsupportedFeature))
			Expect(extensionCatalog(ctx, fixture.Admin, pkg.Name)).To(Equal(before))
			Expect(fixture.memberValue(ctx, pkg, "public")).To(Equal(3))
		}, Entry("missing version", "unavailable", NodeTimeout(30*time.Second)), Entry(
			"no update path", "orphan", NodeTimeout(30*time.Second)))

	DescribeTable("combines relocation and version changes atomically across capability transitions",
		func(ctx SpecContext, from, to string, relocation bool, value int) {
			pkg := fixture.packageFiles(ctx, "", nil)
			fixture.create(ctx, pkg, from)
			fixture.sql(ctx, "CREATE SCHEMA destination")
			before := extensionCatalog(ctx, fixture.Admin, pkg.Name)
			databaseApply(ctx, fixture.session(ctx, false, 5*time.Second), fixture.desired(extensionSpec(pkg.Name, to,
				"destination", "")))
			after := extensionCatalog(ctx, fixture.Admin, pkg.Name)
			Expect(after.Version).To(Equal(to))
			Expect(after.Schema).To(Equal("destination"))
			Expect(after.Relocatable).To(Equal(relocation))
			Expect(after.OID).To(Equal(before.OID))
			Expect(fixture.memberValue(ctx, pkg, "destination")).To(Equal(value))
		}, Entry("moves before target disables relocation", "omega", "locked", false, 4, NodeTimeout(30*time.Second)),
		Entry("updates before target enables relocation", "locked", "omega", true, 3, NodeTimeout(30*time.Second)))

	It("rolls back relocation and script writes when the combined update script fails", func(ctx SpecContext) {
		pkg := fixture.packageFiles(ctx, "", nil)
		fixture.create(ctx, pkg, "omega")
		fixture.sql(ctx, "CREATE SCHEMA destination")
		before := extensionCatalog(ctx, fixture.Admin, pkg.Name)
		_, err := fixture.session(ctx, false, 5*time.Second).Apply(ctx, fixture.desired(extensionSpec(pkg.Name,
			"broken", "destination", "")))
		Expect(err).To(HaveOccurred())
		Expect(failureReason(err)).NotTo(Equal(pgdatabase.UnsupportedFeature),
			"the available broken script must execute rather than fail at an unsupported-operation gate")
		Expect(extensionCatalog(ctx, fixture.Admin, pkg.Name)).To(Equal(before), "MOVE must roll back with failed UPDATE")
		Expect(fixture.memberValue(ctx, pkg, "public")).To(Equal(3), "script writes must roll back")
	}, NodeTimeout(30*time.Second))

	It("rolls back a successful enabling update when subsequent relocation lacks schema authority",
		func(ctx SpecContext) {
			pkg := fixture.packageFiles(ctx, "", nil)
			fixture.sql(ctx, "GRANT CREATE ON DATABASE "+quoteIdentifier(fixture.Database)+" TO "+
				quoteIdentifier(fixture.Role)+"; GRANT CREATE ON SCHEMA public TO "+quoteIdentifier(fixture.Role)+
				"; CREATE SCHEMA denied")
			_, err := fixture.Limited.ExecContext(ctx, "CREATE EXTENSION "+quoteIdentifier(pkg.Name)+
				" VERSION 'locked' SCHEMA public")
			Expect(err).NotTo(HaveOccurred())
			before := extensionCatalog(ctx, fixture.Admin, pkg.Name)
			_, err = fixture.session(ctx, true, 5*time.Second).Apply(ctx, fixture.desired(extensionSpec(pkg.Name,
				"omega", "denied", "")))
			Expect(failureReason(err)).To(Equal(pgdatabase.InsufficientPrivileges))
			Expect(extensionCatalog(ctx, fixture.Admin, pkg.Name)).To(Equal(before))
			Expect(fixture.memberValue(ctx, pkg, "public")).To(Equal(4), "successful first UPDATE must roll back")
		}, NodeTimeout(30*time.Second))

	It("installs nonrelocatable unfixed extensions into explicit schemas but refuses later moves",
		func(ctx SpecContext) {
			pkg := fixture.packageFiles(ctx, "", nil)
			fixture.sql(ctx, "CREATE SCHEMA original; CREATE SCHEMA destination")
			session := fixture.session(ctx, false, 5*time.Second)
			databaseApply(ctx, session, fixture.desired(extensionSpec(pkg.Name, "locked", "original", "")))
			before := extensionCatalog(ctx, fixture.Admin, pkg.Name)
			Expect(before.Schema).To(Equal("original"))
			Expect(before.Relocatable).To(BeFalse())
			_, err := session.Apply(ctx, fixture.desired(extensionSpec(pkg.Name, "locked", "destination", "")))
			Expect(failureReason(err)).To(Equal(pgdatabase.UnsupportedFeature))
			Expect(extensionCatalog(ctx, fixture.Admin, pkg.Name)).To(Equal(before))
		}, NodeTimeout(30*time.Second))

	It("lets PostgreSQL create a missing control-fixed schema and rejects conflicting install schemas",
		func(ctx SpecContext) {
			name, err := randomName("fixed_")
			Expect(err).NotTo(HaveOccurred())
			pkg := fixture.packageFiles(ctx, "relocatable = false\nschema = '"+name+"'\n", nil)
			session := fixture.session(ctx, false, 5*time.Second)
			var exists bool
			Expect(fixture.Admin.QueryRowContext(ctx,
				"SELECT EXISTS(SELECT 1 FROM pg_catalog.pg_namespace WHERE nspname=$1)", name).Scan(&exists)).To(Succeed())
			Expect(exists).To(BeFalse())
			databaseApply(ctx, session, fixture.desired(extensionSpec(pkg.Name, "", "", "")))
			Expect(extensionCatalog(ctx, fixture.Admin, pkg.Name).Schema).To(Equal(name))
			Expect(fixture.memberValue(ctx, pkg, name)).To(Equal(3))
			other := fixture.packageFiles(ctx, "relocatable = false\nschema = '"+name+"'\n", nil)
			_, err = session.Apply(ctx, fixture.desired(extensionSpec(other.Name, "", "public", "")))
			Expect(failureReason(err)).To(Equal(pgdatabase.UnsupportedFeature))
			Expect(extensionCatalog(ctx, fixture.Admin, other.Name).Exists).To(BeFalse())
		}, NodeTimeout(30*time.Second))

	It("waits for undeclared prerequisites and explicit missing schemas without using CREATE CASCADE",
		func(ctx SpecContext) {
			prerequisite := fixture.packageFiles(ctx, "", nil)
			dependent := fixture.packageFiles(ctx, "requires = '"+prerequisite.Name+"'\n", nil)
			session := fixture.session(ctx, false, 5*time.Second)
			_, err := session.Apply(ctx, fixture.desired(extensionSpec(dependent.Name, "", "", "")))
			Expect(failureReason(err)).To(Equal(pgdatabase.DependenciesNotReady))
			Expect(extensionCatalog(ctx, fixture.Admin, prerequisite.Name).Exists).To(BeFalse())
			Expect(extensionCatalog(ctx, fixture.Admin, dependent.Name).Exists).To(BeFalse())
			fixture.create(ctx, prerequisite, "omega")
			_, err = session.Apply(ctx, fixture.desired(extensionSpec(dependent.Name, "", "missing", "")))
			Expect(failureReason(err)).To(Equal(pgdatabase.DependenciesNotReady))
			fixture.sql(ctx, "CREATE SCHEMA missing")
			databaseApply(ctx, session, fixture.desired(extensionSpec(dependent.Name, "", "missing", "")))
		}, NodeTimeout(30*time.Second))

	It("creates declared prerequisites and schemas then removes dependents before "+
		"prerequisites in arbitrary input order", func(ctx SpecContext) {
		prerequisite := fixture.packageFiles(ctx, "", nil)
		dependent := fixture.packageFiles(ctx, "requires = '"+prerequisite.Name+"'\n", nil)
		session := fixture.session(ctx, false, 5*time.Second)
		desired := fixture.desired(extensionSpec(dependent.Name, "", "objects", ""),
			extensionSpec(prerequisite.Name, "", "objects", ""))
		desired.Spec.Schemas = []epco.SchemaSpec{schemaSpec("objects", "", "")}
		databaseApply(ctx, session, desired)
		Expect(extensionCatalog(ctx, fixture.Admin, prerequisite.Name).Exists).To(BeTrue())
		Expect(extensionCatalog(ctx, fixture.Admin, dependent.Name).Exists).To(BeTrue())
		desired.Spec.Extensions = []epco.ExtensionSpec{extensionSpec(prerequisite.Name, "ignored", "missing",
			epco.EnsureAbsent), extensionSpec(dependent.Name, "ignored", "missing", epco.EnsureAbsent)}
		desired.Spec.Schemas[0].Ensure = epco.EnsureAbsent
		databaseApply(ctx, session, desired)
		Expect(extensionCatalog(ctx, fixture.Admin, prerequisite.Name).Exists).To(BeFalse())
		Expect(extensionCatalog(ctx, fixture.Admin, dependent.Name).Exists).To(BeFalse())
		var exists bool
		Expect(fixture.Admin.QueryRowContext(ctx,
			"SELECT EXISTS(SELECT 1 FROM pg_catalog.pg_namespace WHERE nspname='objects')").Scan(&exists)).To(Succeed())
		Expect(exists).To(BeFalse(), "schema drops follow extension member cleanup")
	}, NodeTimeout(30*time.Second))

	It("retains own members on no-op and observation without package files and drops "+
		"members through explicit absence", func(ctx SpecContext) {
		pkg := fixture.packageFiles(ctx, "", nil)
		fixture.create(ctx, pkg, "seed")
		before := extensionCatalog(ctx, fixture.Admin, pkg.Name)
		fixture.removePackage(ctx, pkg)
		session := fixture.session(ctx, false, 5*time.Second)
		databaseApply(ctx, session, fixture.desired(extensionSpec(pkg.Name, "seed", "public", "")))
		observed := fixture.desired(extensionSpec(pkg.Name, "unavailable", "missing", ""))
		observed.Spec.ManagementPolicy = epco.ManagementObserve
		_, err := session.Apply(ctx, observed)
		Expect(failureReason(err)).To(Equal(pgdatabase.DependenciesNotReady))
		Expect(extensionCatalog(ctx, fixture.Admin, pkg.Name)).To(Equal(before))
		Expect(fixture.memberValue(ctx, pkg, "public")).To(Equal(1))
		observed.Spec.Extensions = []epco.ExtensionSpec{extensionSpec(pkg.Name, "seed", "public", "")}
		databaseApply(ctx, session, observed)
		databaseApply(ctx, session, fixture.desired(extensionSpec(pkg.Name, "unavailable", "missing", epco.EnsureAbsent)))
		Expect(extensionCatalog(ctx, fixture.Admin, pkg.Name).Exists).To(BeFalse())
		var memberExists bool
		Expect(fixture.Admin.QueryRowContext(ctx, "SELECT pg_catalog.to_regclass($1) IS NOT NULL", "public."+
			quoteIdentifier(pkg.Name+"_members")).Scan(&memberExists)).To(Succeed())
		Expect(memberExists).To(BeFalse())
		databaseApply(ctx, session, fixture.desired(extensionSpec(pkg.Name, "", "", epco.EnsureAbsent)))
	}, NodeTimeout(30*time.Second))

	It("protects external table type dependencies and data using DROP RESTRICT then converges after removal",
		func(ctx SpecContext) {
			pkg := fixture.packageFiles(ctx, "", nil)
			fixture.create(ctx, pkg, "omega")
			fixture.sql(ctx, "CREATE TABLE public.application_data (value integer, payload "+quoteIdentifier(pkg.Name+
				"_type")+"); INSERT INTO public.application_data VALUES (42, 'preserved')")
			before := extensionCatalog(ctx, fixture.Admin, pkg.Name)
			session := fixture.session(ctx, false, 5*time.Second)
			desired := fixture.desired(extensionSpec(pkg.Name, "", "", epco.EnsureAbsent))
			_, err := session.Apply(ctx, desired)
			Expect(failureReason(err)).To(Equal(pgdatabase.DeletionBlocked))
			Expect(extensionCatalog(ctx, fixture.Admin, pkg.Name)).To(Equal(before))
			Expect(fixture.memberValue(ctx, pkg, "public")).To(Equal(3))
			var value int
			Expect(fixture.Admin.QueryRowContext(ctx, "SELECT value FROM public.application_data").Scan(&value)).To(Succeed())
			Expect(value).To(Equal(42))
			fixture.sql(ctx, "DROP TABLE public.application_data")
			databaseApply(ctx, session, desired)
			Expect(extensionCatalog(ctx, fixture.Admin, pkg.Name).Exists).To(BeFalse())
		}, NodeTimeout(30*time.Second))

	It("creates and updates trusted extensions under the supplied restricted principal and "+
		"preserves actual ownership", func(ctx SpecContext) {
		pkg := fixture.packageFiles(ctx, "", nil)
		fixture.sql(ctx, "GRANT CREATE ON DATABASE "+quoteIdentifier(fixture.Database)+" TO "+
			quoteIdentifier(fixture.Role)+"; GRANT CREATE ON SCHEMA public TO "+quoteIdentifier(fixture.Role))
		session := fixture.session(ctx, true, 5*time.Second)
		databaseApply(ctx, session, fixture.desired(extensionSpec(pkg.Name, "seed", "public", "")))
		Expect(extensionCatalog(ctx, fixture.Admin, pkg.Name).Owner).To(Equal(fixture.Role),
			"owner is executor rather than Database.spec.owner")
		databaseApply(ctx, session, fixture.desired(extensionSpec(pkg.Name, "omega", "", "")))
		Expect(fixture.memberValue(ctx, pkg, "public")).To(Equal(3))
		Expect(extensionCatalog(ctx, fixture.Admin, pkg.Name).Owner).To(Equal(fixture.Role))
		untrusted := fixture.packageFiles(ctx, "trusted = false\n", nil)
		_, err := session.Apply(ctx, fixture.desired(extensionSpec(untrusted.Name, "", "", "")))
		Expect(failureReason(err)).To(Equal(pgdatabase.InsufficientPrivileges))
		Expect(extensionCatalog(ctx, fixture.Admin, untrusted.Name).Exists).To(BeFalse())
		databaseApply(ctx, session, fixture.desired(extensionSpec(pkg.Name, "", "", epco.EnsureAbsent)))
		var superuser bool
		Expect(fixture.Admin.QueryRowContext(ctx, "SELECT rolsuper FROM pg_catalog.pg_roles WHERE rolname=$1",
			fixture.Role).Scan(&superuser)).To(Succeed())
		Expect(superuser).To(BeFalse())
	}, NodeTimeout(30*time.Second))

	It("refuses update and drop without actual owner authority and never creates memberships", func(ctx SpecContext) {
		pkg := fixture.packageFiles(ctx, "", nil)
		fixture.create(ctx, pkg, "seed")
		fixture.sql(ctx, "GRANT CREATE ON DATABASE "+quoteIdentifier(fixture.Database)+" TO "+quoteIdentifier(fixture.Role))
		before := extensionCatalog(ctx, fixture.Admin, pkg.Name)
		session := fixture.session(ctx, true, 5*time.Second)
		for _, spec := range []epco.ExtensionSpec{extensionSpec(pkg.Name, "omega", "", ""),
			extensionSpec(pkg.Name, "", "", epco.EnsureAbsent)} {
			_, err := session.Apply(ctx, fixture.desired(spec))
			Expect(failureReason(err)).To(Equal(pgdatabase.InsufficientPrivileges))
			Expect(extensionCatalog(ctx, fixture.Admin, pkg.Name)).To(Equal(before))
		}
		Expect(roleMemberships(ctx, fixture.Fixture, fixture.Role)).To(BeEmpty())
	}, NodeTimeout(30*time.Second))

	It("uses existing NOINHERIT SET authority after native denial and rolls back a failed owner retry",
		func(ctx SpecContext) {
			administrator, err := fixture.Server.NewFixture(ctx)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func(ctx SpecContext) {
				fixture.sql(ctx, "REVOKE CONNECT ON DATABASE "+quoteIdentifier(fixture.Database)+" FROM "+
					quoteIdentifier(administrator.Role))
				Expect(administrator.Close(ctx)).To(Succeed())
			})
			fixture.sql(ctx, "GRANT CREATE ON DATABASE "+quoteIdentifier(fixture.Database)+" TO "+quoteIdentifier(fixture.Role)+
				"; GRANT CREATE ON SCHEMA public TO "+quoteIdentifier(fixture.Role)+
				"; GRANT CONNECT ON DATABASE "+quoteIdentifier(fixture.Database)+" TO "+quoteIdentifier(administrator.Role)+
				"; CREATE SCHEMA destination AUTHORIZATION "+quoteIdentifier(fixture.Role)+"; CREATE SCHEMA denied")
			pkg := fixture.packageFiles(ctx, "superuser = false\n", nil)
			_, err = fixture.Limited.ExecContext(ctx, "CREATE EXTENSION "+quoteIdentifier(pkg.Name)+
				" VERSION 'seed' SCHEMA public")
			Expect(err).NotTo(HaveOccurred())
			probe, err := fixture.Limited.BeginTx(ctx, nil)
			Expect(err).NotTo(HaveOccurred())
			_, err = probe.ExecContext(ctx, "ALTER EXTENSION "+quoteIdentifier(pkg.Name)+" SET SCHEMA destination")
			Expect(probe.Rollback()).To(Succeed())
			Expect(err).NotTo(HaveOccurred(), "fixture owner must have native relocation authority")
			fixture.sql(ctx, "ALTER ROLE "+quoteIdentifier(administrator.Role)+" NOINHERIT")
			var version int
			Expect(fixture.Admin.QueryRowContext(ctx, "SELECT current_setting('server_version_num')::integer").
				Scan(&version)).To(Succeed())
			membership := "GRANT " + quoteIdentifier(fixture.Role) + " TO " + quoteIdentifier(administrator.Role)
			if version >= 160000 {
				membership += issue33SetMembershipOptions
			}
			fixture.sql(ctx, membership)
			memberships := roleMemberships(ctx, administrator, administrator.Role)
			native, err := fixture.Server.Open(fixture.Database, administrator.Role, administrator.Password)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { Expect(native.Close()).To(Succeed()) })
			_, err = native.ExecContext(ctx, "ALTER EXTENSION "+quoteIdentifier(pkg.Name)+" UPDATE TO 'omega'")
			Expect(SQLState(err)).To(Equal("42501"), "NOINHERIT login must lack native owner authority before SET")
			config := productionConfig(administrator, roleTLSMode, false)
			config.Connection.Database = fixture.Database
			session, err := pgdatabase.NewDatabaseFactory().Open(ctx, config, pgdatabase.DatabaseSessionOptions{})
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func(ctx SpecContext) { Expect(session.Close(ctx)).To(Succeed()) })
			extensions := session.(pgdatabase.DatabaseExtensionSession)
			_, err = extensions.ReconcileExtensions(ctx, fixture.Database,
				[]epco.ExtensionSpec{extensionSpec(pkg.Name, "omega", "destination", "")})
			Expect(err).NotTo(HaveOccurred(), "the native login needs the existing SET-authorized owner to update and move")
			state := extensionCatalog(ctx, fixture.Admin, pkg.Name)
			Expect(state.Version).To(Equal("omega"))
			Expect(state.Schema).To(Equal("destination"))
			Expect(state.Owner).To(Equal(fixture.Role))
			Expect(fixture.memberValue(ctx, pkg, "destination")).To(Equal(3))
			fixture.sql(ctx, "ALTER EXTENSION "+quoteIdentifier(pkg.Name)+" UPDATE TO 'locked'")
			before := extensionCatalog(ctx, fixture.Admin, pkg.Name)
			_, err = extensions.ReconcileExtensions(ctx, fixture.Database,
				[]epco.ExtensionSpec{extensionSpec(pkg.Name, "omega", "denied", "")})
			Expect(failureReason(err)).To(Equal(pgdatabase.InsufficientPrivileges))
			Expect(extensionCatalog(ctx, fixture.Admin, pkg.Name)).To(Equal(before),
				"failed owner relocation must undo the enabling update")
			Expect(fixture.memberValue(ctx, pkg, "destination")).To(Equal(4),
				"failed retry must roll back extension script writes")
			Expect(roleMemberships(ctx, administrator, administrator.Role)).To(Equal(memberships),
				"existing SET authority is never modified")
		}, NodeTimeout(30*time.Second))

	It("stops lock waits on context cancellation and bounds statement deadlines without "+
		"leaking target sessions", func(ctx SpecContext) {
		pkg := fixture.packageFiles(ctx, "", nil)
		fixture.create(ctx, pkg, "seed")
		before := extensionCatalog(ctx, fixture.Admin, pkg.Name)
		locker, err := fixture.Admin.BeginTx(ctx, nil)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = locker.Rollback() })
		_, err = locker.ExecContext(ctx, "LOCK TABLE public."+quoteIdentifier(pkg.Name+"_members")+
			" IN ACCESS EXCLUSIVE MODE")
		Expect(err).NotTo(HaveOccurred())
		session := fixture.session(ctx, false, 5*time.Second)
		canceled, cancel := context.WithCancel(ctx)
		defer cancel()
		finished := make(chan error, 1)
		go func() {
			_, err := session.Apply(canceled, fixture.desired(extensionSpec(pkg.Name, "omega", "", "")))
			finished <- err
		}()
		Eventually(func() (int, error) {
			var count int
			err := fixture.Server.Admin.QueryRowContext(ctx,
				"SELECT count(*) FROM pg_catalog.pg_stat_activity WHERE datname=$1 AND "+
					"wait_event_type='Lock'", fixture.Database).Scan(&count)
			return count, err
		}, 3*time.Second, 20*time.Millisecond).Should(BeNumerically(">", 0))
		cancel()
		var canceledErr error
		Eventually(finished, 2*time.Second).Should(Receive(&canceledErr))
		Expect(canceledErr).To(MatchError(context.Canceled))
		Eventually(func() (int, error) {
			var count int
			err := fixture.Server.Admin.QueryRowContext(ctx,
				"SELECT count(*) FROM pg_catalog.pg_stat_activity WHERE datname=$1 AND "+
					"application_name <> 'epco-tests'", fixture.Database).Scan(&count)
			return count, err
		}, 2*time.Second, 20*time.Millisecond).Should(Equal(0))
		deadline := fixture.session(ctx, false, 100*time.Millisecond)
		started := time.Now()
		_, err = deadline.Apply(ctx, fixture.desired(extensionSpec(pkg.Name, "omega", "", "")))
		Expect(err).To(HaveOccurred())
		Expect(time.Since(started)).To(BeNumerically("<", 2*time.Second))
		Expect(locker.Rollback()).To(Succeed())
		Expect(extensionCatalog(ctx, fixture.Admin, pkg.Name)).To(Equal(before))
		Expect(fixture.memberValue(ctx, pkg, "public")).To(Equal(1))
		databaseApply(ctx, fixture.session(ctx, false, 5*time.Second), fixture.desired(extensionSpec(pkg.Name,
			"omega", "", "")))
	}, NodeTimeout(30*time.Second))

	It("keeps extension work pending with admission disabled and repairs it only on explicit true",
		func(ctx SpecContext) {
			pkg := fixture.packageFiles(ctx, "", nil)
			_, err := fixture.Server.Admin.ExecContext(ctx, "ALTER DATABASE "+quoteIdentifier(fixture.Database)+
				" ALLOW_CONNECTIONS false")
			Expect(err).NotTo(HaveOccurred())
			session := fixture.session(ctx, false, 5*time.Second)
			desired := fixture.desired(extensionSpec(pkg.Name, "", "", ""))
			desired.Spec.AllowConnections = new(false)
			_, err = session.Apply(ctx, desired)
			Expect(failureReason(err)).To(Equal(pgdatabase.DependenciesNotReady))
			state, err := session.Inspect(ctx, fixture.Database)
			Expect(err).NotTo(HaveOccurred())
			Expect(state.AllowConnections).To(BeFalse())
			Expect(extensionCatalog(ctx, fixture.Admin, pkg.Name).Exists).To(BeFalse())
			desired.Spec.AllowConnections = new(true)
			databaseApply(ctx, session, desired)
			state, err = session.Inspect(ctx, fixture.Database)
			Expect(err).NotTo(HaveOccurred())
			Expect(state.AllowConnections).To(BeTrue())
			Expect(extensionCatalog(ctx, fixture.Admin, pkg.Name).Exists).To(BeTrue())
		}, NodeTimeout(30*time.Second))

	It("honors server no_relocate constraints even when the installed prerequisite is relocatable",
		func(ctx SpecContext) {
			var version int
			Expect(fixture.Admin.QueryRowContext(ctx, "SHOW server_version_num").Scan(&version)).To(Succeed())
			if version < 180000 {
				Skip("no_relocate control parameter is introduced in PostgreSQL 18")
			}
			prerequisite := fixture.packageFiles(ctx, "", nil)
			dependent := fixture.packageFiles(ctx, "requires = '"+prerequisite.Name+"'\nno_relocate = '"+
				prerequisite.Name+"'\n", nil)
			fixture.create(ctx, prerequisite, "omega")
			fixture.create(ctx, dependent, "omega")
			fixture.sql(ctx, "CREATE SCHEMA destination")
			before := extensionCatalog(ctx, fixture.Admin, prerequisite.Name)
			Expect(before.Relocatable).To(BeTrue())
			_, err := fixture.Admin.ExecContext(ctx, "ALTER EXTENSION "+quoteIdentifier(prerequisite.Name)+
				" SET SCHEMA destination")
			Expect(err).To(HaveOccurred(), "native PostgreSQL independently proves the constraint")
			_, err = fixture.session(ctx, false, 5*time.Second).Apply(ctx,
				fixture.desired(extensionSpec(prerequisite.Name, "", "destination", "")))
			Expect(err).To(HaveOccurred())
			Expect(extensionCatalog(ctx, fixture.Admin, prerequisite.Name)).To(Equal(before))
			Expect(fixture.memberValue(ctx, prerequisite, "public")).To(Equal(3))
		}, NodeTimeout(30*time.Second))

	It("reports declared prerequisite cycles deterministically without installing either extension",
		func(ctx SpecContext) {
			first, err := randomName("epco_cycle_")
			Expect(err).NotTo(HaveOccurred())
			second, err := randomName("epco_cycle_")
			Expect(err).NotTo(HaveOccurred())
			fixture.namedPackageFiles(ctx, first, "requires = '"+second+"'\n", nil)
			fixture.namedPackageFiles(ctx, second, "requires = '"+first+"'\n", nil)
			desired := fixture.desired(extensionSpec(first, "", "", ""), extensionSpec(second, "", "", ""))
			session := fixture.session(ctx, false, 5*time.Second)
			started := time.Now()
			_, err = session.Apply(ctx, desired)
			Expect(failureReason(err)).To(Equal(pgdatabase.InvalidConfiguration))
			Expect(time.Since(started)).To(BeNumerically("<", 2*time.Second))
			_, retryErr := session.Apply(ctx, desired)
			Expect(failureReason(retryErr)).To(Equal(failureReason(err)))
			Expect(extensionCatalog(ctx, fixture.Admin, first).Exists).To(BeFalse())
			Expect(extensionCatalog(ctx, fixture.Admin, second).Exists).To(BeFalse())
		}, NodeTimeout(30*time.Second))

	It("rejects protected explicit and control-fixed schemas without ordinary extension mutation",
		func(ctx SpecContext) {
			pkg := fixture.packageFiles(ctx, "", nil)
			protected := fixture.packageFiles(ctx, "relocatable = false\nschema = 'pg_catalog'\n", nil)
			session := fixture.session(ctx, false, 5*time.Second)
			_, err := session.Apply(ctx, fixture.desired(extensionSpec(pkg.Name, "", "pg_catalog", "")))
			Expect(failureReason(err)).To(Equal(pgdatabase.ProtectedSchema))
			_, err = session.Apply(ctx, fixture.desired(extensionSpec(protected.Name, "", "", "")))
			Expect(failureReason(err)).To(Equal(pgdatabase.ProtectedSchema))
			Expect(extensionCatalog(ctx, fixture.Admin, pkg.Name).Exists).To(BeFalse())
			Expect(extensionCatalog(ctx, fixture.Admin, protected.Name).Exists).To(BeFalse())
		}, NodeTimeout(30*time.Second))

	It("rejects an omitted extension schema when the actual target default is protected", func(ctx SpecContext) {
		fixture.sql(ctx, "ALTER DATABASE "+quoteIdentifier(fixture.Database)+" SET search_path = pg_catalog")
		databaseApplySession := fixture.session(ctx, false, 5*time.Second)
		_, err := databaseApplySession.Apply(ctx, fixture.desired(extensionSpec("hstore", "", "", "")))
		Expect(failureReason(err)).To(Equal(pgdatabase.ProtectedSchema))
		Expect(extensionCatalog(ctx, fixture.Admin, "hstore").Exists).To(BeFalse())
	}, NodeTimeout(30*time.Second))

	It("uses the ordinary permitted target default schema when no schema is requested", func(ctx SpecContext) {
		pkg := fixture.packageFiles(ctx, "", nil)
		fixture.sql(ctx, "CREATE SCHEMA application_default; ALTER DATABASE "+quoteIdentifier(fixture.Database)+
			" SET search_path = application_default")
		databaseApply(ctx, fixture.session(ctx, false, 5*time.Second), fixture.desired(extensionSpec(pkg.Name, "", "", "")))
		Expect(extensionCatalog(ctx, fixture.Admin, pkg.Name).Schema).To(Equal("application_default"))
		Expect(fixture.memberValue(ctx, pkg, "application_default")).To(Equal(3))
	}, NodeTimeout(30*time.Second))

	It("does not treat the target version control-fixed schema as an existing extension "+
		"relocation instruction", func(ctx SpecContext) {
		pkg := fixture.packageFiles(ctx, "",
			map[string]string{"--omega.control": "relocatable = false\nschema = 'installation_only'\n"})
		fixture.create(ctx, pkg, "seed")
		before := extensionCatalog(ctx, fixture.Admin, pkg.Name)
		databaseApply(ctx, fixture.session(ctx, false, 5*time.Second), fixture.desired(extensionSpec(pkg.Name,
			"omega", "", "")))
		after := extensionCatalog(ctx, fixture.Admin, pkg.Name)
		Expect(after.OID).To(Equal(before.OID))
		Expect(after.Version).To(Equal("omega"))
		Expect(after.Schema).To(Equal("public"))
		Expect(after.Relocatable).To(BeFalse())
		Expect(fixture.memberValue(ctx, pkg, "public")).To(Equal(3))
	}, NodeTimeout(30*time.Second))

	It("updates to an opaque quoted version label without truncation or semver assumptions", func(ctx SpecContext) {
		version := "update's " + strings.Repeat("label", 16)
		pkg := fixture.packageFiles(ctx, "", map[string]string{"--seed--" + version +
			".sql": "UPDATE @members@ SET value = 73;"})
		fixture.create(ctx, pkg, "seed")
		before := extensionCatalog(ctx, fixture.Admin, pkg.Name)
		databaseApply(ctx, fixture.session(ctx, false, 5*time.Second), fixture.desired(extensionSpec(pkg.Name,
			version, "", "")))
		after := extensionCatalog(ctx, fixture.Admin, pkg.Name)
		Expect(after.Version).To(Equal(version))
		Expect(after.OID).To(Equal(before.OID))
		Expect(fixture.memberValue(ctx, pkg, "public")).To(Equal(73))
	}, NodeTimeout(30*time.Second))

	It("rejects incompatible prerequisite presence and absence without dropping existing prerequisite",
		func(ctx SpecContext) {
			prerequisite := fixture.packageFiles(ctx, "", nil)
			dependent := fixture.packageFiles(ctx, "requires = '"+prerequisite.Name+"'\n", nil)
			fixture.create(ctx, prerequisite, "omega")
			before := extensionCatalog(ctx, fixture.Admin, prerequisite.Name)
			desired := fixture.desired(extensionSpec(prerequisite.Name, "", "", epco.EnsureAbsent),
				extensionSpec(dependent.Name, "", "", ""))
			_, err := fixture.session(ctx, false, 5*time.Second).Apply(ctx, desired)
			Expect(failureReason(err)).To(Equal(pgdatabase.InvalidConfiguration))
			Expect(extensionCatalog(ctx, fixture.Admin, prerequisite.Name)).To(Equal(before))
			Expect(extensionCatalog(ctx, fixture.Admin, dependent.Name).Exists).To(BeFalse())
		}, NodeTimeout(30*time.Second))

	It("refuses mutation when the target connection is read only", func(ctx SpecContext) {
		fixture.sql(ctx, "ALTER DATABASE "+quoteIdentifier(fixture.Database)+" SET default_transaction_read_only = on")
		_, err := fixture.session(ctx, false, 5*time.Second).Apply(ctx, fixture.desired(extensionSpec("hstore",
			"", "", "")))
		Expect(failureReason(err)).To(Equal(pgdatabase.NotWritable))
		Expect(extensionCatalog(ctx, fixture.Admin, "hstore").Exists).To(BeFalse())
	}, NodeTimeout(30*time.Second))

	DescribeTable("protects external function and composite type dependencies with RESTRICT",
		func(ctx SpecContext, kind string) {
			pkg := fixture.packageFiles(ctx, "", nil)
			fixture.create(ctx, pkg, "omega")
			objectType := "public." + quoteIdentifier(pkg.Name+"_type")
			if kind == "function" {
				fixture.sql(ctx, "CREATE FUNCTION public.application_function("+objectType+
					") RETURNS text LANGUAGE sql AS 'SELECT $1::text'")
			} else {
				fixture.sql(ctx, "CREATE TYPE public.application_type AS (payload "+objectType+")")
			}
			before := extensionCatalog(ctx, fixture.Admin, pkg.Name)
			desired := fixture.desired(extensionSpec(pkg.Name, "", "", epco.EnsureAbsent))
			session := fixture.session(ctx, false, 5*time.Second)
			_, err := session.Apply(ctx, desired)
			Expect(failureReason(err)).To(Equal(pgdatabase.DeletionBlocked))
			Expect(extensionCatalog(ctx, fixture.Admin, pkg.Name)).To(Equal(before))
			var value string
			if kind == "function" {
				Expect(fixture.Admin.QueryRowContext(ctx, "SELECT public.application_function('preserved'::"+objectType+
					")").Scan(&value)).To(Succeed())
				fixture.sql(ctx, "DROP FUNCTION public.application_function("+objectType+")")
			} else {
				Expect(fixture.Admin.QueryRowContext(ctx,
					"SELECT (ROW('preserved')::public.application_type).payload::text").Scan(&value)).To(Succeed())
				fixture.sql(ctx, "DROP TYPE public.application_type")
			}
			Expect(value).To(Equal("preserved"))
			databaseApply(ctx, session, desired)
			Expect(extensionCatalog(ctx, fixture.Admin, pkg.Name).Exists).To(BeFalse())
		}, Entry("external function", "function", NodeTimeout(30*time.Second)), Entry(
			"external type", "type", NodeTimeout(30*time.Second)))

	It("classifies a package without a default version as unsupported while allowing explicit installation",
		func(ctx SpecContext) {
			pkg := fixture.packageFiles(ctx, "",
				map[string]string{".control": "relocatable = true\nsuperuser = true\ntrusted = true\n"})
			session := fixture.session(ctx, false, 5*time.Second)
			_, err := session.Apply(ctx, fixture.desired(extensionSpec(pkg.Name, "", "", "")))
			Expect(failureReason(err)).To(Equal(pgdatabase.UnsupportedFeature))
			Expect(extensionCatalog(ctx, fixture.Admin, pkg.Name).Exists).To(BeFalse())
			databaseApply(ctx, session, fixture.desired(extensionSpec(pkg.Name, "omega", "", "")))
			Expect(extensionCatalog(ctx, fixture.Admin, pkg.Name).Version).To(Equal("omega"))
		}, NodeTimeout(30*time.Second))
})
