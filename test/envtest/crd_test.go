//go:build envtest

package envtest_test

import (
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	nameField    = "name"
	clusterField = "cluster"
	specField    = "spec"
	passwordKey  = "password"
)

func resource(kind string) *unstructured.Unstructured {
	spec := map[string]any{}
	switch kind {
	case "PostgresCluster":
		spec["connection"] = map[string]any{"host": "postgres.example.test"}
		spec["credentialsSecret"] = map[string]any{nameField: "credentials"}
	case "Database":
		spec[clusterField] = map[string]any{nameField: clusterField}
		spec["owner"] = "owner"
	case "DatabaseRole":
		spec[clusterField] = map[string]any{nameField: clusterField}
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "postgresql.epco.io/v1alpha1", "kind": kind,
		"metadata": map[string]any{nameField: "sample", "namespace": namespace.Name}, specField: spec,
	}}
}

func field(object *unstructured.Unstructured, path ...string) any {
	value, found, err := unstructured.NestedFieldNoCopy(object.Object, path...)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	ExpectWithOffset(1, found).To(BeTrue(), "missing field %v", path)
	return value
}

func setField(object *unstructured.Unstructured, value any, path ...string) {
	ExpectWithOffset(1, unstructured.SetNestedField(object.Object, value, path...)).To(Succeed())
}

