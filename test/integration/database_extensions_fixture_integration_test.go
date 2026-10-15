//go:build integration

package integration

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	epco "github.com/vterdunov/external-postgres-control-operator/api/v1alpha1"
	pgdatabase "github.com/vterdunov/external-postgres-control-operator/internal/postgres"
)

//go:embed testdata/extensions/*
var extensionScripts embed.FS

type extensionSQLFixture struct{ *Fixture }

type extensionPackage struct {
	Name  string
	Files []string
}

func (f *extensionSQLFixture) packageFiles(ctx context.Context, control string,
	extra map[string]string) extensionPackage {
	name, err := randomName("epco_ext_")
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	return f.namedPackageFiles(ctx, name, control, extra)
}

func (f *extensionSQLFixture) namedPackageFiles(ctx context.Context, name, control string,
	extra map[string]string) extensionPackage {
	dir := GinkgoT().TempDir()
	entries, err := fs.ReadDir(extensionScripts, "testdata/extensions")
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	files := make(map[string]string, len(entries)+len(extra))
	for _, entry := range entries {
		content, err := extensionScripts.ReadFile("testdata/extensions/" + entry.Name())
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
		files[strings.TrimPrefix(entry.Name(), "extension")] = string(content)
	}
	files[".control"] += control
	maps.Copy(files, extra)
	pkg := extensionPackage{Name: name}
	for suffix, content := range files {
		content = strings.NewReplacer("@members@", quoteIdentifier(name+"_members"), "@type@",
			quoteIdentifier(name+"_type"), "@sequence@", quoteIdentifier(name+"_sequence")).Replace(content)
		filename := name + suffix
		ExpectWithOffset(1, os.WriteFile(filepath.Join(dir, filename), []byte(content), 0644)).To(Succeed())
		pkg.Files = append(pkg.Files, filename)
	}
	stage, err := randomName("epco-extensions-")
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	_, err = f.Server.docker(ctx, "cp", dir, f.Server.Container+":/tmp/"+stage)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	_, err = f.Server.docker(ctx, "exec", f.Server.Container, "sh", "-ec",
		`cp "$1"/* "$(pg_config --sharedir)/extension/"`, "fixture", "/tmp/"+stage)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	return pkg
}

func (f *extensionSQLFixture) removePackage(ctx context.Context, pkg extensionPackage) {
	args := make([]string, 0, 6+len(pkg.Files))
	args = append(args, "exec", f.Server.Container, "sh", "-ec",
		`directory="$(pg_config --sharedir)/extension"; for filename do rm -- "$directory/$filename"; done`, "fixture")
	args = append(args, pkg.Files...)
	_, err := f.Server.docker(ctx, args...)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
}

func (f *extensionSQLFixture) sql(ctx context.Context, query string) {
	_, err := f.Admin.ExecContext(ctx, query)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
}

func (f *extensionSQLFixture) create(ctx context.Context, pkg extensionPackage, version string) {
	f.sql(ctx, "CREATE EXTENSION "+quoteIdentifier(pkg.Name)+" VERSION "+quoteLiteral(version)+" SCHEMA "+
		quoteIdentifier("public"))
}

func (f *extensionSQLFixture) memberValue(ctx context.Context, pkg extensionPackage, schema string) int {
	var value int
	ExpectWithOffset(1, f.Admin.QueryRowContext(ctx, "SELECT value FROM "+quoteIdentifier(schema)+"."+
		quoteIdentifier(pkg.Name+"_members")).Scan(&value)).To(Succeed())
	return value
}

func newExtensionSQLFixture(ctx context.Context) *extensionSQLFixture {
	base, err := postgres.NewFixture(ctx)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	f := &extensionSQLFixture{base}
	DeferCleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		Expect(f.Close(cleanup)).To(Succeed())
	})
	return f
}

func (f *extensionSQLFixture) session(ctx context.Context, restricted bool,
	timeout time.Duration) pgdatabase.DatabaseSession {
	config := productionConfig(f.Fixture, roleTLSMode, false)
	config.Connection.Database = roleSQLAdmin
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

func (f *extensionSQLFixture) desired(extensions ...epco.ExtensionSpec) pgdatabase.DatabaseDesired {
	return pgdatabase.DatabaseDesired{Name: f.Database, Spec: epco.DatabaseSpec{
		Owner: epco.SQLName(roleSQLAdmin), Extensions: extensions,
	}}
}

func extensionSpec(name, version, schema string, ensure epco.EnsureOption) epco.ExtensionSpec {
	return epco.ExtensionSpec{Name: epco.SQLName(name), Ensure: ensure, Version: version, Schema: epco.SQLName(schema)}
}

type installedExtension struct {
	Exists                 bool
	Version, Schema, Owner string
	Relocatable            bool
	OID                    uint32
}

func extensionCatalog(ctx context.Context, db *sql.DB, name string) installedExtension {
	var state installedExtension
	err := db.QueryRowContext(ctx, `SELECT e.oid, e.extversion, n.nspname,
		pg_catalog.pg_get_userbyid(e.extowner), e.extrelocatable
		FROM pg_catalog.pg_extension e JOIN pg_catalog.pg_namespace n ON n.oid=e.extnamespace
		WHERE e.extname=$1`, name).Scan(&state.OID, &state.Version, &state.Schema, &state.Owner, &state.Relocatable)
	if errors.Is(err, sql.ErrNoRows) {
		return state
	}
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	state.Exists = true
	return state
}
