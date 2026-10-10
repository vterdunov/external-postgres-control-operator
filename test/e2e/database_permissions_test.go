//go:build e2e

package e2e

import (
	"context"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	epco "github.com/vterdunov/external-postgres-control-operator/api/v1alpha1"
	"github.com/vterdunov/external-postgres-control-operator/test/utils"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	installedACLPassword     = "isolated-acl-fixture-password"
	installedACLDatabaseName = "epco_e2e_acl"
)

func installedACLLogin(ctx context.Context, role, query string, allowed bool) string {
	const script = "IFS= read -r PGPASSWORD; export PGPASSWORD; " +
		"exec psql -X -v ON_ERROR_STOP=1 -v VERBOSITY=sqlstate -h 127.0.0.1 -U \"$1\" -d \"$2\" -Atc \"$3\""
	cmd := kubectlCommand(ctx, "-n", "epco-postgres", "exec", "-i", "deployment/postgres", "--",
		"sh", "-c", script, "sh", role, installedACLDatabaseName, query)
	cmd.Stdin = strings.NewReader(installedACLPassword + "\n")
	output, err := utils.Run(cmd)
	if allowed {
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
	} else {
		ExpectWithOffset(1, err).To(HaveOccurred())
		ExpectWithOffset(1, output).To(ContainSubstring("42501"), "permission refusal must come from PostgreSQL")
	}
	return strings.TrimSpace(output)
}

func installedACLPermission(role string) []epco.Permission {
	return []epco.Permission{{Name: epco.SQLName(role)}}
}

