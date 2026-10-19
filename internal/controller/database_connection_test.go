package controller_test

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"maps"
	"net"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"

	epco "github.com/vterdunov/external-postgres-control-operator/api/v1alpha1"
	"github.com/vterdunov/external-postgres-control-operator/internal/controller"
	"github.com/vterdunov/external-postgres-control-operator/internal/postgres"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

const (
	connectionDropBlockedCase            = "drop blocked"
	connectionDefaultTLSCase             = "default TLS"
	connectionMissingRoleCase            = "missing role"
	connectionUnrelatedOutputCase        = "unrelated output"
	connectionOutputOmissionPath         = "output omission DELETE"
	connectionRetainedOutputUpdatePath   = "retained output UPDATE"
	connectionEntryRemovalCase           = "entry removal"
	connectionRetainDeletionCase         = "retain deletion"
	connectionEarlyOnlyDeleteCase        = "early-only delete"
	connectionDatabaseConflictCase       = "database SQL conflict"
	connectionRoleConflictCase           = "role SQL conflict"
	connectionAdminSourceCase            = "admin credentials source"
	testAllowedLabel                     = "allowed"
	testAccessDeniedReason               = "AccessDenied"
	testStatusSubresource                = "status"
	connectionPublicCAName               = "configured-public-ca"
	connectionIPv6Case                   = "IPv6 special characters"
	connectionMissingPasswordCase        = "missing password"
	connectionWrongUsernameCase          = "wrong username"
	connectionInvalidConfigurationReason = "InvalidConfiguration"
	connectionAccessDeniedDeletionCase   = "access denied deletion"
	connectionForeignOwnerCase           = "output owned by another Kubernetes object"
	connectionOwnershipConflictReason    = "OwnershipConflict"
	testConfigMapKind                    = "ConfigMap"
	connectionCAKey                      = "root"
	connectionTLSRequire                 = "require"
	connectionOutputCreatePath           = "output CREATE"
	connectionSourceReclaimPath          = "generated source reclaim DELETE"
	connectionConflictFault              = "Conflict"
	testSecretResource                   = "secrets"
	connectionAlreadyExistsFault         = "AlreadyExists"
	connectionNotFoundFault              = "NotFound"
	connectionCanceledFault              = "canceled"
	connectionDeadlineFault              = "deadline"
	connectionExplicitAbsentCase         = "explicit absent"
	connectionOutputName                 = "application-connection"
	connectionSourceName                 = "application-source"
	connectionRoleName                   = "application-role-resource"
	connectionSQLRole                    = "application_login"
	connectionPassword                   = "application-only-value"
)

func connectionDatabase() *epco.Database {
	db := ownershipDatabase(testOrdersNamespace, "application-database-resource", "database-uid")
	db.Spec.Name = "application_database"
	db.Spec.ConnectionSecrets = []epco.ConnectionSecret{{Name: connectionOutputName, RoleRef: epco.LocalReference{Name: connectionRoleName}}}
	return db
}

func connectionRole(db *epco.Database) *epco.DatabaseRole {
	return &epco.DatabaseRole{Namespace: db.Namespace, Name: connectionRoleName, UID: "role-uid", Generation: 3,
		Spec: epco.DatabaseRoleSpec{Cluster: db.Spec.Cluster, Name: connectionSQLRole, Login: true,
			ManagementPolicy: epco.ManagementManage, ReclaimPolicy: epco.ReclaimRetain,
			PasswordSecret: &epco.LocalReference{Name: connectionSourceName}}}
}

func connectionSource(role *epco.DatabaseRole) *corev1.Secret {
	return &corev1.Secret{Namespace: role.Namespace, Name: connectionSourceName, UID: "source-uid", ResourceVersion: "10",
		Type: corev1.SecretTypeBasicAuth, Data: map[string][]byte{
			corev1.BasicAuthUsernameKey:  []byte(controller.EffectiveRoleName(role)),
			corev1.BasicAuthPasswordKey:  []byte(connectionPassword),
			"unrelated-private-material": []byte("source-private-material"),
		}}
}

func connectionFixture(t *testing.T) (*epco.Database, *epco.DatabaseRole, *corev1.Secret, *controller.DatabaseReconciler, client.WithWatch, *databaseSessionStub) {
	t.Helper()
	db := connectionDatabase()
	role := connectionRole(db)
	source := connectionSource(role)
	r, cl, session := databaseFixture(t, db, role, source)
	session.state.Name = controller.EffectiveDatabaseName(db)
	return db, role, source, r, cl, session
}

