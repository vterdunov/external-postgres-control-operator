package controller_test

import (
	"context"
	"errors"
	"testing"

	epco "github.com/vterdunov/external-postgres-control-operator/api/v1alpha1"
	"github.com/vterdunov/external-postgres-control-operator/internal/postgres"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	testExtensionName           = "hstore"
	testExtensionPublicSchema   = "public"
	testExtensionAbsentMismatch = "absent mismatch"
	testExtensionCancelled      = "cancelled"
)

type extensionSessionStub struct {
	*databaseSessionStub
	states []postgres.ExtensionState
}

func TestDatabaseExtensionsRequireExplicitAdapterCapability(t *testing.T) {
	db := ownershipDatabase(testOrdersNamespace, dbTestApplicationName, dbTestResourceUID)
	db.Spec.Extensions = []epco.ExtensionSpec{{Name: testExtensionName}}
	r, cl, _ := databaseFixture(t, db)
	runDatabase(t, r, db)
	requireDatabaseCondition(t, currentDatabase(t, cl, db), metav1.ConditionFalse, postgres.UnsupportedFeature)
}

func TestDatabaseExtensionObservationChecksRequestedStateAndPreservesHistory(t *testing.T) {
	for _, scenario := range []string{"match", dbTestMissingOwner, "version mismatch", "schema mismatch", "absent match", testExtensionAbsentMismatch, "former manager"} {
		t.Run(scenario, func(t *testing.T) {
			db := ownershipDatabase(testOrdersNamespace, dbTestApplicationName, dbTestResourceUID)
			db.Spec.ManagementPolicy = epco.ManagementObserve
			db.Spec.Extensions = []epco.ExtensionSpec{{Name: testExtensionName, Version: "1.8", Schema: dbTestApplicationName}}
			db.Spec.ConnectionSecrets = []epco.ConnectionSecret{{Name: "ignored-observed-output", RoleRef: epco.LocalReference{Name: "missing-role"}}}
			extension := postgres.ExtensionState{Name: testExtensionName, Version: "1.8", Schema: dbTestApplicationName, Exists: true}
			ready := metav1.ConditionTrue
			switch scenario {
			case dbTestMissingOwner:
				extension.Exists = false
				ready = metav1.ConditionFalse
			case "version mismatch":
				extension.Version = "1.7"
				ready = metav1.ConditionFalse
			case "schema mismatch":
				extension.Schema = testExtensionPublicSchema
				ready = metav1.ConditionFalse
			case "absent match", testExtensionAbsentMismatch:
				db.Spec.Extensions[0].Ensure = epco.EnsureAbsent
				extension.Exists = scenario == testExtensionAbsentMismatch
				if extension.Exists {
					ready = metav1.ConditionFalse
				}
			case "former manager":
				db.Status.HasManagedSQL = true
				db.Status.AppliedGeneration = 1
			}
			// Rebuild after preparing historical status and desired state.
			r, cl, session := databaseFixture(t, db)
			extensions := &extensionSessionStub{databaseSessionStub: session, states: []postgres.ExtensionState{extension}}
			r.Databases = databaseFactoryFunc(func(context.Context, *postgres.Config) (postgres.DatabaseSession, error) { return extensions, nil })
			runDatabase(t, r, db)
			latest := currentDatabase(t, cl, db)
			reason := "Observed"
			if ready == metav1.ConditionFalse {
				reason = postgres.DependenciesNotReady
			}
			requireDatabaseCondition(t, latest, ready, reason)
			if latest.Status.Applied != nil || latest.Status.AppliedGeneration != db.Status.AppliedGeneration || latest.Status.HasManagedSQL != db.Status.HasManagedSQL {
				t.Fatal("extension observation changed managed lifecycle history")
			}
			noDatabaseMutation(t, session)
			outputs := &corev1.SecretList{}
			if err := cl.List(t.Context(), outputs, client.InNamespace(db.Namespace)); err != nil {
				t.Fatal(err)
			}
			if len(outputs.Items) != 0 {
				t.Fatal("extension observer published connection output")
			}
		})
	}
}