var _ = Describe("R3 Installed Database permissions", func() {
	It("converges grants, defaults and named-table removal without dropping data", func(ctx SpecContext) {
		const (
			name    = installedACLDatabaseName
			owner   = "epco_e2e_acl_owner"
			admin   = "epco_e2e_acl_admin"
			service = "epco_e2e_acl_service"
			reader  = "epco_e2e_acl_reader"
			llm     = "epco_e2e_acl_llm"
			orders  = "SELECT value FROM app.orders"
		)
		cluster := installedRoleDescriptor(ctx, "acl-restricted-connection", nil)
		installedDatabaseOwner(ctx, cluster, owner)
		installedDatabaseSQL(ctx, installedDatabasePostgresName,
			"CREATE ROLE "+admin+" LOGIN NOSUPERUSER CREATEDB NOCREATEROLE INHERIT PASSWORD '"+installedACLPassword+"'; "+
				"GRANT "+owner+" TO "+admin+"; "+
				"CREATE ROLE "+service+" LOGIN PASSWORD '"+installedACLPassword+"'; "+
				"CREATE ROLE "+reader+" LOGIN PASSWORD '"+installedACLPassword+"'; "+
				"CREATE ROLE "+llm+" LOGIN PASSWORD '"+installedACLPassword+"'")
		DeferCleanup(func() {
			cleanup, cancel := boundedContext()
			defer cancel()
			installedDatabaseSQL(cleanup, installedDatabasePostgresName,
				"DROP ROLE "+admin+", "+service+", "+reader+", "+llm)
		})
		installedDatabaseFixtureCleanup(name)
		credentials := &corev1.Secret{}
		Expect(apiClient.Get(ctx, client.ObjectKey{Namespace: operatorNamespace,
			Name: cluster.Spec.CredentialsSecret.Name}, credentials)).To(Succeed())
		credentials.Data[corev1.BasicAuthUsernameKey] = []byte(admin)
		credentials.Data[corev1.BasicAuthPasswordKey] = []byte(installedACLPassword)
		Expect(apiClient.Update(ctx, credentials)).To(Succeed())
		Expect(apiClient.Get(ctx, client.ObjectKeyFromObject(cluster), cluster)).To(Succeed())
		clusterBefore := cluster.DeepCopy()
		cluster.Spec.ReconcileInterval = "2s"
		Expect(apiClient.Patch(ctx, cluster, client.MergeFrom(clusterBefore))).To(Succeed())
		installedClusterStatus(ctx, client.ObjectKeyFromObject(cluster), metav1.ConditionTrue, "Ready")
		database := &epco.Database{Name: "acl-database", Namespace: operatorNamespace,
			Spec: epco.DatabaseSpec{Name: name, Owner: owner,
				Cluster: epco.ClusterReference{Name: cluster.Name},
				Schemas: []epco.SchemaSpec{{Name: installedDatabaseApplicationSchema}}}}
		Expect(apiClient.Create(ctx, database)).To(Succeed())
		DeferCleanup(func() { deleteObject(database) })
		database = installedDatabase(ctx, database, metav1.ConditionTrue, "Ready")
		installedDatabaseSQL(ctx, name, "SET ROLE "+owner+
			"; CREATE TABLE app.orders(value int); INSERT INTO app.orders VALUES(42); "+
			"CREATE TABLE app.invoices(value int); INSERT INTO app.invoices VALUES(7); "+
			"CREATE TABLE app.hidden(value int); INSERT INTO app.hidden VALUES(9); "+
			"CREATE SEQUENCE app.order_ids")
		databaseBefore := database.DeepCopy()
		database.Spec.Permissions = &epco.DatabasePermissions{Connect: []epco.Permission{
			{Name: "public", Ensure: epco.EnsureAbsent}, {Name: service}, {Name: reader}, {Name: llm},
		}}
		database.Spec.Schemas = []epco.SchemaSpec{{Name: installedDatabaseApplicationSchema,
			Permissions: &epco.SchemaPermissions{
				Usage: []epco.Permission{{Name: service}, {Name: reader}, {Name: llm}},
				Tables: &epco.TablePermissions{
					Select: []epco.Permission{{Name: service}, {Name: reader}},
					Insert: installedACLPermission(service), Update: installedACLPermission(service),
					Delete: installedACLPermission(service),
				},
				Sequences: &epco.SequencePermissions{Usage: installedACLPermission(service)},
			},
			DefaultPrivileges: []epco.DefaultPrivileges{{ForRole: owner,
				Tables: &epco.TablePermissions{Select: []epco.Permission{{Name: service}, {Name: reader}},
					Insert: installedACLPermission(service), Update: installedACLPermission(service),
					Delete: installedACLPermission(service)},
				Sequences: &epco.SequencePermissions{Usage: installedACLPermission(service)},
			}},
			TablePermissions: []epco.NamedTablePermissions{
				{Name: "orders", Permissions: epco.TablePermissions{Select: installedACLPermission(llm)}},
				{Name: "invoices", Permissions: epco.TablePermissions{Select: installedACLPermission(llm)}},
			},
		}}
		Expect(apiClient.Patch(ctx, database, client.MergeFrom(databaseBefore))).To(Succeed())
		database = installedDatabase(ctx, database, metav1.ConditionTrue, "Ready")
		Expect(installedACLLogin(ctx, admin, "SELECT current_user", true)).To(Equal(admin))
		Expect(installedDatabaseSQL(ctx, installedDatabasePostgresName,
			"SELECT count(*) FROM pg_database d CROSS JOIN LATERAL aclexplode(COALESCE(d.datacl,acldefault('d',d.datdba))) a "+
				"WHERE d.datname='"+name+"' AND a.grantee=0 AND a.privilege_type='CONNECT'")).To(Equal("0"))
		Expect(installedACLLogin(ctx, reader, orders, true)).To(Equal("42"))
		installedACLLogin(ctx, service,
			"BEGIN; INSERT INTO app.orders VALUES(43); UPDATE app.orders SET value=44 WHERE value=43; "+
				"DELETE FROM app.orders WHERE value=44; ROLLBACK", true)
		installedACLLogin(ctx, reader, "INSERT INTO app.orders VALUES(99)", false)
		installedACLLogin(ctx, reader, "UPDATE app.orders SET value=99", false)
		installedACLLogin(ctx, reader, "DELETE FROM app.orders", false)
		installedACLLogin(ctx, reader, "CREATE TABLE app.denied(value int)", false)
		Expect(installedACLLogin(ctx, service, "SELECT nextval('app.order_ids')", true)).To(Equal("1"))
		installedACLLogin(ctx, reader, "SELECT nextval('app.order_ids')", false)
		Expect(installedACLLogin(ctx, llm, orders, true)).To(Equal("42"))
		installedACLLogin(ctx, llm, "SELECT * FROM app.hidden", false)
		installedDatabaseSQL(ctx, name, "SET ROLE "+owner+
			"; CREATE TABLE app.future(value int); INSERT INTO app.future VALUES(23)")
		Expect(installedACLLogin(ctx, reader, "SELECT value FROM app.future", true)).To(Equal("23"))
		installedACLLogin(ctx, llm, "SELECT * FROM app.future", false)
		databaseBefore = database.DeepCopy()
		database.Spec.Schemas[0].TablePermissions = database.Spec.Schemas[0].TablePermissions[1:]
		Expect(apiClient.Patch(ctx, database, client.MergeFrom(databaseBefore))).To(Succeed())
		database = installedDatabase(ctx, database, metav1.ConditionTrue, "Ready")
		installedACLLogin(ctx, llm, "SELECT * FROM app.orders", false)
		Expect(installedACLLogin(ctx, llm, "SELECT value FROM app.invoices", true)).To(Equal("7"))
		Expect(installedDatabaseSQL(ctx, name, orders)).To(Equal("42"))
		retainedUID := database.UID
		deleteObject(database)
		recreated := &epco.Database{Name: database.Name, Namespace: database.Namespace,
			Spec: epco.DatabaseSpec{Name: name, Owner: owner,
				Cluster: epco.ClusterReference{Name: cluster.Name},
				Schemas: []epco.SchemaSpec{{Name: installedDatabaseApplicationSchema}}}}
		Expect(apiClient.Create(ctx, recreated)).To(Succeed())
		DeferCleanup(func() { deleteObject(recreated) })
		recreated = installedDatabase(ctx, recreated, metav1.ConditionTrue, "Ready")
		Expect(recreated.UID).NotTo(Equal(retainedUID))
		Expect(installedACLLogin(ctx, llm, "SELECT value FROM app.invoices", true)).To(Equal("7"))
		recreatedBefore := recreated.DeepCopy()
		recreated.Spec.Schemas[0].TablePermissions = []epco.NamedTablePermissions{{Name: "invoices",
			Permissions: epco.TablePermissions{Select: []epco.Permission{{Name: llm, Ensure: epco.EnsureAbsent}}}}}
		Expect(apiClient.Patch(ctx, recreated, client.MergeFrom(recreatedBefore))).To(Succeed())
		recreated = installedDatabase(ctx, recreated, metav1.ConditionTrue, "Ready")
		installedACLLogin(ctx, llm, "SELECT * FROM app.invoices", false)
		Expect(installedACLLogin(ctx, reader, "SELECT value FROM app.invoices", true)).To(Equal("7"))
	}, NodeTimeout(7*time.Minute))
})