func connectionUpdate(t *testing.T, cl client.Client, object client.Object) {
	t.Helper()
	existing := object.DeepCopyObject().(client.Object)
	if err := cl.Get(t.Context(), client.ObjectKeyFromObject(object), existing); err != nil {
		t.Fatal(err)
	}
	object.SetResourceVersion(existing.GetResourceVersion())
	if err := cl.Update(t.Context(), object); err != nil {
		t.Fatal(err)
	}
}

func connectionSecret(t *testing.T, cl client.Reader, db *epco.Database) *corev1.Secret {
	t.Helper()
	secret := &corev1.Secret{}
	if err := cl.Get(t.Context(), client.ObjectKey{Namespace: db.Namespace, Name: connectionOutputName}, secret); err != nil {
		t.Fatal("requested application connection Secret was not published")
	}
	return secret
}

func connectionSecretMissing(t *testing.T, cl client.Reader, db *epco.Database) {
	t.Helper()
	if err := cl.Get(t.Context(), client.ObjectKey{Namespace: db.Namespace, Name: connectionOutputName}, &corev1.Secret{}); !apierrors.IsNotFound(err) {
		t.Fatal("application connection Secret unexpectedly exists")
	}
}

func connectionOutage() error {
	return &postgres.Failure{Reason: postgres.DependenciesNotReady, Message: "isolated SQL endpoint unavailable"}
}

func connectionOffline(r *controller.DatabaseReconciler) {
	r.Databases = databaseFactoryFunc(func(context.Context, *postgres.Config) (postgres.DatabaseSession, error) {
		return nil, connectionOutage()
	})
}

func TestDatabaseConnectionInitialPublicationPrecedesSQLAndDependencyReadiness(t *testing.T) {
	for _, failure := range []string{"SQL open", "missing database owner", "SQL apply"} {
		t.Run(failure, func(t *testing.T) {
			db, role, _, r, cl, session := connectionFixture(t)
			if role.Status.HasManagedSQL || len(role.Status.Conditions) != 0 {
				t.Fatal("fixture unexpectedly starts SQL ready")
			}
			switch failure {
			case "SQL open":
				r.Databases = databaseFactoryFunc(func(context.Context, *postgres.Config) (postgres.DatabaseSession, error) {
					connectionSecret(t, cl, db)
					return nil, connectionOutage()
				})
			case "missing database owner":
				session.state.Exists = false
				db = updateDatabase(t, cl, db, func(v *epco.Database) { v.Spec.Owner = connectionSQLRole })
			case "SQL apply":
				session.apply = func(context.Context, postgres.DatabaseDesired) (*postgres.DatabaseApplyResult, error) {
					connectionSecret(t, cl, db)
					return nil, connectionOutage()
				}
			}
			runDatabase(t, r, db)
			output := connectionSecret(t, cl, db)
			if !bytes.Equal(output.Data[corev1.BasicAuthPasswordKey], []byte(connectionPassword)) {
				t.Fatal("initial publication did not use current application password source")
			}
			latest := currentDatabase(t, cl, db)
			if !slices.Contains(latest.Finalizers, controller.DatabaseFinalizer) {
				t.Fatal("initial publication lacks durable cleanup finalizer")
			}
			condition := meta.FindStatusCondition(latest.Status.Conditions, testReadyCondition)
			if condition == nil || condition.Status != metav1.ConditionFalse || latest.Status.AppliedGeneration != 0 {
				t.Fatal("Secret publication incorrectly established database SQL readiness")
			}
		})
	}
}

func TestDatabaseConnectionPayloadUsesEffectiveApplicationNamesAndStructuredEncoding(t *testing.T) {
	for _, scenario := range []string{"ordinary", "metadata fallbacks", connectionIPv6Case, "literal wildcard", "empty password", connectionDefaultTLSCase} {
		t.Run(scenario, func(t *testing.T) { connectionPayloadScenario(t, scenario) })
	}
}

