package controller_test

import (
	"bytes"
	"testing"

	epco "github.com/vterdunov/external-postgres-control-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func TestDatabaseConnectionConfiguredPublicCARequiresValidSourceEvenWithTLSDisabled(t *testing.T) {
	for _, scenario := range []string{"missing CA Secret", "empty configured CA key"} {
		t.Run(scenario, func(t *testing.T) {
			db, _, _, reconciler, cl, session := connectionFixture(t)
			cluster := &epco.PostgresCluster{}
			if err := cl.Get(t.Context(), types.NamespacedName{Namespace: testInfraNamespace, Name: testDescriptorName}, cluster); err != nil {
				t.Fatal(err)
			}
			cluster.Spec.TLS = &epco.PostgresTLS{Mode: dbTestTLSDisable, CASecret: &epco.SecretKeyReference{Name: connectionPublicCAName, Key: connectionCAKey}}
			connectionUpdate(t, cl, cluster)
			reason := "SecretNotFound"
			if scenario == "empty configured CA key" {
				if err := cl.Create(t.Context(), &corev1.Secret{Namespace: cluster.Namespace, Name: connectionPublicCAName,
					Data: map[string][]byte{connectionCAKey: {}}}); err != nil {
					t.Fatal(err)
				}
				reason = "InvalidSecret"
			}
			runDatabase(t, reconciler, db)
			connectionSecretMissing(t, cl, db)
			requireDatabaseCondition(t, currentDatabase(t, cl, db), metav1.ConditionFalse, reason)
			noDatabaseMutation(t, session)
		})
	}
}

func TestDatabaseConnectionConfiguredPublicCAIsExportedEvenWithAdministrativeTLSDisabled(t *testing.T) {
	db, _, _, reconciler, cl, session := connectionFixture(t)
	cluster := &epco.PostgresCluster{}
	if err := cl.Get(t.Context(), types.NamespacedName{Namespace: testInfraNamespace, Name: testDescriptorName}, cluster); err != nil {
		t.Fatal(err)
	}
	publicCA, _ := connectionTLSMaterial(t)
	if err := cl.Create(t.Context(), &corev1.Secret{Namespace: cluster.Namespace, Name: connectionPublicCAName,
		Data: map[string][]byte{connectionCAKey: publicCA}}); err != nil {
		t.Fatal(err)
	}
	cluster.Spec.TLS = &epco.PostgresTLS{Mode: dbTestTLSDisable, CASecret: &epco.SecretKeyReference{Name: connectionPublicCAName, Key: connectionCAKey}}
	connectionUpdate(t, cl, cluster)
	connectionOffline(reconciler)
	runDatabase(t, reconciler, db)
	output := connectionSecret(t, cl, db)
	if !bytes.Equal(output.Data["ca.crt"], publicCA) || string(output.Data["sslrootcert"]) != "/etc/postgresql/ca.crt" || string(output.Data["sslmode"]) != dbTestTLSDisable {
		t.Fatal("configured public CA must be exported independently of administrative TLS usage")
	}
	noDatabaseMutation(t, session)
}
