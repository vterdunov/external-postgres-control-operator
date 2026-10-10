package controller_test

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"strings"
	"testing"

	epco "github.com/vterdunov/external-postgres-control-operator/api/v1alpha1"
	"github.com/vterdunov/external-postgres-control-operator/internal/controller"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

type connectionWriteFixture struct {
	client     client.WithWatch
	root       client.Object
	secretName string
	create     bool
	remove     bool
	setClient  func(client.Client)
	reconcile  func() (ctrl.Result, error)
	status     func() ([]byte, error)
}

func connectionWriteErrorFixture(t *testing.T, path string) connectionWriteFixture {
	t.Helper()
	if path == connectionOutputCreatePath || path == connectionRetainedOutputUpdatePath || path == connectionOutputOmissionPath {
		db, _, _, reconciler, cl, _ := connectionFixture(t)
		connectionOffline(reconciler)
		if path == connectionRetainedOutputUpdatePath {
			runDatabase(t, reconciler, db)
			if err := cl.Delete(t.Context(), currentDatabase(t, cl, db)); err != nil {
				t.Fatal(err)
			}
			runDatabase(t, reconciler, db)
			recreated := connectionDatabase()
			recreated.UID = "new-adoption-manager-uid"
			if err := cl.Create(t.Context(), recreated); err != nil {
				t.Fatal(err)
			}
			db = recreated
		}
		if path == connectionOutputOmissionPath {
			runDatabase(t, reconciler, db)
			latest := currentDatabase(t, cl, db)
			latest.Spec.ConnectionSecrets = nil
			latest.Generation++
			connectionUpdate(t, cl, latest)
		}
		return connectionWriteFixture{client: cl, root: db, secretName: connectionOutputName,
			create:    path == connectionOutputCreatePath,
			remove:    path == connectionOutputOmissionPath,
			setClient: func(c client.Client) { reconciler.Client = c },
			reconcile: func() (ctrl.Result, error) {
				return reconciler.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(db)})
			},
			status: func() ([]byte, error) { return json.Marshal(currentDatabase(t, cl, db).Status) },
		}
	}
	role := ownershipRole(testOrdersNamespace, "generated-write-error", "current-role-uid")
	role.Spec.Login = true
	role.Spec.PasswordGeneration = &epco.PasswordGeneration{SecretName: testGeneratedName}
	var objects []client.Object
	if path == "retained source UPDATE" {
		source := ownedGeneratedSource(role)
		source.Annotations[controller.GeneratedRoleUIDAnnotation] = "retired-role-uid"
		objects = append(objects, source)
	}
	reconciler, cl, _ := roleFixture(t, role, objects...)
	if path == connectionSourceReclaimPath {
		runRole(t, reconciler, role)
		latest := currentRole(t, cl, role)
		latest.Spec.ReclaimPolicy = epco.ReclaimDelete
		latest.Generation++
		connectionUpdate(t, cl, latest)
		if err := cl.Delete(t.Context(), currentRole(t, cl, role)); err != nil {
			t.Fatal("could not request isolated generated source reclamation")
		}
	}
	return connectionWriteFixture{client: cl, root: role, secretName: testGeneratedName,
		create:    path == "generated source CREATE",
		remove:    path == connectionSourceReclaimPath,
		setClient: func(c client.Client) { reconciler.Client = c },
		reconcile: func() (ctrl.Result, error) {
			return reconciler.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(role)})
		},
		status: func() ([]byte, error) {
			latest := &epco.DatabaseRole{}
			if err := cl.Get(t.Context(), client.ObjectKeyFromObject(role), latest); err != nil {
				if path == connectionSourceReclaimPath && apierrors.IsNotFound(err) {
					return nil, nil
				}
				return nil, err
			}
			return json.Marshal(latest.Status)
		},
	}
}

func connectionEchoedWriteError(kind string, secret *corev1.Secret) error {
	var base error
	switch kind {
	case connectionConflictFault:
		base = apierrors.NewConflict(schema.GroupResource{Resource: testSecretResource}, secret.Name, errors.New("safe conflict"))
	case connectionAlreadyExistsFault:
		base = apierrors.NewAlreadyExists(schema.GroupResource{Resource: testSecretResource}, secret.Name)
	case connectionNotFoundFault:
		base = apierrors.NewNotFound(schema.GroupResource{Resource: testSecretResource}, secret.Name)
	case connectionCanceledFault:
		base = context.Canceled
	case connectionDeadlineFault:
		base = context.DeadlineExceeded
	default:
		base = errors.New("admission rejected this payload")
	}
	// Keep the deliberately unsafe admission echo private to the interceptor.
	echo := errors.New("echoed payload " + string(secret.Data["password"]) + " " + string(secret.Data["uri"]))
	return errors.Join(base, echo)
}