func connectionPayloadScenario(t *testing.T, scenario string) {
	t.Helper()
	db, role, source, r, cl, _ := connectionFixture(t)
	cluster := &epco.ExternalPostgres{}
	if err := cl.Get(t.Context(), types.NamespacedName{Namespace: testInfraNamespace, Name: testDescriptorName}, cluster); err != nil {
		t.Fatal(err)
	}
	switch scenario {
	case "metadata fallbacks":
		db.Spec.Name = ""
		role.Spec.Name = ""
	case connectionIPv6Case:
		cluster.Spec.Connection.Host = "2001:db8::42"
		cluster.Spec.Connection.Port = 6432
		db.Spec.Name = "db /%+雪"
		role.Spec.Name = "role /%+雪"
		source.Data["password"] = []byte("pa:ss\\word /%+雪@?#&=")
	case "literal wildcard":
		db.Spec.Name = "*"
		role.Spec.Name = "*"
	case "empty password":
		source.Data["password"] = []byte{}
	case connectionDefaultTLSCase:
		cluster.Spec.TLS = nil
	}
	source.Data["username"] = []byte(controller.EffectiveRoleName(role))
	for _, object := range []client.Object{db, role, source, cluster} {
		connectionUpdate(t, cl, object)
	}
	connectionOffline(r)
	runDatabase(t, r, db)
	output := connectionSecret(t, cl, db)
	port, mode := connectionRequirePayloadIdentity(t, db, source, cluster, output, scenario)
	connectionRequirePayloadEncoding(t, db, role, source, cluster, output, port, mode)
}

func connectionRequirePayloadIdentity(t *testing.T, db *epco.Database, source *corev1.Secret,
	cluster *epco.ExternalPostgres, output *corev1.Secret, scenario string) (port, mode string) {
	t.Helper()
	keys := []string{"dbname", "fqdn-jdbc-uri", "fqdn-uri", "host", "jdbc-uri", "password", "pgpass", "port", "sslmode", "uri", "user", "username"}
	if output.Type != corev1.SecretTypeBasicAuth || len(output.Data) != len(keys) {
		t.Fatal("output must be basic-auth with exactly the specified application keys")
	}
	for _, key := range keys {
		if _, ok := output.Data[key]; !ok {
			t.Fatalf("missing required connection key %s", key)
		}
	}
	if !bytes.Equal(output.Data["username"], source.Data["username"]) || !bytes.Equal(output.Data["user"], source.Data["username"]) || !bytes.Equal(output.Data["password"], source.Data["password"]) || string(output.Data["dbname"]) != controller.EffectiveDatabaseName(db) {
		t.Fatal("application identity or password changed during rendering")
	}
	port, mode = "5432", dbTestTLSDisable
	if scenario == connectionIPv6Case {
		port = "6432"
	}
	if scenario == connectionDefaultTLSCase {
		mode = "verify-full"
	}
	if string(output.Data["host"]) != cluster.Spec.Connection.Host || string(output.Data["port"]) != port || string(output.Data["sslmode"]) != mode {
		t.Fatal("output endpoint or TLS defaults differ from descriptor")
	}
	if !bytes.Equal(output.Data["uri"], output.Data["fqdn-uri"]) || !bytes.Equal(output.Data["jdbc-uri"], output.Data["fqdn-jdbc-uri"]) {
		t.Fatal("external URI aliases differ")
	}
	return port, mode
}

func connectionRequirePayloadEncoding(t *testing.T, db *epco.Database, role *epco.DatabaseRole,
	source *corev1.Secret, cluster *epco.ExternalPostgres, output *corev1.Secret, port, mode string) {
	t.Helper()
	uri, err := url.Parse(string(output.Data["uri"]))
	if err != nil || uri.User == nil {
		t.Fatal("invalid PostgreSQL URI")
	}
	password, present := uri.User.Password()
	if uri.Scheme != "postgresql" || uri.Host != net.JoinHostPort(cluster.Spec.Connection.Host, port) || uri.User.Username() != string(source.Data["username"]) || !present || password != string(source.Data["password"]) || uri.Path != "/"+controller.EffectiveDatabaseName(db) || uri.Query().Get("sslmode") != mode {
		t.Fatal("PostgreSQL URI does not decode to the application connection")
	}
	if strings.Contains(strings.TrimPrefix(uri.EscapedPath(), "/"), "/") {
		t.Fatal("database must be encoded as one URI path segment")
	}
	jdbc, err := url.Parse(strings.TrimPrefix(string(output.Data["jdbc-uri"]), "jdbc:"))
	if err != nil || jdbc.Scheme != "postgresql" || jdbc.Host != uri.Host || jdbc.Path != uri.Path || jdbc.Query().Get("user") != uri.User.Username() || jdbc.Query().Get("password") != password || jdbc.Query().Get("sslmode") != mode {
		t.Fatal("JDBC URI does not decode to the application connection")
	}
	expected := pgpassExpected(cluster.Spec.Connection.Host, port, controller.EffectiveDatabaseName(db), controller.EffectiveRoleName(role), string(source.Data["password"]))
	if !bytes.Equal(output.Data["pgpass"], []byte(expected)) {
		t.Fatal("pgpass does not preserve exact fields, escapes or literal wildcard")
	}
	if len(output.OwnerReferences) != 0 {
		t.Fatal("connection output may not have garbage-collecting owner references")
	}
	for _, value := range output.Data {
		if bytes.Contains(value, []byte("administrative-credential")) || bytes.Contains(value, []byte("source-private-material")) {
			t.Fatal("administrative or unrelated source data leaked into application output")
		}
	}
}

