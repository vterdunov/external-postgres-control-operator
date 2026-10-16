//go:build integration

package integration

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	epco "github.com/vterdunov/external-postgres-control-operator/api/v1alpha1"
	pgdatabase "github.com/vterdunov/external-postgres-control-operator/internal/postgres"
)

const expectedAdministrativeSearchPath = "pg_catalog, pg_temp"

type administrativeStartup struct {
	database, searchPath, statementTimeout string
	pid                                    uint32
}

type administrativeQuery struct {
	connection *pgx.Conn
	sql        string
	arguments  int
}

type administrativeQueryTracer struct{ queries []administrativeQuery }

func (t *administrativeQueryTracer) TraceQueryStart(ctx context.Context, connection *pgx.Conn,
	data pgx.TraceQueryStartData) context.Context {
	t.queries = append(t.queries, administrativeQuery{connection, data.SQL, len(data.Args)})
	return ctx
}

func (*administrativeQueryTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func recordAdministrativeStartup(config *pgdatabase.Config, observed *[]administrativeStartup) {
	config.Connection.AfterConnect = func(ctx context.Context, connection *pgconn.PgConn) error {
		results, err := connection.Exec(ctx, `SELECT pg_catalog.current_database(),
			pg_catalog.current_setting('search_path'),pg_catalog.current_setting('statement_timeout')`).ReadAll()
		if err != nil {
			return err
		}
		if len(results) != 1 || len(results[0].Rows) != 1 || len(results[0].Rows[0]) != 3 {
			return errors.New("administrative startup query returned an unexpected result shape")
		}
		row := results[0].Rows[0]
		*observed = append(*observed, administrativeStartup{string(row[0]), string(row[1]), string(row[2]), connection.PID()})
		return nil
	}
}

func expectSafeAdministrativeStartup(observed []administrativeStartup) {
	ExpectWithOffset(1, observed).NotTo(BeEmpty(), "the production connection must reach PostgreSQL")
	for _, startup := range observed {
		ExpectWithOffset(1, startup.searchPath).To(Equal(expectedAdministrativeSearchPath),
			"administrative connection to %s must override the target default at startup", startup.database)
		ExpectWithOffset(1, startup.statementTimeout).To(Equal("2s"),
			"pinning the search path must preserve the configured statement timeout")
	}
}

var _ = Describe("Administrative PostgreSQL search path", func() {
	var fixture *extensionSQLFixture
	BeforeEach(func(ctx SpecContext) { fixture = newExtensionSQLFixture(ctx) }, NodeTimeout(30*time.Second))

	DescribeTable("pins the safe startup path before administrative SQL",
		func(ctx SpecContext, factory string) {
			fixture.sql(ctx, "CREATE SCHEMA application_default; ALTER DATABASE "+quoteIdentifier(fixture.Database)+
				" SET search_path TO application_default,pg_catalog")
			config := productionConfig(fixture.Fixture, roleTLSMode, false)
			var observed []administrativeStartup
			recordAdministrativeStartup(config, &observed)
			switch factory {
			case "checker":
				Expect(pgdatabase.NewChecker().Check(ctx, config)).To(Succeed())
			case "role":
				session, err := pgdatabase.NewRoleFactory().Open(ctx, config)
				Expect(err).NotTo(HaveOccurred())
				Expect(session.Close(ctx)).To(Succeed())
			case "database":
				session, err := pgdatabase.NewDatabaseFactory().Open(ctx, config, pgdatabase.DatabaseSessionOptions{})
				Expect(err).NotTo(HaveOccurred())
				Expect(session.Close(ctx)).To(Succeed())
			}
			expectSafeAdministrativeStartup(observed)
		},
		Entry("connection checker", "checker", NodeTimeout(30*time.Second)),
		Entry("role session", "role", NodeTimeout(30*time.Second)),
		Entry("database session", "database", NodeTimeout(30*time.Second)))

	It("keeps target connections safe while honoring the ordinary role-in-database extension schema",
		func(ctx SpecContext) {
			pkg := fixture.packageFiles(ctx, "", nil)
			fixture.sql(ctx, "CREATE SCHEMA database_default; CREATE SCHEMA role_default; ALTER DATABASE "+
				quoteIdentifier(fixture.Database)+" SET search_path TO database_default,pg_catalog; ALTER ROLE "+
				quoteIdentifier(roleSQLAdmin)+" IN DATABASE "+quoteIdentifier(fixture.Database)+
				" SET search_path TO role_default,pg_catalog")
			// The ordinary observer establishes PostgreSQL's effective default without
			// changing the production configuration or the fixture's existing sessions.
			ordinary, err := postgres.Open(fixture.Database, roleSQLAdmin, postgres.password)
			Expect(err).NotTo(HaveOccurred())
			var defaultSchema string
			err = ordinary.QueryRowContext(ctx, "SELECT pg_catalog.current_schema()").Scan(&defaultSchema)
			Expect(ordinary.Close()).To(Succeed())
			Expect(err).NotTo(HaveOccurred())
			Expect(defaultSchema).To(Equal("role_default"))

			config := productionConfig(fixture.Fixture, roleTLSMode, false)
			config.Connection.Database = roleSQLAdmin
			config.Connection.User, config.Connection.Password = roleSQLAdmin, postgres.password
			var observed []administrativeStartup
			recordAdministrativeStartup(config, &observed)
			tracer := &administrativeQueryTracer{}
			config.Connection.Tracer = tracer
			session, err := pgdatabase.NewDatabaseFactory().Open(ctx, config, pgdatabase.DatabaseSessionOptions{})
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func(ctx SpecContext) { Expect(session.Close(ctx)).To(Succeed()) })
			desired := fixture.desired(extensionSpec(pkg.Name, "", "", ""))
			desired.Spec.Schemas = []epco.SchemaSpec{{Name: "declared_schema"}}
			databaseApply(ctx, session, desired)
			Expect(extensionCatalog(ctx, fixture.Admin, pkg.Name).Schema).To(Equal(defaultSchema))
			Expect(fixture.memberValue(ctx, pkg, defaultSchema)).To(Equal(3))
			var targetConnections, ordinaryConnections int
			for _, startup := range observed {
				if startup.searchPath != expectedAdministrativeSearchPath {
					ordinaryConnections++
					Expect(startup.database).To(Equal(fixture.Database))
					Expect(startup.searchPath).To(Equal("role_default, pg_catalog"))
					Expect(startup.statementTimeout).To(Equal("2s"))
					var lookupQueries []administrativeQuery
					for _, query := range tracer.queries {
						if query.connection.PgConn().PID() == startup.pid {
							lookupQueries = append(lookupQueries, query)
						}
					}
					Expect(lookupQueries).To(HaveLen(1), "ordinary-default discovery must run one production query")
					Expect(lookupQueries[0].sql).To(Equal("SELECT COALESCE(pg_catalog.current_schema()::pg_catalog.text,'')"))
					Expect(lookupQueries[0].arguments).To(BeZero())
					Expect(lookupQueries[0].connection.IsClosed()).To(BeTrue(),
						"ordinary-default discovery must close its connection before extension reconciliation returns")
					continue
				}
				expectSafeAdministrativeStartup([]administrativeStartup{startup})
				if startup.database == fixture.Database {
					targetConnections++
				}
			}
			Expect(ordinaryConnections).To(Equal(1), "only the isolated default-schema discovery may inherit ordinary settings")
			Expect(targetConnections).To(BeNumerically(">=", 2),
				"schema and extension reconciliation must both open target connections")
		}, NodeTimeout(30*time.Second))
})