func TestDatabaseExtensionPendingReservesConfirmedBaseAndRejectsContender(t *testing.T) {
	db := ownershipDatabase(testOrdersNamespace, dbTestApplicationName, dbTestResourceUID)
	db.Spec.Extensions = []epco.ExtensionSpec{{Name: testExtensionName}}
	r, cl, session := databaseFixture(t, db)
	session.state.Exists = false
	extensions := &extensionSessionStub{databaseSessionStub: session}
	session.apply = func(context.Context, postgres.DatabaseDesired) (*postgres.DatabaseApplyResult, error) {
		session.state.Exists = true
		return &postgres.DatabaseApplyResult{BaseManaged: true}, &postgres.Failure{Reason: postgres.DependenciesNotReady, Message: "extension prerequisite missing"}
	}
	r.Databases = databaseFactoryFunc(func(context.Context, *postgres.Config) (postgres.DatabaseSession, error) { return extensions, nil })
	runDatabase(t, r, db)
	latest := currentDatabase(t, cl, db)
	requireDatabaseCondition(t, latest, metav1.ConditionFalse, postgres.DependenciesNotReady)
	if !session.state.Exists || !latest.Status.HasManagedSQL || latest.Status.AppliedGeneration != 0 || latest.Status.LastSuccessfulReconcileTime != nil {
		t.Fatal("pending extension lost confirmed base reservation or falsely published full success")
	}
	other := ownershipDatabase(testBillingNamespace, "contender", "contender-uid")
	if err := cl.Create(t.Context(), other); err != nil {
		t.Fatal(err)
	}
	if err := cl.Create(t.Context(), &corev1.Namespace{Name: other.Namespace}); err != nil {
		t.Fatal(err)
	}
	runDatabase(t, r, other)
	requireDatabaseCondition(t, currentDatabase(t, cl, other), metav1.ConditionFalse, postgres.OwnershipConflict)
}

func TestDatabaseExtensionCompletionRejectsChangedSpecAndCancellation(t *testing.T) {
	for _, scenario := range []string{"spec changed", testExtensionCancelled} {
		t.Run(scenario, func(t *testing.T) {
			db := ownershipDatabase(testOrdersNamespace, dbTestApplicationName, dbTestResourceUID)
			db.Spec.Extensions = []epco.ExtensionSpec{{Name: testExtensionName}}
			r, cl, session := databaseFixture(t, db)
			extensions := &extensionSessionStub{databaseSessionStub: session}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			session.apply = func(context.Context, postgres.DatabaseDesired) (*postgres.DatabaseApplyResult, error) {
				if scenario == testExtensionCancelled {
					cancel()
				} else {
					updateDatabase(t, cl, db, func(latest *epco.Database) { latest.Spec.Extensions[0].Version = "latest-request" })
				}
				extensions.states = []postgres.ExtensionState{{Name: testExtensionName, Exists: true}}
				return &postgres.DatabaseApplyResult{BaseManaged: true}, nil
			}
			r.Databases = databaseFactoryFunc(func(context.Context, *postgres.Config) (postgres.DatabaseSession, error) { return extensions, nil })
			_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(db)})
			if scenario == testExtensionCancelled && !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation was lost: %v", err)
			}
			latest := currentDatabase(t, cl, db)
			condition := meta.FindStatusCondition(latest.Status.Conditions, testReadyCondition)
			if latest.Status.AppliedGeneration != 0 || (condition != nil && condition.Status == metav1.ConditionTrue) {
				t.Fatal("stale or cancelled extension completion published Ready")
			}
		})
	}
}

func (s *extensionSessionStub) InspectExtensions(context.Context, string, []epco.ExtensionSpec) ([]postgres.ExtensionState, error) {
	return s.states, nil
}

func (s *extensionSessionStub) ReconcileExtensions(context.Context, string, []epco.ExtensionSpec) (*postgres.ExtensionResult, error) {
	return &postgres.ExtensionResult{States: s.states}, nil
}

func TestDatabaseExtensionDeclarationsDoNotBlockDatabaseAbsence(t *testing.T) {
	db := ownershipDatabase(testOrdersNamespace, dbTestApplicationName, dbTestResourceUID)
	db.Spec.Ensure = epco.EnsureAbsent
	db.Spec.Extensions = []epco.ExtensionSpec{{Name: testExtensionName, Version: "unavailable-version", Schema: "missing-schema"}}
	r, cl, session := databaseFixture(t, db)
	session.drop = func(context.Context, string) error {
		session.state.Exists = false
		return nil
	}
	runDatabase(t, r, db)
	if session.state.Exists {
		t.Fatal("extension child declarations prevented explicit database absence")
	}
	latest := currentDatabase(t, cl, db)
	requireDatabaseCondition(t, latest, metav1.ConditionTrue, testReadyCondition)
	if !latest.Status.HasManagedSQL || latest.Status.AppliedGeneration != latest.Generation {
		t.Fatal("confirmed database absence did not record management and current applied generation")
	}
}