func pgpassExpected(fields ...string) string {
	for i, value := range fields {
		value = strings.ReplaceAll(value, "\\", "\\\\")
		value = strings.ReplaceAll(value, ":", "\\:")
		if i < 4 {
			value = strings.ReplaceAll(value, "*", "\\*")
		}
		fields[i] = value
	}
	return strings.Join(fields, ":") + "\n"
}

func TestDatabaseConnectionRejectsInvalidExportInputsWithoutPublishingOrLeaking(t *testing.T) {
	for _, scenario := range []string{"password LF", "password CR", "password invalid UTF8", "database LF", "username LF", "CA path LF", connectionWrongUsernameCase, "wrong Secret type", connectionMissingPasswordCase, connectionMissingRoleCase, "NOLOGIN", "observe role", "disabled password", "unmanaged password", "other canonical cluster", "administrative role"} {
		t.Run(scenario, func(t *testing.T) {
			db, role, source, r, cl, session := connectionFixture(t)
			wantReason := "InvalidSecret"
			switch scenario {
			case "password LF":
				source.Data["password"] = []byte("red-secret\nvalue")
			case "password CR":
				source.Data["password"] = []byte("red-secret\rvalue")
			case "password invalid UTF8":
				source.Data["password"] = []byte{0xff, 's', 'e', 'c', 'r', 'e', 't'}
			case "database LF":
				db.Spec.Name = "db\nname"
				wantReason = connectionInvalidConfigurationReason
			case "username LF":
				role.Spec.Name = "role\nname"
				source.Data["username"] = []byte(role.Spec.Name)
				wantReason = connectionInvalidConfigurationReason
			case "CA path LF":
				db.Spec.ConnectionSecrets[0].SSLRootCertPath = "/root\npath"
				wantReason = connectionInvalidConfigurationReason
			case connectionWrongUsernameCase:
				source.Data["username"] = []byte("someone_else")
			case "wrong Secret type":
				source.Type = corev1.SecretTypeOpaque
			case connectionMissingPasswordCase:
				delete(source.Data, "password")
			case connectionMissingRoleCase:
				if err := cl.Delete(t.Context(), role); err != nil {
					t.Fatal(err)
				}
				wantReason = "DependenciesNotReady"
			case "NOLOGIN":
				role.Spec.Login = false
				wantReason = connectionInvalidConfigurationReason
			case "observe role":
				role.Spec.ManagementPolicy = epco.ManagementObserve
				wantReason = connectionInvalidConfigurationReason
			case "disabled password":
				role.Spec.DisablePassword = true
				role.Spec.PasswordSecret = nil
				wantReason = connectionInvalidConfigurationReason
			case "unmanaged password":
				role.Spec.PasswordSecret = nil
				wantReason = connectionInvalidConfigurationReason
			case "other canonical cluster":
				role.Spec.Cluster.Name = "different-descriptor"
				wantReason = connectionInvalidConfigurationReason
			case "administrative role":
				role.Spec.Name = testAdminName
				source.Data["username"] = []byte(testAdminName)
				wantReason = "ProtectedRole"
			}
			for _, object := range []client.Object{db, source} {
				connectionUpdate(t, cl, object)
			}
			if scenario != connectionMissingRoleCase {
				connectionUpdate(t, cl, role)
			}
			runDatabase(t, r, db)
			connectionSecretMissing(t, cl, db)
			requireDatabaseCondition(t, currentDatabase(t, cl, db), metav1.ConditionFalse, wantReason)
			noDatabaseMutation(t, session)
			encoded, err := json.Marshal(currentDatabase(t, cl, db).Status)
			if err != nil {
				t.Fatal(err)
			}
			for _, secretValue := range [][]byte{source.Data["password"], []byte("administrative-credential")} {
				if len(secretValue) > 0 && bytes.Contains(encoded, secretValue) {
					t.Fatal("credentials leaked into Database status")
				}
			}
		})
	}
}