var _ = Describe("installed CRD contracts", func() {
	It("defaults the cluster connection, timeouts and explicit TLS stanza", func(ctx SpecContext) {
		object := resource("PostgresCluster")
		setField(object, map[string]any{}, specField, "tls")
		Expect(scope.Create(ctx, object)).To(Succeed())
		Expect(field(object, specField, "connection", "port")).To(Equal(int64(5432)))
		Expect(field(object, specField, "connection", "database")).To(Equal("postgres"))
		Expect(field(object, specField, "reconcileInterval")).To(Equal("5m"))
		Expect(field(object, specField, "connectTimeout")).To(Equal("10s"))
		Expect(field(object, specField, "statementTimeout")).To(Equal("30s"))
		Expect(field(object, specField, "tls", "mode")).To(Equal("verify-full"))
	})

	It("defaults database lifecycle and nested permissions", func(ctx SpecContext) {
		object := resource("Database")
		setField(object, []any{map[string]any{
			nameField: "app", "permissions": map[string]any{"usage": []any{map[string]any{nameField: "reader"}}},
		}}, specField, "schemas")
		Expect(scope.Create(ctx, object)).To(Succeed())
		Expect(field(object, specField, "ensure")).To(Equal("present"))
		Expect(field(object, specField, "managementPolicy")).To(Equal("manage"))
		Expect(field(object, specField, "databaseReclaimPolicy")).To(Equal("retain"))
		schemas := field(object, specField, "schemas").([]any)
		schema := schemas[0].(map[string]any)
		Expect(schema["ensure"]).To(Equal("present"))
		permission := schema["permissions"].(map[string]any)["usage"].([]any)[0].(map[string]any)
		Expect(permission["ensure"]).To(Equal("present"))
	})

	It("defaults role attributes without granting login or administrative privileges", func(ctx SpecContext) {
		object := resource("DatabaseRole")
		Expect(scope.Create(ctx, object)).To(Succeed())
		attributes := []string{"login", "superuser", "createdb", "createrole", "replication", "bypassrls", "disablePassword"}
		for _, attribute := range attributes {
			Expect(field(object, specField, attribute)).To(BeFalse(), attribute)
		}
		Expect(field(object, specField, "inherit")).To(BeTrue())
		Expect(field(object, specField, "connectionLimit")).To(Equal(int64(-1)))
		Expect(field(object, specField, "membershipPolicy")).To(Equal("authoritative"))
		Expect(field(object, specField, "managementPolicy")).To(Equal("manage"))
		Expect(field(object, specField, "databaseRoleReclaimPolicy")).To(Equal("retain"))
	})

	DescribeTable("rejects invalid CEL combinations at admission", func(
		ctx SpecContext, kind string, mutate func(*unstructured.Unstructured), message string,
	) {
		object := resource(kind)
		mutate(object)
		err := scope.Create(ctx, object)
		Expect(apierrors.IsInvalid(err)).To(BeTrue(), "%v", err)
		Expect(err).To(MatchError(ContainSubstring(message)))
	},
		Entry("missing managed database owner", "Database", func(o *unstructured.Unstructured) {
			unstructured.RemoveNestedField(o.Object, specField, "owner")
		}, "owner is required"),
		Entry("observe database with explicit absence", "Database", func(o *unstructured.Unstructured) {
			setField(o, "observe", specField, "managementPolicy")
			setField(o, "absent", specField, "ensure")
		}, "observe requires"),
		Entry("ICU locale with libc provider", "Database", func(o *unstructured.Unstructured) {
			setField(o, "en-US", specField, "icuLocale")
			setField(o, "libc", specField, "localeProvider")
		}, "icuLocale requires"),
		Entry("mutually exclusive password sources", "DatabaseRole", func(o *unstructured.Unstructured) {
			setField(o, map[string]any{nameField: passwordKey}, specField, "passwordSecret")
			setField(o, map[string]any{"secretName": "generated"}, specField, "passwordGeneration")
		}, "mutually exclusive"),
		Entry("disabled password with source", "DatabaseRole", func(o *unstructured.Unstructured) {
			setField(o, true, specField, "disablePassword")
			setField(o, map[string]any{nameField: passwordKey}, specField, "passwordSecret")
		}, "disablePassword cannot"),
		Entry("observed role with delete policy", "DatabaseRole", func(o *unstructured.Unstructured) {
			setField(o, "observe", specField, "managementPolicy")
			setField(o, "delete", specField, "databaseRoleReclaimPolicy")
		}, "observe requires"),
		Entry("zero positive duration", "PostgresCluster", func(o *unstructured.Unstructured) {
			setField(o, "0s", specField, "connectTimeout")
		}, "duration must be positive"),
	)

	DescribeTable("keeps SQL identities immutable", func(
		ctx SpecContext, kind string, path []string, value any, message string,
	) {
		object := resource(kind)
		Expect(scope.Create(ctx, object)).To(Succeed())
		setField(object, value, path...)
		err := environment.Client.Update(ctx, object)
		Expect(apierrors.IsInvalid(err)).To(BeTrue(), "%v", err)
		Expect(err).To(MatchError(ContainSubstring(message)))
	},
		Entry("database SQL name", "Database", []string{specField, nameField}, "other", "effective SQL name is immutable"),
		Entry("role SQL name", "DatabaseRole", []string{specField, nameField}, "other", "effective SQL name is immutable"),
		Entry("database cluster", "Database", []string{specField, clusterField, nameField},
			"other", "cluster reference is immutable"),
		Entry("role cluster", "DatabaseRole", []string{specField, clusterField, "namespace"},
			"other", "cluster reference is immutable"),
		Entry("creation-only encoding added", "Database", []string{specField, "encoding"}, "UTF8", "encoding is immutable"),
	)

	It("allows the fallback SQL name to become explicit without changing identity", func(ctx SpecContext) {
		object := resource("Database")
		Expect(scope.Create(ctx, object)).To(Succeed())
		setField(object, object.GetName(), specField, nameField)
		Expect(environment.Client.Update(ctx, object)).To(Succeed())
	})

	It("rejects changes and removal of every creation-only database option", func(ctx SpecContext) {
		options := map[string]string{
			"template": "template0", "encoding": "UTF8", "locale": "C", "localeProvider": "icu",
			"localeCollate": "C", "localeCType": "C", "icuLocale": "en-US", "icuRules": "&a<b",
			"builtinLocale": "C", "collationVersion": "1",
		}
		for name, initial := range options {
			object := resource("Database")
			object.SetName("immutable-" + strings.ToLower(name))
			if name == "icuLocale" || name == "icuRules" {
				setField(object, "icu", specField, "localeProvider")
			}
			if name == "builtinLocale" {
				setField(object, "builtin", specField, "localeProvider")
			}
			setField(object, initial, specField, name)
			Expect(scope.Create(ctx, object)).To(Succeed(), name)
			for _, remove := range []bool{false, true} {
				changed := object.DeepCopy()
				if remove {
					unstructured.RemoveNestedField(changed.Object, specField, name)
				} else {
					replacement := "other"
					if name == "localeProvider" {
						replacement = "libc"
					}
					setField(changed, replacement, specField, name)
				}
				err := environment.Client.Update(ctx, changed)
				Expect(apierrors.IsInvalid(err)).To(BeTrue(), "%s: %v", name, err)
				Expect(err).To(MatchError(ContainSubstring(name + " is immutable")))
			}
		}
	})

	It("keeps the generated password source immutable once configured", func(ctx SpecContext) {
		object := resource("DatabaseRole")
		setField(object, map[string]any{"secretName": "original"}, specField, "passwordGeneration")
		Expect(scope.Create(ctx, object)).To(Succeed())
		for _, remove := range []bool{false, true} {
			changed := object.DeepCopy()
			if remove {
				unstructured.RemoveNestedField(changed.Object, specField, "passwordGeneration")
			} else {
				setField(changed, "replacement", specField, "passwordGeneration", "secretName")
			}
			err := environment.Client.Update(ctx, changed)
			Expect(apierrors.IsInvalid(err)).To(BeTrue(), "%v", err)
			Expect(err).To(MatchError(ContainSubstring("generated password source is immutable")))
		}
	})

	DescribeTable("isolates status writes and rejects stale resource versions", func(ctx SpecContext, kind string) {
		object := resource(kind)
		setField(object, "discard-on-create", "status", "message")
		Expect(scope.Create(ctx, object)).To(Succeed())
		_, found, err := unstructured.NestedFieldNoCopy(object.Object, "status", "message")
		Expect(err).NotTo(HaveOccurred())
		Expect(found).To(BeFalse())
		setField(object, "discard-on-update", "status", "message")
		object.SetLabels(map[string]string{"test": "updated"})
		Expect(environment.Client.Update(ctx, object)).To(Succeed())
		_, found, err = unstructured.NestedFieldNoCopy(object.Object, "status", "message")
		Expect(err).NotTo(HaveOccurred())
		Expect(found).To(BeFalse())
		stale := object.DeepCopy()
		generation := object.GetGeneration()
		setField(object, "observed", "status", "message")
		setField(object, generation, "status", "observedGeneration")
		var specPath []string
		var attemptedSpecValue any
		switch kind {
		case "PostgresCluster":
			specPath, attemptedSpecValue = []string{specField, "connection", "host"}, "ignored.example.test"
		case "Database":
			specPath, attemptedSpecValue = []string{specField, "owner"}, "ignored-owner"
		case "DatabaseRole":
			specPath, attemptedSpecValue = []string{specField, "login"}, true
		}
		originalSpecValue := field(object, specPath...)
		setField(object, attemptedSpecValue, specPath...)
		Expect(environment.Client.Status().Update(ctx, object)).To(Succeed())
		Expect(field(object, specPath...)).To(Equal(originalSpecValue))
		Expect(field(object, "status", "message")).To(Equal("observed"))
		Expect(object.GetGeneration()).To(Equal(generation))
		stale.SetLabels(map[string]string{"test": "stale"})
		Expect(apierrors.IsConflict(environment.Client.Update(ctx, stale))).To(BeTrue())
		setField(stale, "stale", "status", "message")
		Expect(apierrors.IsConflict(environment.Client.Status().Update(ctx, stale))).To(BeTrue())
		Expect(environment.Client.Get(ctx, client.ObjectKeyFromObject(object), object)).To(Succeed())
		Expect(object.GetLabels()).To(HaveKeyWithValue("test", "updated"))
		Expect(field(object, "status", "message")).To(Equal("observed"))
	}, Entry("PostgresCluster", "PostgresCluster"), Entry("Database", "Database"), Entry("DatabaseRole", "DatabaseRole"))

	DescribeTable("retains deleting resources until their finalizer is removed", func(ctx SpecContext, kind string) {
		object := resource(kind)
		object.SetFinalizers([]string{"tests.epco.io/lifecycle"})
		Expect(scope.Create(ctx, object)).To(Succeed())
		Expect(environment.Client.Delete(ctx, object)).To(Succeed())
		Expect(environment.Client.Get(ctx, client.ObjectKeyFromObject(object), object)).To(Succeed())
		Expect(object.GetDeletionTimestamp()).NotTo(BeNil())
		Expect(object.GetFinalizers()).To(ConsistOf("tests.epco.io/lifecycle"))
		object.SetFinalizers(nil)
		Expect(environment.Client.Update(ctx, object)).To(Succeed())
		Expect(scope.WaitGone(ctx, object)).To(Succeed())
	}, Entry("PostgresCluster", "PostgresCluster"), Entry("Database", "Database"), Entry("DatabaseRole", "DatabaseRole"))
})
