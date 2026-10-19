package controller_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"math/big"
	"net/url"
	"strings"
	"testing"
	"time"

	epco "github.com/vterdunov/external-postgres-control-operator/api/v1alpha1"
	"github.com/vterdunov/external-postgres-control-operator/internal/controller"
	"github.com/vterdunov/external-postgres-control-operator/internal/postgres"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
)

func connectionTLSMaterial(t *testing.T) ([]byte, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	certificate := &x509.Certificate{SerialNumber: big.NewInt(1), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, certificate, certificate, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
}

type connectionEligibleSession struct{ *databaseSessionStub }

func (*connectionEligibleSession) CheckConnection(context.Context, string, string) error { return nil }

func TestDatabaseConnectionPublicCAAndJDBCRequireHaveMatchingVerification(t *testing.T) {
	for _, scenario := range []string{"default CA path", "spaces plus CA path"} {
		t.Run(scenario, func(t *testing.T) {
			db, role, _, r, cl, session := connectionFixture(t)
			ca, private := connectionTLSMaterial(t)
			for _, secret := range []*corev1.Secret{
				{Namespace: testInfraNamespace, Name: "public-ca", Data: map[string][]byte{connectionCAKey: ca, "private": private}},
				{Namespace: testInfraNamespace, Name: "admin-client", Type: corev1.SecretTypeTLS, Data: map[string][]byte{"tls.crt": ca, "tls.key": private}},
			} {
				if err := cl.Create(t.Context(), secret); err != nil {
					t.Fatal(err)
				}
			}
			cluster := &epco.ExternalPostgres{}
			if err := cl.Get(t.Context(), types.NamespacedName{Namespace: testInfraNamespace, Name: testDescriptorName}, cluster); err != nil {
				t.Fatal(err)
			}
			cluster.Spec.TLS = &epco.PostgresTLS{Mode: connectionTLSRequire, CASecret: &epco.SecretKeyReference{Name: "public-ca", Key: connectionCAKey}, ClientCertificateSecret: &epco.LocalReference{Name: "admin-client"}}
			connectionUpdate(t, cl, cluster)
			path := "/etc/postgresql/ca.crt"
			if scenario == "spaces plus CA path" {
				path = "/mounted CA/a+b root.crt"
				db = updateDatabase(t, cl, db, func(v *epco.Database) { v.Spec.ConnectionSecrets[0].SSLRootCertPath = path })
			}
			connectionOffline(r)
			runDatabase(t, r, db)
			output := connectionSecret(t, cl, db)
			if !bytes.Equal(output.Data["ca.crt"], ca) || string(output.Data["sslrootcert"]) != path || string(output.Data["sslmode"]) != connectionTLSRequire {
				t.Fatal("configured public CA or application mount path missing")
			}
			uri, err := url.Parse(string(output.Data["uri"]))
			if err != nil {
				t.Fatal("invalid PostgreSQL URI")
			}
			if uri.Query().Get("sslrootcert") != path || uri.Query().Get("sslmode") != connectionTLSRequire || strings.Contains(uri.RawQuery, "+") {
				t.Fatal("libpq URI must percent encode spaces and literal plus signs")
			}
			jdbc, err := url.Parse(strings.TrimPrefix(string(output.Data["jdbc-uri"]), "jdbc:"))
			if err != nil || jdbc.Query().Get("sslrootcert") != path || jdbc.Query().Get("sslmode") != "verify-ca" {
				t.Fatal("JDBC require with explicit CA must verify the configured CA")
			}
			for _, value := range output.Data {
				if bytes.Contains(value, private) || bytes.Contains(value, []byte("administrative-credential")) {
					t.Fatal("administrative client key or password leaked into application output")
				}
			}

			// Establish the SQL acknowledgement through the public role reconciler.
			roleSession := &roleSessionStub{state: &postgres.RoleState{Exists: true, Name: connectionSQLRole, AdminUser: testAdminName, ServerVersion: 180000, MaxIdentifierLength: 63, Login: true}}
			roleReconciler := &controller.DatabaseRoleReconciler{Client: cl, APIReader: cl, Builder: r.Builder, Roles: roleFactoryFunc(func(context.Context, *postgres.Config) (postgres.RoleSession, error) { return roleSession, nil }), Locks: &controller.ManagementLocks{}}
			runRole(t, roleReconciler, role)
			r.Databases = databaseFactoryFunc(func(context.Context, *postgres.Config) (postgres.DatabaseSession, error) {
				return &connectionEligibleSession{session}, nil
			})
			cluster.Spec.TLS.CASecret = nil
			cluster.Spec.TLS.ClientCertificateSecret = nil
			connectionUpdate(t, cl, cluster)
			// Descriptor changes require acknowledgement on the current configuration.
			runRole(t, roleReconciler, role)
			runDatabase(t, r, db)
			after := connectionSecret(t, cl, db)
			for _, key := range []string{"ca.crt", "sslrootcert"} {
				if _, ok := after.Data[key]; ok {
					t.Fatal("removed CA left stale output trust material")
				}
			}
			for _, key := range []string{"uri", "fqdn-uri", "jdbc-uri", "fqdn-jdbc-uri"} {
				parsed, err := url.Parse(strings.TrimPrefix(string(after.Data[key]), "jdbc:"))
				if err != nil || parsed.Query().Has("sslrootcert") || parsed.Query().Get("sslmode") != connectionTLSRequire {
					t.Fatal("removed CA left stale URI trust parameters")
				}
			}
		})
	}
}