func TestDatabaseConnectionGeneratedPasswordSourceIsInitializedBeforeSQLOpen(t *testing.T) {
	db := connectionDatabase()
	role := connectionRole(db)
	role.Spec.PasswordSecret = nil
	role.Spec.PasswordGeneration = &epco.PasswordGeneration{SecretName: connectionSourceName}
	dbReconciler, cl, _ := databaseFixture(t, db, role)
	r := &controller.DatabaseRoleReconciler{Client: cl, APIReader: cl, Builder: dbReconciler.Builder, Locks: &controller.ManagementLocks{}}
	r.Roles = roleFactoryFunc(func(context.Context, *postgres.Config) (postgres.RoleSession, error) {
		if !currentRole(t, cl, role).Status.GeneratedPasswordInitialized {
			t.Fatal("SQL opened before generated source initialization was persisted")
		}
		if err := cl.Get(t.Context(), client.ObjectKey{Namespace: role.Namespace, Name: connectionSourceName}, &corev1.Secret{}); err != nil {
			t.Fatal("SQL opened before generated password source was persisted")
		}
		return nil, connectionOutage()
	})
	runRole(t, r, role)
	source := &corev1.Secret{}
	if err := cl.Get(t.Context(), client.ObjectKey{Namespace: role.Namespace, Name: connectionSourceName}, source); err != nil {
		t.Fatal("SQL outage prevented generated password source initialization")
	}
	connectionOffline(dbReconciler)
	runDatabase(t, dbReconciler, db)
	if !bytes.Equal(connectionSecret(t, cl, db).Data["password"], source.Data["password"]) {
		t.Fatal("generated source was not reused for initial application export")
	}
}

func TestDatabaseConnectionSourceOutputExclusionIsSymmetric(t *testing.T) {
	for _, direction := range []string{"database output claims source", "role source claims output"} {
		for _, generated := range []bool{false, true} {
			t.Run(direction+map[bool]string{false: "/supplied", true: "/generated"}[generated], func(t *testing.T) {
				db := connectionDatabase()
				role := connectionRole(db)
				name := connectionOutputName
				if direction == "database output claims source" {
					name = connectionSourceName
					db.Spec.ConnectionSecrets[0].Name = name
				}
				if generated {
					role.Spec.PasswordSecret = nil
					role.Spec.PasswordGeneration = &epco.PasswordGeneration{SecretName: name}
				} else {
					role.Spec.PasswordSecret.Name = name
				}
				if direction == "role source claims output" {
					r, cl, session := roleFixture(t, role, db)
					session.apply = func(context.Context, postgres.RoleDesired) (*postgres.RoleApplyResult, error) {
						t.Fatal("source/output collision reached SQL mutation")
						return nil, nil
					}
					runRole(t, r, role)
					requireRoleCondition(t, currentRole(t, cl, role), metav1.ConditionFalse, "ConnectionSecretConflict")
					if generated {
						if err := cl.Get(t.Context(), client.ObjectKey{Namespace: role.Namespace, Name: name}, &corev1.Secret{}); !apierrors.IsNotFound(err) {
							t.Fatal("role generated credentials over a declared application output")
						}
					}
				} else {
					r, cl, session := databaseFixture(t, db, role)
					runDatabase(t, r, db)
					requireDatabaseCondition(t, currentDatabase(t, cl, db), metav1.ConditionFalse, "ConnectionSecretConflict")
					noDatabaseMutation(t, session)
				}
			})
		}
	}
}

