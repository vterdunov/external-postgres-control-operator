//go:build integration

package integration

import (
	"context"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	epco "github.com/vterdunov/external-postgres-control-operator/api/v1alpha1"
)

const extensionOperatorSchema = "operator_tenant"

// The tenant owns the operators and their invoker functions. A side effect
// records the effective SQL principal without falsifying equality results.
func extensionOperatorShadow(ctx context.Context, f *extensionSQLFixture, kinds ...string) {
	f.sql(ctx, "CREATE SCHEMA "+extensionOperatorSchema+" AUTHORIZATION "+quoteIdentifier(f.Role))
	_, err := f.Limited.ExecContext(ctx, "CREATE TABLE "+extensionOperatorSchema+
		".probe (executor pg_catalog.text, kind pg_catalog.text)")
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	for _, kind := range kinds {
		native := map[string]string{"name": "nameeq", "oid": "oideq", "text": "texteq"}[kind]
		ExpectWithOffset(1, native).NotTo(BeEmpty())
		_, err = f.Limited.ExecContext(ctx, fmt.Sprintf(`
			CREATE FUNCTION %[1]s.eq_%[2]s(pg_catalog.%[2]s,pg_catalog.%[2]s)
			RETURNS pg_catalog.bool LANGUAGE plpgsql VOLATILE AS $probe$
			BEGIN
				INSERT INTO %[1]s.probe VALUES(current_user,'%[2]s');
				RETURN pg_catalog.%[3]s($1,$2);
			END $probe$;
			CREATE OPERATOR %[1]s.= (FUNCTION=%[1]s.eq_%[2]s,
				LEFTARG=pg_catalog.%[2]s,RIGHTARG=pg_catalog.%[2]s)`, extensionOperatorSchema, kind, native))
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
	}
	// This is the ordinary target default for newly opened production sessions.
	f.sql(ctx, "ALTER DATABASE "+quoteIdentifier(f.Database)+" SET search_path TO "+
		extensionOperatorSchema+",pg_catalog")
	// Keep the independent fixture observer out of the tenant search path.
	f.sql(ctx, "SET search_path TO pg_catalog")
}

func extensionOperatorProbe(ctx context.Context, f *extensionSQLFixture) int {
	var count int
	ExpectWithOffset(1, f.Admin.QueryRowContext(ctx, "SELECT count(*) FROM "+extensionOperatorSchema+
		".probe").Scan(&count)).To(Succeed())
	return count
}