func TestDatabaseConnectionSecretWritesNeverExposeAdmissionEchoes(t *testing.T) {
	for _, path := range []string{connectionOutputCreatePath, connectionRetainedOutputUpdatePath, "generated source CREATE", "retained source UPDATE",
		connectionOutputOmissionPath, connectionSourceReclaimPath} {
		for _, kind := range []string{"admission", connectionConflictFault, connectionAlreadyExistsFault, connectionNotFoundFault, connectionCanceledFault, connectionDeadlineFault} {
			t.Run(path+"/"+kind, func(t *testing.T) {
				fixture := connectionWriteErrorFixture(t, path)
				var sensitive [][]byte
				attempted := false
				interceptWrite := func(object client.Object) (bool, error) {
					secret, ok := object.(*corev1.Secret)
					if !ok || secret.Name != fixture.secretName || secret.Namespace != fixture.root.GetNamespace() {
						return false, nil
					}
					attempted = true
					sensitive = [][]byte{bytes.Clone(secret.Data["password"]), bytes.Clone(secret.Data["uri"])}
					return true, connectionEchoedWriteError(kind, secret)
				}
				fixture.setClient(interceptor.NewClient(fixture.client, interceptor.Funcs{
					Create: func(ctx context.Context, c client.WithWatch, object client.Object, options ...client.CreateOption) error {
						if fixture.create {
							if intercepted, err := interceptWrite(object); intercepted {
								return err
							}
						}
						return c.Create(ctx, object, options...)
					},
					Update: func(ctx context.Context, c client.WithWatch, object client.Object, options ...client.UpdateOption) error {
						if !fixture.create && !fixture.remove {
							if intercepted, err := interceptWrite(object); intercepted {
								return err
							}
						}
						return c.Update(ctx, object, options...)
					},
					Delete: func(ctx context.Context, c client.WithWatch, object client.Object, options ...client.DeleteOption) error {
						if fixture.remove {
							if intercepted, err := interceptWrite(object); intercepted {
								return err
							}
						}
						return c.Delete(ctx, object, options...)
					},
				}))
				result, err := fixture.reconcile()
				if !attempted {
					t.Fatal("test did not reach the declared Secret payload write")
				}
				status, statusErr := fixture.status()
				if statusErr != nil {
					t.Fatal("could not inspect credential-free resource status")
				}
				for _, value := range sensitive {
					if len(value) == 0 {
						continue
					}
					if err != nil && strings.Contains(err.Error(), string(value)) {
						t.Fatal("Secret write returned credential-bearing error text")
					}
					if bytes.Contains(status, value) {
						t.Fatal("Secret write published credential-bearing status")
					}
				}
				connectionAssertWriteClassification(t, kind, fixture, result, err)
			})
		}
	}
}

func connectionAssertWriteClassification(t *testing.T, kind string, fixture connectionWriteFixture, result ctrl.Result, err error) {
	t.Helper()
	switch kind {
	case connectionCanceledFault:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("Secret write lost cancellation classification")
		}
	case connectionDeadlineFault:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal("Secret write lost deadline classification")
		}
	case connectionConflictFault:
		if err != nil && !apierrors.IsConflict(err) {
			t.Fatal("Secret write lost optimistic-conflict classification")
		}
		if err == nil && result.RequeueAfter <= 0 {
			t.Fatal("Secret write lost optimistic-conflict retry")
		}
	case connectionAlreadyExistsFault:
		if err != nil && !apierrors.IsAlreadyExists(err) {
			t.Fatal("Secret write lost deterministic-name conflict classification")
		}
		if err == nil && result.RequeueAfter <= 0 {
			t.Fatal("Secret write lost deterministic-name conflict retry")
		}
	case connectionNotFoundFault:
		if err != nil && !apierrors.IsNotFound(err) {
			t.Fatal("Secret write lost disappearance classification")
		}
		if err == nil && result.RequeueAfter <= 0 && !fixture.remove {
			t.Fatal("Secret disappearance must retry current state")
		}
	default:
		if err == nil && result.RequeueAfter <= 0 {
			t.Fatal("failed admission write must retry or report failure")
		}
	}
}