func TestDatabaseConnectionPublicationIntentBlocksCreateWhenStatusCannotPersist(t *testing.T) {
	db, _, _, r, cl, _ := connectionFixture(t)
	failure := apierrors.NewServiceUnavailable("isolated intent status failure")
	r.Client = interceptor.NewClient(cl, interceptor.Funcs{SubResourceUpdate: func(context.Context, client.Client, string, client.Object, ...client.SubResourceUpdateOption) error {
		return failure
	}})
	_, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(db)})
	if !errors.Is(err, failure) {
		t.Fatal("publication did not return the durable status failure")
	}
	connectionSecretMissing(t, cl, db)
}

func TestDatabaseConnectionDeletedOutputCannotUseInitialPublicationAfterRestart(t *testing.T) {
	db, _, source, r, cl, _ := connectionFixture(t)
	connectionOffline(r)
	runDatabase(t, r, db)
	if err := cl.Delete(t.Context(), connectionSecret(t, cl, db)); err != nil {
		t.Fatal(err)
	}
	source.Data["password"] = []byte("second-unconfirmed-password")
	connectionUpdate(t, cl, source)
	restarted := &controller.DatabaseReconciler{Client: cl, APIReader: cl, Builder: r.Builder, Databases: r.Databases, Locks: &controller.ManagementLocks{}}
	runDatabase(t, restarted, db)
	connectionSecretMissing(t, cl, db)
}

func TestDatabaseConnectionRemovalAndLifecyclePreserveIndependentPasswordSource(t *testing.T) {
	for _, scenario := range []string{connectionEntryRemovalCase, "observe freeze", connectionRetainDeletionCase, connectionEarlyOnlyDeleteCase, connectionExplicitAbsentCase, connectionDropBlockedCase, connectionAccessDeniedDeletionCase} {
		t.Run(scenario, func(t *testing.T) {
			db, source, r, cl, session, before := connectionLifecycleScenario(t, scenario)
			runDatabase(t, r, db)
			if scenario == connectionEntryRemovalCase || scenario == connectionEarlyOnlyDeleteCase || scenario == connectionExplicitAbsentCase {
				connectionSecretMissing(t, cl, db)
			} else {
				after := connectionSecret(t, cl, db)
				if !reflect.DeepEqual(after.Data, before.Data) || after.UID != before.UID {
					t.Fatal("frozen or blocked output changed")
				}
			}
			if scenario == connectionEarlyOnlyDeleteCase && session.drops != 0 {
				t.Fatal("early-output-only deletion dropped unmanaged SQL")
			}
			if scenario == connectionExplicitAbsentCase && session.drops != 1 {
				t.Fatal("explicit absence did not confirm SQL deletion before output cleanup")
			}
			if scenario == connectionDropBlockedCase || scenario == connectionAccessDeniedDeletionCase {
				if !slices.Contains(currentDatabase(t, cl, db).Finalizers, controller.DatabaseFinalizer) {
					t.Fatal("pending cleanup lost its finalizer")
				}
			}
			current := &corev1.Secret{}
			if err := cl.Get(t.Context(), client.ObjectKeyFromObject(source), current); err != nil {
				t.Fatal("Database lifecycle removed the independent password source")
			}
			if !reflect.DeepEqual(current.Data, source.Data) {
				t.Fatal("Database lifecycle modified the password source")
			}
		})
	}
}