var _ = Describe("Database extension catalog operator resolution", func() {
	var fixture *extensionSQLFixture
	BeforeEach(func(ctx SpecContext) { fixture = newExtensionSQLFixture(ctx) }, NodeTimeout(30*time.Second))

	It("provides a native oracle for tenant operators executing as the caller", func(ctx SpecContext) {
		extensionOperatorShadow(ctx, fixture, "name", "oid", "text")
		fixture.sql(ctx, "SET search_path TO "+extensionOperatorSchema+",pg_catalog")
		for _, expression := range []string{"'probe'::pg_catalog.name", "1::pg_catalog.oid", "'probe'::pg_catalog.text"} {
			var equal bool
			Expect(fixture.Admin.QueryRowContext(ctx, "SELECT "+expression+"="+expression).Scan(&equal)).To(Succeed())
			Expect(equal).To(BeTrue())
		}
		var executors, kinds int
		Expect(fixture.Admin.QueryRowContext(ctx, "SELECT count(DISTINCT kind),count(*) FILTER (WHERE executor "+
			"OPERATOR(pg_catalog.=) $1) FROM "+extensionOperatorSchema+".probe", roleSQLAdmin).
			Scan(&kinds, &executors)).To(Succeed())
		Expect(kinds).To(Equal(3))
		Expect(executors).To(Equal(3), "tenant invoker code runs as the administrative caller")
		fixture.sql(ctx, "TRUNCATE "+extensionOperatorSchema+".probe")
		for _, expression := range []string{"'probe'::pg_catalog.name", "1::pg_catalog.oid", "'probe'::pg_catalog.text"} {
			var equal bool
			Expect(fixture.Admin.QueryRowContext(ctx, "SELECT "+expression+" OPERATOR(pg_catalog.=) "+expression).
				Scan(&equal)).To(Succeed())
			Expect(equal).To(BeTrue())
		}
		Expect(extensionOperatorProbe(ctx, fixture)).To(BeZero())
	}, NodeTimeout(30*time.Second))

	DescribeTable("observes installed extensions without running tenant operator code",
		func(ctx SpecContext, kind string) {
			pkg := fixture.packageFiles(ctx, "", nil)
			fixture.create(ctx, pkg, "omega")
			extensionOperatorShadow(ctx, fixture, kind)
			desired := fixture.desired(extensionSpec(pkg.Name, "omega", "public", ""))
			desired.Spec.ManagementPolicy = epco.ManagementObserve
			result, err := fixture.session(ctx, false, 5*time.Second).Apply(ctx, desired)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).NotTo(BeNil())
			Expect(result.BaseManaged).To(BeFalse(), "pure observation must not acquire management")
			Expect(extensionOperatorProbe(ctx, fixture)).To(BeZero(),
				"observation must not execute tenant code as administrator")
			Expect(extensionCatalog(ctx, fixture.Admin, pkg.Name).Version).To(Equal("omega"))
		}, Entry("name/name filter", "name", NodeTimeout(30*time.Second)), Entry(
			"oid/oid namespace join", "oid", NodeTimeout(30*time.Second)))

	It("installs using the ordinary default schema without invoking tenant text equality", func(ctx SpecContext) {
		pkg := fixture.packageFiles(ctx, "", nil)
		extensionOperatorShadow(ctx, fixture, "text")
		databaseApply(ctx, fixture.session(ctx, false, 5*time.Second), fixture.desired(extensionSpec(pkg.Name, "", "", "")))
		state := extensionCatalog(ctx, fixture.Admin, pkg.Name)
		Expect(state.Schema).To(Equal(extensionOperatorSchema),
			"catalog hardening must preserve the ordinary creation default")
		Expect(state.Version).To(Equal("omega"))
		Expect(fixture.memberValue(ctx, pkg, extensionOperatorSchema)).To(Equal(3))
		Expect(extensionOperatorProbe(ctx, fixture)).To(BeZero(), "version metadata must use built-in equality")
	}, NodeTimeout(30*time.Second))

	It("updates versions without executing tenant text equality in metadata and update paths", func(ctx SpecContext) {
		pkg := fixture.packageFiles(ctx, "", nil)
		fixture.create(ctx, pkg, "seed")
		extensionOperatorShadow(ctx, fixture, "text")
		databaseApply(ctx, fixture.session(ctx, false, 5*time.Second),
			fixture.desired(extensionSpec(pkg.Name, "omega", "", "")))
		Expect(extensionCatalog(ctx, fixture.Admin, pkg.Name).Version).To(Equal("omega"))
		Expect(fixture.memberValue(ctx, pkg, "public")).To(Equal(3))
		Expect(extensionOperatorProbe(ctx, fixture)).To(BeZero(),
			"update-path source and target comparisons must use built-in equality")
	}, NodeTimeout(30*time.Second))

	It("creates declared schemas and installs extensions without tenant name equality", func(ctx SpecContext) {
		pkg := fixture.packageFiles(ctx, "", nil)
		extensionOperatorShadow(ctx, fixture, "name")
		desired := fixture.desired(extensionSpec(pkg.Name, "", "destination", ""))
		desired.Spec.Schemas = []epco.SchemaSpec{{Name: "destination"}}
		databaseApply(ctx, fixture.session(ctx, false, 5*time.Second), desired)
		Expect(extensionCatalog(ctx, fixture.Admin, pkg.Name).Schema).To(Equal("destination"))
		Expect(fixture.memberValue(ctx, pkg, "destination")).To(Equal(3))
		Expect(extensionOperatorProbe(ctx, fixture)).To(BeZero(),
			"schema name and owner-role lookups must not invoke tenant operators")
	}, NodeTimeout(30*time.Second))

	It("moves installed extensions using safe inherited schema lookup", func(ctx SpecContext) {
		pkg := fixture.packageFiles(ctx, "", nil)
		fixture.create(ctx, pkg, "omega")
		extensionOperatorShadow(ctx, fixture, "name")
		databaseApply(ctx, fixture.session(ctx, false, 5*time.Second),
			fixture.desired(extensionSpec(pkg.Name, "", extensionOperatorSchema, "")))
		Expect(extensionCatalog(ctx, fixture.Admin, pkg.Name).Schema).To(Equal(extensionOperatorSchema))
		Expect(extensionOperatorProbe(ctx, fixture)).To(BeZero(), "relocation schema inspection must use built-in equality")
	}, NodeTimeout(30*time.Second))
})
