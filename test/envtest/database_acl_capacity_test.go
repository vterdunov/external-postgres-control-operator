//go:build envtest

package envtest_test

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	epco "github.com/vterdunov/external-postgres-control-operator/api/v1alpha1"
	"github.com/vterdunov/external-postgres-control-operator/internal/controller"
	"github.com/vterdunov/external-postgres-control-operator/internal/postgres"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	capacityRelationCount = 1000
	capacityReader        = "capacity_reader_role_0123456789"
	capacitySchema        = "app"
	capacityScope         = "tables"
	capacityClass         = "table"
	capacityPrivilege     = "SELECT"
)

// SQL behavior is already tested independently. This optional adapter models a
// legal catalog expansion and its confirmed outcomes while retaining the real
// controller, API status subresource, serialization and checkpoint protocol.
type capacityACLFactory struct {
	databaseWatchFactory
	confirmed atomic.Int32
}

type capacityACLSession struct {
	postgres.DatabaseSession
	owner *capacityACLFactory
}

func (f *capacityACLFactory) Open(ctx context.Context, config *postgres.Config) (postgres.DatabaseSession, error) {
	session, err := f.databaseWatchFactory.Open(ctx, config)
	if err != nil {
		return nil, err
	}
	return &capacityACLSession{DatabaseSession: session, owner: f}, nil
}

func (*capacityACLSession) PreflightACL(context.Context, postgres.DatabaseDesired) error { return nil }
func (*capacityACLSession) PrepareACL(_ context.Context, desired postgres.ACLDesired) (*postgres.ACLResult, error) {
	return &postgres.ACLResult{Targets: desired.Targets}, nil
}

func capacityACLResult(desired postgres.ACLDesired) *postgres.ACLResult {
	target := desired.Target
	target.BindingRevision = desired.BindingRevision
	target.Bindings = make([]epco.ACLBinding, 0, capacityRelationCount)
	outcomes := make([]epco.PermissionStatus, 0, capacityRelationCount)
	declaration := fmt.Sprintf("%q/%q/%q/%q/%q/%q/%q",
		capacityScope, capacityClass, capacitySchema, "", "", capacityPrivilege, capacityReader)
	for i := range capacityRelationCount {
		name := fmt.Sprintf("relation_%04d_", i) + strings.Repeat("x", 49)
		target.Bindings = append(target.Bindings, epco.ACLBinding{
			Declaration: declaration, Scope: capacityScope, Class: capacityClass, Schema: capacitySchema,
			Table: name, Privilege: capacityPrivilege, Grantee: capacityReader,
			Owner: databaseTestOwnerName, Grantor: databaseTestOwnerName,
			DatabaseOID: 20000, SchemaOID: 20001, ObjectOID: uint32(30000 + i),
			GranteeOID: 20002, OwnerOID: 20003, GrantorOID: 20003,
			BindingRevision: desired.BindingRevision, Generation: desired.Generation,
			Before: []epco.ACLGrantIdentity{{Grantor: epco.ACLRoleIdentity{Name: databaseTestOwnerName, OID: 20003}}},
		})
		outcomes = append(outcomes, epco.PermissionStatus{
			Scope: capacityScope, Class: capacityClass, Schema: capacitySchema, Table: name,
			Grantee: capacityReader, Privilege: capacityPrivilege, Applied: true, Effective: new(true),
			Message: "The declared direct ACL layer was confirmed",
		})
	}
	return &postgres.ACLResult{Targets: []epco.ManagedACLTarget{target}, Permissions: outcomes}
}

func (s *capacityACLSession) ReconcileACL(ctx context.Context,
	desired postgres.ACLDesired) (*postgres.ACLResult, error) {
	result := capacityACLResult(desired)
	if err := desired.Checkpoint(ctx, result.Targets); err != nil {
		return result, err
	}
	s.owner.confirmed.Add(1)
	return result, nil
}