func TestDatabaseExtensionDeclarationsReachCurrentLifecycle(t *testing.T) {
	db := ownershipDatabase(testOrdersNamespace, dbTestApplicationName, dbTestResourceUID)
	db.Spec.Extensions = []epco.ExtensionSpec{{Name: testExtensionName, Schema: testExtensionPublicSchema}}
	r, cl, session := databaseFixture(t, db)
	extensions := &extensionSessionStub{databaseSessionStub: session}
	r.Databases = databaseFactoryFunc(func(context.Context, *postgres.Config) (postgres.DatabaseSession, error) {
		return extensions, nil
	})
	installed := false
	session.apply = func(_ context.Context, desired postgres.DatabaseDesired) (*postgres.DatabaseApplyResult, error) {
		if len(desired.Spec.Extensions) == 1 && desired.Spec.Extensions[0].Name == testExtensionName {
			installed = true
			extensions.states = []postgres.ExtensionState{{Name: testExtensionName, Schema: testExtensionPublicSchema, Exists: true}}
		}
		return &postgres.DatabaseApplyResult{}, nil
	}
	runDatabase(t, r, db)
	if !installed {
		t.Fatal("present extension declaration never reached SQL lifecycle reconciliation")
	}
	requireDatabaseCondition(t, currentDatabase(t, cl, db), metav1.ConditionTrue, testReadyCondition)
}

func TestDatabaseExtensionsFallbackRepairsAdmissionBeforeDeclaredSchema(t *testing.T) {
	const schemaName = "addons"
	db := ownershipDatabase(testOrdersNamespace, dbTestApplicationName, dbTestResourceUID)
	db.Spec.AllowConnections = new(true)
	db.Spec.Schemas = []epco.SchemaSpec{{Name: schemaName}}
	db.Spec.Extensions = []epco.ExtensionSpec{{Name: testExtensionName, Schema: schemaName}}
	r, cl, session := databaseFixture(t, db)
	session.state.AllowConnections = false
	extensions := &extensionSessionStub{databaseSessionStub: session}
	schemaExists := false
	session.inspectSchemas = func(context.Context, string, []epco.SchemaSpec) ([]postgres.SchemaState, error) {
		if !session.state.AllowConnections {
			return nil, &postgres.Failure{Reason: postgres.DependenciesNotReady, Message: "target admission is disabled"}
		}
		return []postgres.SchemaState{{Name: schemaName, Owner: dbTestOwnerName, Exists: schemaExists}}, nil
	}
	session.apply = func(_ context.Context, desired postgres.DatabaseDesired) (*postgres.DatabaseApplyResult, error) {
		if desired.Spec.AllowConnections != nil {
			session.state.AllowConnections = *desired.Spec.AllowConnections
		}
		for _, schema := range desired.Spec.Schemas {
			if schema.Name == schemaName && schema.Ensure != epco.EnsureAbsent {
				schemaExists = true
			}
		}
		for _, extension := range desired.Spec.Extensions {
			if !schemaExists {
				return &postgres.DatabaseApplyResult{BaseManaged: true}, &postgres.Failure{
					Reason: postgres.DependenciesNotReady, Message: "extension target schema does not exist"}
			}
			extensions.states = []postgres.ExtensionState{{Name: string(extension.Name),
				Schema: string(extension.Schema), Exists: true}}
		}
		return &postgres.DatabaseApplyResult{BaseManaged: true}, nil
	}
	// This adapter supports extensions but deliberately has no ACL capability.
	r.Databases = databaseFactoryFunc(func(context.Context, *postgres.Config) (postgres.DatabaseSession, error) {
		return extensions, nil
	})
	runDatabase(t, r, db)
	latest := currentDatabase(t, cl, db)
	requireDatabaseCondition(t, latest, metav1.ConditionTrue, testReadyCondition)
	if !session.state.AllowConnections || !schemaExists || len(extensions.states) != 1 ||
		!extensions.states[0].Exists || extensions.states[0].Schema != schemaName {
		t.Fatal("admission repair did not converge declared schema and installed extension")
	}
	if latest.Status.AppliedGeneration != latest.Generation {
		t.Fatal("confirmed extension lifecycle did not publish the current generation")
	}
}