func connectionLifecycleScenario(t *testing.T, scenario string) (*epco.Database, *corev1.Secret,
	*controller.DatabaseReconciler, client.WithWatch, *databaseSessionStub, *corev1.Secret) {
	t.Helper()
	db, _, source, r, cl, session := connectionFixture(t)
	connectionOffline(r)
	runDatabase(t, r, db)
	before := connectionSecret(t, cl, db).DeepCopy()
	latest := currentDatabase(t, cl, db)
	if latest.Status.HasManagedSQL {
		t.Fatal("fixture must not have managed SQL")
	}
	if scenario == connectionExplicitAbsentCase || scenario == connectionDropBlockedCase {
		latest.Status.HasManagedSQL = true
		if err := cl.Status().Update(t.Context(), latest); err != nil {
			t.Fatal(err)
		}
	}
	switch scenario {
	case connectionEntryRemovalCase:
		latest.Spec.ConnectionSecrets = nil
	case "observe freeze":
		latest.Spec.ManagementPolicy = epco.ManagementObserve
		source.Data["password"] = []byte("unconfirmed-after-observe")
		connectionUpdate(t, cl, source)
	case connectionRetainDeletionCase:
		latest.Spec.ReclaimPolicy = epco.ReclaimRetain
	case connectionEarlyOnlyDeleteCase, connectionDropBlockedCase, connectionAccessDeniedDeletionCase:
		latest.Spec.ReclaimPolicy = epco.ReclaimDelete
	case connectionExplicitAbsentCase:
		latest.Spec.Ensure = epco.EnsureAbsent
		latest.Spec.ReclaimPolicy = epco.ReclaimRetain
	}
	latest.Generation++
	if err := cl.Update(t.Context(), latest); err != nil {
		t.Fatal(err)
	}
	if scenario == connectionRetainDeletionCase || scenario == connectionEarlyOnlyDeleteCase || scenario == connectionDropBlockedCase || scenario == connectionAccessDeniedDeletionCase {
		if err := cl.Delete(t.Context(), latest); err != nil {
			t.Fatal(err)
		}
	}
	if scenario == connectionAccessDeniedDeletionCase {
		cluster := &epco.ExternalPostgres{}
		if err := cl.Get(t.Context(), types.NamespacedName{Namespace: testInfraNamespace, Name: testDescriptorName}, cluster); err != nil {
			t.Fatal(err)
		}
		cluster.Spec.Access.NamespaceSelector = &metav1.LabelSelector{MatchLabels: map[string]string{testAllowedLabel: "yes"}}
		if err := cl.Update(t.Context(), cluster); err != nil {
			t.Fatal(err)
		}
	}
	if scenario == connectionExplicitAbsentCase || scenario == connectionDropBlockedCase {
		r.Databases = databaseFactoryFunc(func(context.Context, *postgres.Config) (postgres.DatabaseSession, error) { return session, nil })
		if scenario == connectionDropBlockedCase {
			session.drop = func(context.Context, string) error {
				return &postgres.Failure{Reason: postgres.DeletionBlocked, Message: "isolated dependency"}
			}
		}
	}
	return db, source, r, cl, session, before
}

func TestDatabaseConnectionAuthorizationAndOwnershipPrecedePublication(t *testing.T) {
	for _, scenario := range []string{"namespace denied", connectionDatabaseConflictCase, connectionRoleConflictCase, connectionUnrelatedOutputCase, connectionForeignOwnerCase, connectionAdminSourceCase} {
		t.Run(scenario, func(t *testing.T) {
			db, role, source, r, cl, session := connectionFixture(t)
			wantReason := "ConnectionSecretConflict"
			switch scenario {
			case "namespace denied":
				cluster := &epco.ExternalPostgres{}
				if err := cl.Get(t.Context(), types.NamespacedName{Namespace: testInfraNamespace, Name: testDescriptorName}, cluster); err != nil {
					t.Fatal(err)
				}
				cluster.Spec.Access.NamespaceSelector = &metav1.LabelSelector{MatchLabels: map[string]string{"access": testAllowedLabel}}
				connectionUpdate(t, cl, cluster)
				wantReason = testAccessDeniedReason
			case connectionDatabaseConflictCase:
				winner := db.DeepCopy()
				winner.Name = "recorded-database-manager"
				winner.UID = "other-database-uid"
				winner.ResourceVersion = ""
				winner.Spec.ConnectionSecrets = nil
				winner.Status.HasManagedSQL = true
				if err := cl.Create(t.Context(), winner); err != nil {
					t.Fatal(err)
				}
				wantReason = connectionOwnershipConflictReason
			case connectionRoleConflictCase:
				winner := role.DeepCopy()
				winner.Name = "recorded-role-manager"
				winner.UID = "other-role-uid"
				winner.ResourceVersion = ""
				winner.Status.HasManagedSQL = true
				if err := cl.Create(t.Context(), winner); err != nil {
					t.Fatal(err)
				}
				wantReason = connectionOwnershipConflictReason
			case connectionUnrelatedOutputCase, connectionForeignOwnerCase:
				unrelated := &corev1.Secret{Namespace: db.Namespace, Name: connectionOutputName, Type: corev1.SecretTypeOpaque, Data: map[string][]byte{"private": []byte("independent-data")}}
				if scenario == connectionForeignOwnerCase {
					unrelated.OwnerReferences = []metav1.OwnerReference{{APIVersion: "v1", Kind: testConfigMapKind, Name: "active-owner", UID: "active-uid"}}
				}
				if err := cl.Create(t.Context(), unrelated); err != nil {
					t.Fatal(err)
				}
			case connectionAdminSourceCase:
				// Administrative source identity is namespace qualified. Use a local
				// descriptor selecting this exact source, rather than its name alone.
				local := &epco.ExternalPostgres{Namespace: db.Namespace, Name: testDescriptorName,
					Spec: epco.ExternalPostgresSpec{Connection: epco.PostgresConnection{Host: testEndpointHost},
						CredentialsSecret: epco.LocalReference{Name: source.Name}, TLS: &epco.PostgresTLS{Mode: dbTestTLSDisable}}}
				if err := cl.Create(t.Context(), local); err != nil {
					t.Fatal(err)
				}
				db.Spec.Cluster.Namespace = db.Namespace
				role.Spec.Cluster = db.Spec.Cluster
				connectionUpdate(t, cl, db)
				connectionUpdate(t, cl, role)
				wantReason = "ProtectedRole"
			}
			runDatabase(t, r, db)
			requireDatabaseCondition(t, currentDatabase(t, cl, db), metav1.ConditionFalse, wantReason)
			noDatabaseMutation(t, session)
			if scenario == connectionUnrelatedOutputCase || scenario == connectionForeignOwnerCase {
				output := connectionSecret(t, cl, db)
				if output.Type != corev1.SecretTypeOpaque || !bytes.Equal(output.Data["private"], []byte("independent-data")) {
					t.Fatal("conflicting output was overwritten")
				}
			} else {
				connectionSecretMissing(t, cl, db)
			}
		})
	}
}