func capacityResourceSize(database *epco.Database) int {
	encoded, err := json.Marshal(database)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	return len(encoded)
}

var _ = Describe("R3 ACL bounded outcome reporting", func() {
	It("keeps complete durable history and stable Ready when optional reports exceed the resource budget",
		func(ctx SpecContext) {
			cluster := createRoleDescriptor(ctx, nil)
			database := watchedDatabase(cluster, "capacity-report")
			database.Spec.Schemas = []epco.SchemaSpec{{Name: capacitySchema, Permissions: &epco.SchemaPermissions{
				Tables: &epco.TablePermissions{Select: []epco.Permission{{Name: capacityReader}}}}}}
			Expect(scope.Create(ctx, database)).To(Succeed())
			builder, err := postgres.NewConfigBuilder()
			Expect(err).NotTo(HaveOccurred())
			factory := &capacityACLFactory{}
			reconciler := &controller.DatabaseReconciler{Client: environment.Client, APIReader: environment.Client,
				Builder: builder, Databases: factory, Locks: &controller.ManagementLocks{}}
			fixture := database.DeepCopy()
			result := capacityACLResult(postgres.ACLDesired{
				Target:          epco.ManagedACLTarget{Host: "capacity.example.test", Port: 5432, Database: database.Name},
				BindingRevision: strings.Repeat("e", 64), Generation: database.Generation})
			fixture.Status.ManagedACLTargets = result.Targets
			Expect(capacityResourceSize(fixture)).To(BeNumerically("<", controller.DatabaseACLStatusByteLimit-8192),
				"the complete journal fits with checkpoint headroom")
			fixture.Status.Permissions = result.Permissions
			Expect(capacityResourceSize(fixture)).To(BeNumerically(">", controller.DatabaseACLStatusByteLimit+8192),
				"optional detailed reporting, rather than durable cleanup history, creates the capacity boundary")
			request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(database)}
			Eventually(func(g Gomega) {
				_, reconcileErr := reconciler.Reconcile(ctx, request)
				g.Expect(reconcileErr).NotTo(HaveOccurred())
				g.Expect(factory.confirmed.Load()).To(BeNumerically(">", 0))
			}).WithContext(ctx).WithTimeout(10 * time.Second).Should(Succeed())
			for range 3 {
				current := &epco.Database{}
				Expect(environment.Client.Get(ctx, request.NamespacedName, current)).To(Succeed())
				condition := meta.FindStatusCondition(current.Status.Conditions, databaseTestReadyCondition)
				Expect(condition).NotTo(BeNil())
				Expect(condition.Status).To(Equal(metav1.ConditionTrue))
				Expect(condition.ObservedGeneration).To(Equal(current.Generation))
				Expect(current.Status.AppliedGeneration).To(Equal(current.Generation))
				Expect(current.Status.ManagedACLTargets).To(HaveLen(1))
				Expect(current.Status.ManagedACLTargets[0].Bindings).To(HaveLen(capacityRelationCount),
					"bounded reporting must not truncate cleanup responsibility")
				Expect(capacityResourceSize(current)).To(BeNumerically("<=", controller.DatabaseACLStatusByteLimit),
					"the confirmed whole resource must respect its declared capacity rather than fail on its next retry")
				if len(current.Status.Permissions) < capacityRelationCount {
					raw, convertErr := runtime.DefaultUnstructuredConverter.ToUnstructured(current)
					Expect(convertErr).NotTo(HaveOccurred())
					omitted, found, nestedErr := unstructured.NestedInt64(raw, "status", "permissionsOmitted")
					Expect(nestedErr).NotTo(HaveOccurred())
					Expect(found).To(BeTrue(), "bounded reports must disclose the exact omitted outcome count")
					Expect(omitted).To(Equal(int64(capacityRelationCount - len(current.Status.Permissions))))
				}
				_, err = reconciler.Reconcile(ctx, request)
				Expect(err).NotTo(HaveOccurred())
			}
		}, NodeTimeout(30*time.Second))
})
