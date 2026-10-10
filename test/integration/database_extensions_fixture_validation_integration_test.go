//go:build integration

package integration

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Extension script fixture native PostgreSQL evidence", func() {
	var fixture *extensionSQLFixture
	BeforeEach(func(ctx SpecContext) { fixture = newExtensionSQLFixture(ctx) }, NodeTimeout(30*time.Second))

	It("provides both relocation capability transitions and a genuine multi-step update path", func(ctx SpecContext) {
		pkg := fixture.packageFiles(ctx, "", nil)
		fixture.create(ctx, pkg, "seed")
		fixture.sql(ctx, "ALTER EXTENSION "+quoteIdentifier(pkg.Name)+" UPDATE TO 'omega'")
		Expect(fixture.memberValue(ctx, pkg, "public")).To(Equal(3))
		fixture.sql(ctx, "CREATE SCHEMA destination; ALTER EXTENSION "+quoteIdentifier(pkg.Name)+
			" SET SCHEMA destination; ALTER EXTENSION "+quoteIdentifier(pkg.Name)+" UPDATE TO 'locked'")
		Expect(extensionCatalog(ctx, fixture.Admin, pkg.Name).Relocatable).To(BeFalse())
		Expect(fixture.memberValue(ctx, pkg, "destination")).To(Equal(4))
		_, err := fixture.Admin.ExecContext(ctx, "ALTER EXTENSION "+quoteIdentifier(pkg.Name)+" SET SCHEMA public")
		Expect(SQLState(err)).To(Equal("0A000"))
		fixture.sql(ctx, "ALTER EXTENSION "+quoteIdentifier(pkg.Name)+" UPDATE TO 'omega'; ALTER EXTENSION "+
			quoteIdentifier(pkg.Name)+" SET SCHEMA public")
		Expect(extensionCatalog(ctx, fixture.Admin, pkg.Name).Relocatable).To(BeTrue())
		Expect(fixture.memberValue(ctx, pkg, "public")).To(Equal(3))
	}, NodeTimeout(30*time.Second))

	It("executes broken-script writes before a real division-by-zero error and rolls them back", func(ctx SpecContext) {
		pkg := fixture.packageFiles(ctx, "", nil)
		fixture.create(ctx, pkg, "omega")
		before := extensionCatalog(ctx, fixture.Admin, pkg.Name)
		_, err := fixture.Admin.ExecContext(ctx, "ALTER EXTENSION "+quoteIdentifier(pkg.Name)+" UPDATE TO 'broken'")
		Expect(SQLState(err)).To(Equal("22012"), "fixture must reach the failing script rather than metadata preflight")
		Expect(extensionCatalog(ctx, fixture.Admin, pkg.Name)).To(Equal(before))
		Expect(fixture.memberValue(ctx, pkg, "public")).To(Equal(3))
	}, NodeTimeout(30*time.Second))
})
