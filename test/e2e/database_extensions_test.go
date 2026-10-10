//go:build e2e

package e2e

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	epco "github.com/vterdunov/external-postgres-control-operator/api/v1alpha1"
	"github.com/vterdunov/external-postgres-control-operator/internal/controller"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var _ = Describe("R5 Installed Database extensions", func() {
	It("relocates hstore, retains omissions and preserves application data when RESTRICT blocks", func(ctx SpecContext) {
		const name = "epco_e2e_extensions"
		cluster := installedRoleDescriptor(ctx, "extensions-connection", nil)
		installedDatabaseFixtureCleanup(name)
		db := &epco.Database{Name: "extensions-database", Namespace: operatorNamespace,
			Spec: epco.DatabaseSpec{Name: name, Owner: installedDatabasePostgresName,
				Cluster:    epco.ClusterReference{Name: cluster.Name},
				Schemas:    []epco.SchemaSpec{{Name: "ext_old"}, {Name: "ext_new"}, {Name: "app"}},
				Extensions: []epco.ExtensionSpec{{Name: "hstore", Schema: "ext_old"}}}}
		Expect(apiClient.Create(ctx, db)).To(Succeed())
		DeferCleanup(func() { deleteObject(db) })
		db = installedDatabase(ctx, db, metav1.ConditionTrue, "Ready")
		catalog := "SELECT e.extname, n.nspname, pg_get_userbyid(e.extowner) FROM pg_catalog.pg_extension e " +
			"JOIN pg_catalog.pg_namespace n ON n.oid=e.extnamespace WHERE e.extname='hstore'"
		Expect(installedDatabaseSQL(ctx, name, catalog)).To(Equal("hstore|ext_old|postgres"))
		version := installedDatabaseSQL(ctx, name, "SELECT extversion FROM pg_catalog.pg_extension WHERE extname='hstore'")
		before := db.DeepCopy()
		db.Spec.Extensions[0].Schema = "ext_new"
		db.Spec.Schemas[0].Ensure = epco.EnsureAbsent
		Expect(apiClient.Patch(ctx, db, client.MergeFrom(before))).To(Succeed())
		db = installedDatabase(ctx, db, metav1.ConditionTrue, "Ready")
		Expect(installedDatabaseSQL(ctx, name, catalog)).To(Equal("hstore|ext_new|postgres"))
		Expect(installedDatabaseSQL(ctx, name,
			"SELECT count(*) FROM pg_catalog.pg_namespace WHERE nspname='ext_old'")).To(Equal("0"))
		installedDatabaseSQL(ctx, name, "CREATE TABLE app.sentinel(value ext_new.hstore); "+
			"INSERT INTO app.sentinel VALUES ('answer=>42')")
		before = db.DeepCopy()
		db.Spec.Extensions = nil
		Expect(apiClient.Patch(ctx, db, client.MergeFrom(before))).To(Succeed())
		db = installedDatabase(ctx, db, metav1.ConditionTrue, "Ready")
		Expect(installedDatabaseSQL(ctx, name, catalog)).To(Equal("hstore|ext_new|postgres"))
		Expect(installedDatabaseSQL(ctx, name,
			"SELECT extversion FROM pg_catalog.pg_extension WHERE extname='hstore'")).To(Equal(version))
		before = db.DeepCopy()
		db.Spec.Extensions = []epco.ExtensionSpec{{Name: "hstore", Ensure: epco.EnsureAbsent,
			Version: "irrelevant-unavailable-version", Schema: "irrelevant-missing-schema"}}
		Expect(apiClient.Patch(ctx, db, client.MergeFrom(before))).To(Succeed())
		db = installedDatabase(ctx, db, metav1.ConditionFalse, "DeletionBlocked")
		Expect(db.Status.HasManagedSQL).To(BeTrue())
		Expect(db.Status.AppliedGeneration).To(BeNumerically("<", db.Generation))
		Expect(installedDatabaseSQL(ctx, name,
			"SELECT value OPERATOR(ext_new.->) 'answer' FROM app.sentinel")).To(Equal("42"))
		Expect(installedDatabaseSQL(ctx, name, catalog)).To(Equal("hstore|ext_new|postgres"))
		installedDatabaseSQL(ctx, name, "DROP TABLE app.sentinel")
		// A spec event retries the same absence after the external dependency is removed.
		before = db.DeepCopy()
		db.Spec.Extensions[0].Version = "another-irrelevant-version"
		Expect(apiClient.Patch(ctx, db, client.MergeFrom(before))).To(Succeed())
		db = installedDatabase(ctx, db, metav1.ConditionTrue, "Ready")
		Expect(installedDatabaseSQL(ctx, name,
			"SELECT count(*) FROM pg_catalog.pg_extension WHERE extname='hstore'")).To(Equal("0"))
	}, NodeTimeout(5*time.Minute))

	It("publishes and rotates application outputs while an unrelated extension remains unavailable",
		func(ctx SpecContext) {
			const (
				name  = "epco_e2e_extension_output"
				login = "epco_e2e_extension_login"
			)
			cluster := installedRoleDescriptor(ctx, "extension-output-connection", nil)
			installedDatabaseFixtureCleanup(name)
			source := &corev1.Secret{Name: "extension-login-source", Namespace: operatorNamespace,
				Type: corev1.SecretTypeBasicAuth, Data: map[string][]byte{
					corev1.BasicAuthUsernameKey: []byte(login), corev1.BasicAuthPasswordKey: []byte("extension-initial-password")}}
			Expect(apiClient.Create(ctx, source)).To(Succeed())
			DeferCleanup(func() { deleteObject(source) })
			role := &epco.DatabaseRole{Name: "extension-login", Namespace: operatorNamespace,
				Spec: epco.DatabaseRoleSpec{Name: login, Login: true, ReclaimPolicy: epco.ReclaimDelete,
					Cluster:        epco.ClusterReference{Name: cluster.Name},
					PasswordSecret: &epco.LocalReference{Name: source.Name}}}
			Expect(apiClient.Create(ctx, role)).To(Succeed())
			DeferCleanup(func() { deleteObject(role) })
			role = installedRole(ctx, role, metav1.ConditionTrue, "Ready")
			db := &epco.Database{Name: "extension-output", Namespace: operatorNamespace,
				Spec: epco.DatabaseSpec{Name: name, Owner: installedDatabasePostgresName,
					Cluster:    epco.ClusterReference{Name: cluster.Name},
					Extensions: []epco.ExtensionSpec{{Name: "epco_unavailable_extension"}},
					ConnectionSecrets: []epco.ConnectionSecret{{Name: "extension-output-secret",
						RoleRef: epco.LocalReference{Name: role.Name}}}}}
			Expect(apiClient.Create(ctx, db)).To(Succeed())
			DeferCleanup(func() { deleteObject(db) })
			DeferCleanup(func() { deleteObject(&corev1.Secret{Name: "extension-output-secret", Namespace: operatorNamespace}) })
			output := installedConnectionOutput(ctx, operatorNamespace, "extension-output-secret")
			db = installedDatabase(ctx, db, metav1.ConditionFalse, "UnsupportedFeature")
			Expect(db.Status.HasManagedSQL).To(BeTrue(),
				"confirmed base lifecycle reserves SQL even while extension work is pending")
			Expect(db.Finalizers).To(ContainElement(controller.DatabaseFinalizer))
			Expect(installedConnectionLogin(ctx, output, "SELECT current_user", true)).To(Equal(login))
			outputUID := output.UID
			source.Data[corev1.BasicAuthPasswordKey] = []byte("extension-rotated-password")
			Expect(apiClient.Update(ctx, source)).To(Succeed())
			installedRole(ctx, role, metav1.ConditionTrue, "Ready")
			output = installedConnectionPassword(ctx, operatorNamespace, "extension-output-secret",
				source.Data[corev1.BasicAuthPasswordKey])
			Expect(output.UID).To(Equal(outputUID))
			Expect(installedConnectionLogin(ctx, output, "SELECT current_user", true)).To(Equal(login))
			db = installedDatabase(ctx, db, metav1.ConditionFalse, "UnsupportedFeature")
			Expect(meta.FindStatusCondition(db.Status.Conditions, "Ready").Status).To(Equal(metav1.ConditionFalse))
			Expect(db.Status.AppliedGeneration).To(BeZero())
		}, NodeTimeout(3*time.Minute))
})