func TestDatabaseConnectionOutputNamesHaveOneDatabaseManagerAcrossClusters(t *testing.T) {
	db, _, _, r, cl, session := connectionFixture(t)
	competitor := connectionDatabase()
	competitor.Name = "competing-database"
	competitor.UID = "competing-uid"
	competitor.Spec.Name = "other_database"
	competitor.Spec.Cluster.Name = "another-descriptor"
	if err := cl.Create(t.Context(), competitor); err != nil {
		t.Fatal(err)
	}
	runDatabase(t, r, db)
	requireDatabaseCondition(t, currentDatabase(t, cl, db), metav1.ConditionFalse, "ConnectionSecretConflict")
	connectionSecretMissing(t, cl, db)
	noDatabaseMutation(t, session)
}

func TestDatabaseConnectionRetainedAdoptionPreservesPayloadUntilSQLConfirmation(t *testing.T) {
	for _, scenario := range []string{"valid retained", "tampered provenance", "other effective database"} {
		t.Run(scenario, func(t *testing.T) {
			db, _, source, r, cl, _ := connectionFixture(t)
			connectionOffline(r)
			runDatabase(t, r, db)
			before := connectionSecret(t, cl, db).DeepCopy()
			if len(before.Annotations) == 0 || len(before.OwnerReferences) != 0 {
				t.Fatal("output lacks retention-safe provenance")
			}
			if err := cl.Delete(t.Context(), currentDatabase(t, cl, db)); err != nil {
				t.Fatal(err)
			}
			runDatabase(t, r, db)
			recreated := connectionDatabase()
			recreated.UID = "replacement-uid"
			if scenario == "other effective database" {
				recreated.Spec.Name = "another_application_database"
			}
			if err := cl.Create(t.Context(), recreated); err != nil {
				t.Fatal(err)
			}
			source.Data["password"] = []byte("unconfirmed-after-database-recreation")
			connectionUpdate(t, cl, source)
			if scenario == "tampered provenance" {
				current := connectionSecret(t, cl, db)
				for key := range current.Annotations {
					current.Annotations[key] = "unrelated-provenance"
				}
				connectionUpdate(t, cl, current)
			}
			runDatabase(t, r, recreated)
			after := connectionSecret(t, cl, recreated)
			if !reflect.DeepEqual(after.Data, before.Data) || after.UID != before.UID {
				t.Fatal("retained output was rotated before current SQL confirmation")
			}
			if scenario == "valid retained" {
				annotationValues := slices.Collect(maps.Values(after.Annotations))
				if !slices.Contains(annotationValues, string(recreated.UID)) || slices.Contains(annotationValues, string(db.UID)) {
					t.Fatal("retained output provenance was not conditionally adopted by the new Database UID")
				}
			} else {
				requireDatabaseCondition(t, currentDatabase(t, cl, recreated), metav1.ConditionFalse, "ConnectionSecretConflict")
			}
		})
	}
}
