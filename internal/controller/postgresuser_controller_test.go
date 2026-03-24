package controller

import (
	"context"
	"fmt"

	"github.com/go-logr/logr"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"go.uber.org/mock/gomock"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	dbv1alpha1 "github.com/movetokube/postgres-operator/api/v1alpha1"
	mockpg "github.com/movetokube/postgres-operator/pkg/postgres/mock"
	"github.com/movetokube/postgres-operator/pkg/utils"
)

var _ = Describe("PostgresUser Controller", func() {
	const (
		name         = "test-user"
		namespace    = "operator"
		databaseName = "test-db"
		secretName   = "db-credentials"
		roleName     = "app"
		roleAws      = "rds_iam"
	)

	var (
		sc       *runtime.Scheme
		req      reconcile.Request
		mockCtrl *gomock.Controller
		pg       *mockpg.MockPG
		rp       *PostgresUserReconciler
		cl       client.Client
	)

	initClient := func(postgres *dbv1alpha1.Postgres, user *dbv1alpha1.PostgresUser, markAsDeleted bool) {
		if postgres != nil {
			pgStatusCopy := postgres.Status.DeepCopy()
			Expect(cl.Create(ctx, postgres)).To(BeNil())
			pgStatusCopy.DeepCopyInto(&postgres.Status)
			Expect(cl.Status().Update(ctx, postgres)).To(BeNil())
		}

		if user != nil {
			userStatusCopy := user.Status.DeepCopy()
			if markAsDeleted {
				user.SetFinalizers([]string{"finalizer.db.movetokube.com"})
			}
			Expect(cl.Create(ctx, user)).To(BeNil())
			userStatusCopy.DeepCopyInto(&user.Status)
			Expect(cl.Status().Update(ctx, user)).To(BeNil())
			if markAsDeleted {
				Expect(cl.Delete(ctx, user, &client.DeleteOptions{GracePeriodSeconds: new(int64)})).To(BeNil())
			}
		}
	}

	runReconcile := func(rp *PostgresUserReconciler, ctx context.Context, req reconcile.Request) (err error) {
		_, err = rp.Reconcile(ctx, req)
		if k8sManager != nil {
			k8sManager.GetCache().WaitForCacheSync(ctx)
		}
		return err
	}

	clearUsers := func(namespace string) error {
		l := dbv1alpha1.PostgresUserList{}
		err := k8sClient.List(ctx, &l, client.InNamespace(namespace))
		Expect(err).ToNot(HaveOccurred())
		for _, el := range l.Items {
			org := el.DeepCopy()
			el.SetFinalizers(nil)
			err = k8sClient.Patch(ctx, &el, client.MergeFrom(org))
			if err != nil {
				return err
			}
		}
		return k8sClient.DeleteAllOf(ctx, &dbv1alpha1.PostgresUser{}, client.InNamespace(namespace))
	}

	BeforeEach(func() {
		// Gomock
		mockCtrl = gomock.NewController(GinkgoT())
		pg = mockpg.NewMockPG(mockCtrl)
		cl = k8sClient
		// Create runtime scheme
		sc = scheme.Scheme
		sc.AddKnownTypes(dbv1alpha1.GroupVersion, &dbv1alpha1.Postgres{})
		sc.AddKnownTypes(dbv1alpha1.GroupVersion, &dbv1alpha1.PostgresList{})
		sc.AddKnownTypes(dbv1alpha1.GroupVersion, &dbv1alpha1.PostgresUser{})
		sc.AddKnownTypes(dbv1alpha1.GroupVersion, &dbv1alpha1.PostgresUserList{})
		// Create PostgresUserReconciler
		rp = &PostgresUserReconciler{
			Client:        managerClient,
			Scheme:        sc,
			pg:            pg,
			pgHost:        "postgres.local",
			cloudProvider: "AWS",
		}
		if k8sManager != nil {
			rp.SetupWithManager(k8sManager)
		}
		// Create mock reconcile request
		req = reconcile.Request{
			NamespacedName: types.NamespacedName{
				Name:      name,
				Namespace: namespace,
			},
		}
	})

	AfterEach(func() {
		Expect(clearPgs(namespace)).To(BeNil())
		Expect(clearUsers(namespace)).To(BeNil())
		if k8sManager != nil {
			k8sManager.GetCache().WaitForCacheSync(ctx)
		}
		mockCtrl.Finish()
	})

	It("should not requeue if PostgresUser does not exist", func() {
		// Call Reconcile
		res, err := rp.Reconcile(ctx, req)
		// No error should be returned
		Expect(err).NotTo(HaveOccurred())
		// Request should not be requeued
		Expect(res.RequeueAfter).To(BeZero())
	})

	Describe("Checking deletion logic", func() {
		var (
			postgresDB   *dbv1alpha1.Postgres
			postgresUser *dbv1alpha1.PostgresUser
		)

		BeforeEach(func() {
			postgresDB = &dbv1alpha1.Postgres{
				ObjectMeta: metav1.ObjectMeta{
					Name:      databaseName,
					Namespace: namespace,
				},
				Spec: dbv1alpha1.PostgresSpec{
					Database: databaseName,
				},
				Status: dbv1alpha1.PostgresStatus{
					Succeeded: true,
					Roles: dbv1alpha1.PostgresRoles{
						Owner:  databaseName + "-group",
						Reader: databaseName + "-reader",
						Writer: databaseName + "-writer",
					},
				},
			}

			postgresUser = &dbv1alpha1.PostgresUser{
				ObjectMeta: metav1.ObjectMeta{
					Name:      name,
					Namespace: namespace,
				},
				Spec: dbv1alpha1.PostgresUserSpec{
					Database:   databaseName,
					SecretName: secretName,
					Role:       roleName,
					Privileges: "WRITE",
				},
				Status: dbv1alpha1.PostgresUserStatus{
					Succeeded:     true,
					PostgresGroup: databaseName + "-writer",
					PostgresRole:  "mockuser",
					DatabaseName:  databaseName,
				},
			}
		})

		Context("User deletion with dropOnDelete unset (default)", func() {
			BeforeEach(func() {
				initClient(postgresDB, postgresUser, true)
			})

			It("should remove finalizer without dropping the role", func() {
				// Call Reconcile
				err := runReconcile(rp, ctx, req)
				Expect(err).NotTo(HaveOccurred())

				// Check if PostgresUser was properly deleted
				foundUser := &dbv1alpha1.PostgresUser{}
				err = cl.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, foundUser)
				if err != nil {
					Expect(errors.IsNotFound(err)).To(BeTrue())
				} else {
					Expect(foundUser.GetFinalizers()).To(BeEmpty())
				}
			})
		})

		Context("User deletion with dropOnDelete enabled", func() {
			BeforeEach(func() {
				postgresUser.Spec.DropOnDelete = true
				initClient(postgresDB, postgresUser, true)
			})

			It("should drop the role and remove finalizer", func() {
				// Expect DropRole to be called
				pg.EXPECT().GetDefaultDatabase().Return("postgres")
				pg.EXPECT().DropRole(postgresUser.Status.PostgresRole, postgresUser.Status.PostgresGroup,
					databaseName).Return(nil)

				// Call Reconcile
				err := runReconcile(rp, ctx, req)
				Expect(err).NotTo(HaveOccurred())

				// Check if PostgresUser was properly deleted
				foundUser := &dbv1alpha1.PostgresUser{}
				err = cl.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, foundUser)
				if err != nil {
					Expect(errors.IsNotFound(err)).To(BeTrue())
				} else {
					Expect(foundUser.GetFinalizers()).To(BeEmpty())
				}
			})

			It("should return an error if role dropping fails", func() {
				// Expect DropRole to fail
				pg.EXPECT().GetDefaultDatabase().Return("postgres")
				pg.EXPECT().DropRole(postgresUser.Status.PostgresRole, postgresUser.Status.PostgresGroup,
					databaseName).Return(fmt.Errorf("failed to drop role"))
				// Call Reconcile
				err := runReconcile(rp, ctx, req)
				Expect(err).To(HaveOccurred())

				// Check if PostgresUser still has finalizer
				foundUser := &dbv1alpha1.PostgresUser{}
				err = cl.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, foundUser)
				Expect(err).NotTo(HaveOccurred())
				Expect(foundUser.GetFinalizers()).NotTo(BeEmpty())
			})
		})
	})

	Describe("Checking creation logic", func() {
		var (
			postgresDB   *dbv1alpha1.Postgres
			postgresUser *dbv1alpha1.PostgresUser
		)

		BeforeEach(func() {
			postgresDB = &dbv1alpha1.Postgres{
				ObjectMeta: metav1.ObjectMeta{
					Name:      databaseName,
					Namespace: namespace,
				},
				Spec: dbv1alpha1.PostgresSpec{
					Database: databaseName,
				},
				Status: dbv1alpha1.PostgresStatus{
					Succeeded: true,
					Roles: dbv1alpha1.PostgresRoles{
						Owner:  databaseName + "-group",
						Reader: databaseName + "-reader",
						Writer: databaseName + "-writer",
					},
				},
			}

			postgresUser = &dbv1alpha1.PostgresUser{
				ObjectMeta: metav1.ObjectMeta{
					Name:      name,
					Namespace: namespace,
				},
				Spec: dbv1alpha1.PostgresUserSpec{
					Database:   databaseName,
					SecretName: secretName,
					Role:       roleName,
					Privileges: "WRITE",
				},
			}
		})

		Context("New PostgresUser creation", func() {
			BeforeEach(func() {
				// Create database but not the user yet
				initClient(postgresDB, nil, false)

				// Do not create the user yet, the reconciler will do it
				Expect(cl.Create(ctx, postgresUser)).To(Succeed())
			})

			AfterEach(func() {
				// Clean up any created secrets
				secretList := &corev1.SecretList{}
				Expect(cl.List(ctx, secretList, client.InNamespace(namespace))).To(Succeed())
				for _, secret := range secretList.Items {
					Expect(cl.Delete(ctx, &secret)).To(Succeed())
				}
			})

			It("should create user role, grant privileges, and create a secret", func() {
				var capturedRole string
				// Mock expected calls
				pg.EXPECT().GetDefaultDatabase().Return("postgres").AnyTimes()
				pg.EXPECT().CreateUserRole(gomock.Any(), gomock.Any()).DoAndReturn(
					func(role, password string) (string, error) {
						Expect(role).To(HavePrefix(roleName + "-"))
						capturedRole = role
						return role, nil
					})
				pg.EXPECT().GrantRole(databaseName+"-writer", gomock.Any()).DoAndReturn(
					func(groupRole, role string) error {
						Expect(role).To(Equal(capturedRole))
						return nil
					})
				pg.EXPECT().AlterDefaultLoginRole(gomock.Any(), gomock.Any()).DoAndReturn(
					func(role, groupRole string) error {
						Expect(role).To(Equal(capturedRole))
						Expect(groupRole).To(Equal(databaseName + "-writer"))
						return nil
					})

				// Call Reconcile
				err := runReconcile(rp, ctx, req)
				Expect(err).NotTo(HaveOccurred())

				// Check if PostgresUser status was properly updated
				foundUser := &dbv1alpha1.PostgresUser{}
				err = cl.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, foundUser)
				Expect(err).NotTo(HaveOccurred())
				Expect(foundUser.Status.Succeeded).To(BeTrue())
				Expect(foundUser.Status.PostgresRole).To(HavePrefix(roleName + "-"))
				Expect(foundUser.Status.PostgresGroup).To(Equal(databaseName + "-writer"))
				Expect(foundUser.Status.DatabaseName).To(Equal(databaseName))

				// Check if secret was created
				foundSecret := &corev1.Secret{}
				err = cl.Get(ctx, types.NamespacedName{Name: secretName, Namespace: namespace}, foundSecret)
				Expect(err).NotTo(HaveOccurred())
				Expect(foundSecret.Data).To(HaveKey("DATABASE_NAME"))
				Expect(foundSecret.Data).To(HaveKey("HOST"))
				Expect(foundSecret.Data).To(HaveKey("LOGIN"))
				Expect(foundSecret.Data).To(HaveKey("PASSWORD"))
				Expect(foundSecret.Data).To(HaveKey("POSTGRES_DOTNET_URL"))
				Expect(foundSecret.Data).To(HaveKey("POSTGRES_JDBC_URL"))
				Expect(foundSecret.Data).To(HaveKey("POSTGRES_URL"))
				Expect(foundSecret.Data).To(HaveKey("ROLE"))
				Expect(foundSecret.Data).To(HaveKey("HOSTNAME"))
				Expect(foundSecret.Data).To(HaveKey("PORT"))

				Expect(foundSecret.GetOwnerReferences()).To(BeEmpty())
			})

			It("should fail if the database does not exist", func() {
				// Delete the postgres DB
				Expect(cl.Delete(ctx, postgresDB)).To(Succeed())

				// Set up a new PostgresUser with a non-existent database
				nonExistentUser := &dbv1alpha1.PostgresUser{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "nonexistent-user",
						Namespace: namespace,
					},
					Spec: dbv1alpha1.PostgresUserSpec{
						Database:   "nonexistent-db",
						SecretName: secretName,
						Role:       roleName,
						Privileges: "WRITE",
					},
				}
				Expect(cl.Create(ctx, nonExistentUser)).To(Succeed())

				// Call Reconcile
				req := reconcile.Request{
					NamespacedName: types.NamespacedName{
						Name:      "nonexistent-user",
						Namespace: namespace,
					},
				}
				_, err := rp.Reconcile(ctx, req)
				Expect(err).To(HaveOccurred())

				// Check status
				foundUser := &dbv1alpha1.PostgresUser{}
				err = cl.Get(ctx, types.NamespacedName{Name: "nonexistent-user", Namespace: namespace}, foundUser)
				Expect(err).NotTo(HaveOccurred())
				Expect(foundUser.Status.Succeeded).To(BeFalse())
			})

			It("should set finalizer before side-effects when CreateUserRole fails", func() {
				pg.EXPECT().CreateUserRole(gomock.Any(), gomock.Any()).Return("", fmt.Errorf("could not create user role"))

				err := runReconcile(rp, ctx, req)
				Expect(err).To(HaveOccurred())

				foundUser := &dbv1alpha1.PostgresUser{}
				Expect(cl.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, foundUser)).NotTo(HaveOccurred())
				Expect(foundUser.GetFinalizers()).To(ContainElement("finalizer.db.movetokube.com"))
			})
		})

		Context("New PostgresUser creation with dropOnDelete enabled", func() {
			BeforeEach(func() {
				initClient(postgresDB, nil, false)

				postgresUser.Spec.DropOnDelete = true
				Expect(cl.Create(ctx, postgresUser)).To(Succeed())
			})

			AfterEach(func() {
				secretList := &corev1.SecretList{}
				Expect(cl.List(ctx, secretList, client.InNamespace(namespace))).To(Succeed())
				for _, secret := range secretList.Items {
					Expect(cl.Delete(ctx, &secret)).To(Succeed())
				}
			})

			It("should set owner reference on the secret", func() {
				pg.EXPECT().GetDefaultDatabase().Return("postgres").AnyTimes()
				pg.EXPECT().CreateUserRole(gomock.Any(), gomock.Any()).Return(roleName+"-mock", nil)
				pg.EXPECT().GrantRole(databaseName+"-writer", gomock.Any()).Return(nil)
				pg.EXPECT().AlterDefaultLoginRole(gomock.Any(), gomock.Any()).Return(nil)

				err := runReconcile(rp, ctx, req)
				Expect(err).NotTo(HaveOccurred())

				foundSecret := &corev1.Secret{}
				err = cl.Get(ctx, types.NamespacedName{Name: secretName, Namespace: namespace}, foundSecret)
				Expect(err).NotTo(HaveOccurred())
				Expect(foundSecret.GetOwnerReferences()).NotTo(BeEmpty())
				Expect(foundSecret.GetOwnerReferences()[0].Name).To(Equal(name))
			})
		})

		Context("Existing Secret has stale data", func() {
			var (
				existingPassword string
				existingRole     string
				existingLogin    string
			)

			BeforeEach(func() {
				existingPassword = "existing-password"
				existingRole = "app-abc123"
				existingLogin = "app-abc123"

				postgresUser.Status = dbv1alpha1.PostgresUserStatus{
					Succeeded:     true,
					PostgresGroup: databaseName + "-writer",
					PostgresRole:  existingRole,
					PostgresLogin: existingLogin,
					DatabaseName:  databaseName,
				}

				initClient(postgresDB, postgresUser, false)

				desiredSecret, err := rp.newSecretForCR(logr.Discard(), postgresUser, existingRole, existingPassword, existingLogin)
				Expect(err).NotTo(HaveOccurred())
				desiredSecret.Data["HOST"] = []byte("wrong-host")
				desiredSecret.Data["DATABASE_NAME"] = []byte("wrong-db")
				Expect(cl.Create(ctx, desiredSecret)).To(Succeed())
			})

			AfterEach(func() {
				secretList := &corev1.SecretList{}
				Expect(cl.List(ctx, secretList, client.InNamespace(namespace))).To(Succeed())
				for _, secret := range secretList.Items {
					Expect(cl.Delete(ctx, &secret)).To(Succeed())
				}
			})

			It("should update stale Secret data and preserve PASSWORD", func() {
				err := runReconcile(rp, ctx, req)
				Expect(err).NotTo(HaveOccurred())

				foundSecret := &corev1.Secret{}
				err = cl.Get(ctx, types.NamespacedName{Name: secretName, Namespace: namespace}, foundSecret)
				Expect(err).NotTo(HaveOccurred())
				Expect(string(foundSecret.Data["HOST"])).To(Equal("postgres.local"))
				Expect(string(foundSecret.Data["DATABASE_NAME"])).To(Equal(databaseName))
				Expect(string(foundSecret.Data["PASSWORD"])).To(Equal(existingPassword))
			})
		})

		Context("Existing Secret is up to date", func() {
			var (
				existingPassword string
				existingRole     string
				existingLogin    string
			)

			BeforeEach(func() {
				existingPassword = "existing-password"
				existingRole = "app-abc123"
				existingLogin = "app-abc123"

				postgresUser.Status = dbv1alpha1.PostgresUserStatus{
					Succeeded:     true,
					PostgresGroup: databaseName + "-writer",
					PostgresRole:  existingRole,
					PostgresLogin: existingLogin,
					DatabaseName:  databaseName,
				}

				initClient(postgresDB, postgresUser, false)

				desiredSecret, err := rp.newSecretForCR(logr.Discard(), postgresUser, existingRole, existingPassword, existingLogin)
				Expect(err).NotTo(HaveOccurred())
				Expect(cl.Create(ctx, desiredSecret)).To(Succeed())
			})

			AfterEach(func() {
				secretList := &corev1.SecretList{}
				Expect(cl.List(ctx, secretList, client.InNamespace(namespace))).To(Succeed())
				for _, secret := range secretList.Items {
					Expect(cl.Delete(ctx, &secret)).To(Succeed())
				}
			})

			It("should reconcile idempotently when Secret data already matches", func() {
				err := runReconcile(rp, ctx, req)
				Expect(err).NotTo(HaveOccurred())

				foundSecret := &corev1.Secret{}
				err = cl.Get(ctx, types.NamespacedName{Name: secretName, Namespace: namespace}, foundSecret)
				Expect(err).NotTo(HaveOccurred())
				resourceVersionAfterFirstReconcile := foundSecret.ResourceVersion

				err = runReconcile(rp, ctx, req)
				Expect(err).NotTo(HaveOccurred())

				err = cl.Get(ctx, types.NamespacedName{Name: secretName, Namespace: namespace}, foundSecret)
				Expect(err).NotTo(HaveOccurred())
				Expect(foundSecret.ResourceVersion).To(Equal(resourceVersionAfterFirstReconcile))
				Expect(string(foundSecret.Data["PASSWORD"])).To(Equal(existingPassword))
			})
		})

		Context("Instance filter", func() {
			BeforeEach(func() {
				// Set up annotated resources
				postgresDBWithAnnotation := postgresDB.DeepCopy()
				postgresDBWithAnnotation.Annotations = map[string]string{
					utils.INSTANCE_ANNOTATION: "my-instance",
				}

				postgresUserWithAnnotation := postgresUser.DeepCopy()
				postgresUserWithAnnotation.Annotations = map[string]string{
					utils.INSTANCE_ANNOTATION: "my-instance",
				}

				initClient(postgresDBWithAnnotation, postgresUserWithAnnotation, false)

				// Set up the reconciler with instance filter
				rp.instanceFilter = "my-instance"
			})
			AfterEach(func() {
				// Clean up any created secrets
				secretList := &corev1.SecretList{}
				Expect(cl.List(ctx, secretList, client.InNamespace(namespace))).To(Succeed())
				for _, secret := range secretList.Items {
					Expect(cl.Delete(ctx, &secret)).To(Succeed())
				}
			})

			It("should process users with matching instance annotation", func() {
				// Mock expected calls for a successful reconciliation
				pg.EXPECT().GetDefaultDatabase().Return("postgres").AnyTimes()
				pg.EXPECT().CreateUserRole(gomock.Any(), gomock.Any()).Return(roleName+"-mockrole", nil)
				pg.EXPECT().GrantRole(gomock.Any(), gomock.Any()).Return(nil)
				pg.EXPECT().AlterDefaultLoginRole(gomock.Any(), gomock.Any()).Return(nil)

				// Call Reconcile
				err := runReconcile(rp, ctx, req)
				Expect(err).NotTo(HaveOccurred())
			})

			It("should not process users with non-matching instance annotation", func() {
				// Create a user with different annotation
				userWithDifferentAnnotation := postgresUser.DeepCopy()
				userWithDifferentAnnotation.Name = "different-annotation-user"
				userWithDifferentAnnotation.Annotations = map[string]string{
					utils.INSTANCE_ANNOTATION: "different-instance",
				}
				Expect(cl.Create(ctx, userWithDifferentAnnotation)).To(Succeed())

				// Call Reconcile with the different user
				reqDifferent := reconcile.Request{
					NamespacedName: types.NamespacedName{
						Name:      "different-annotation-user",
						Namespace: namespace,
					},
				}
				err := runReconcile(rp, ctx, reqDifferent)
				Expect(err).NotTo(HaveOccurred())

				// Verify that the user wasn't processed (status.PostgresRole should be empty)
				foundUser := &dbv1alpha1.PostgresUser{}
				err = cl.Get(ctx, types.NamespacedName{Name: "different-annotation-user", Namespace: namespace}, foundUser)
				Expect(err).NotTo(HaveOccurred())
				Expect(foundUser.Status.PostgresRole).To(Equal(""))
			})
		})

		Context("Secret template", func() {
			BeforeEach(func() {
				userWithTemplate := postgresUser.DeepCopy()
				userWithTemplate.Spec.SecretTemplate = map[string]string{
					"CUSTOM_KEY":                "User: {{.Role}}, DB: {{.Database}}",
					"PGPASSWORD":                "{{.Password}}",
					"URIARGSFILTER":             `postgres://foobar?{{ "sslmode=no-verify" | mergeUriArgs }}`,
					"URIARGSFILTER_COMBINED":    `postgres://foobar?{{ "logging=true" | mergeUriArgs }}`,
					"URIARGSFILTER_EMPTYSTRING": `postgres://foobar?{{ "" | mergeUriArgs }}`,
				}
				initClient(postgresDB, userWithTemplate, false)
			})

			AfterEach(func() {
				// Clean up any created secrets
				secretList := &corev1.SecretList{}
				Expect(cl.List(ctx, secretList, client.InNamespace(namespace))).To(Succeed())
				for _, secret := range secretList.Items {
					Expect(cl.Delete(ctx, &secret)).To(Succeed())
				}
			})

			It("should render templates in the secret", func() {
				// Mock expected calls
				pg.EXPECT().GetDefaultDatabase().Return("postgres").AnyTimes()
				pg.EXPECT().CreateUserRole(gomock.Any(), gomock.Any()).Return("app-mockedRole", nil)
				pg.EXPECT().GrantRole(gomock.Any(), gomock.Any()).Return(nil)
				pg.EXPECT().AlterDefaultLoginRole(gomock.Any(), gomock.Any()).Return(nil)

				rp.pgUriArgs = "sslmode=disable"

				// Call Reconcile
				err := runReconcile(rp, ctx, req)
				Expect(err).NotTo(HaveOccurred())

				// Let's update the user status manually to mark it as succeeded
				// This should trigger creation of the secret with templates in our second reconcile
				foundUser := &dbv1alpha1.PostgresUser{}
				err = cl.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, foundUser)
				Expect(err).NotTo(HaveOccurred())

				// Set the status to succeeded
				foundUser.Status.Succeeded = true
				err = cl.Status().Update(ctx, foundUser)
				Expect(err).NotTo(HaveOccurred())

				// Run another reconcile which should update the secret with the correct templates
				err = runReconcile(rp, ctx, req)
				Expect(err).NotTo(HaveOccurred())

				// Now check if the secret was created with the templated values
				foundSecret := &corev1.Secret{}
				GinkgoWriter.Printf("Getting secret %s\n", secretName)
				err = cl.Get(ctx, types.NamespacedName{Name: secretName, Namespace: namespace}, foundSecret)
				Expect(err).NotTo(HaveOccurred())

				Expect(foundSecret.Data).To(HaveLen(5), "secret should contain only the 5 template keys, no defaults")

				Expect(foundSecret.Data).To(HaveKey("CUSTOM_KEY"))
				customKey := string(foundSecret.Data["CUSTOM_KEY"])
				Expect(customKey).To(ContainSubstring("DB: " + databaseName))

				Expect(foundSecret.Data).To(HaveKey("PGPASSWORD"))
				Expect(string(foundSecret.Data["PGPASSWORD"])).NotTo(BeEmpty())

				Expect(foundSecret.Data).To(HaveKey("URIARGSFILTER"))
				Expect(string(foundSecret.Data["URIARGSFILTER"])).To(Equal("postgres://foobar?sslmode=disable"))

				Expect(foundSecret.Data).To(HaveKey("URIARGSFILTER_EMPTYSTRING"))
				Expect(string(foundSecret.Data["URIARGSFILTER_EMPTYSTRING"])).To(Equal("postgres://foobar?sslmode=disable"))

				Expect(foundSecret.Data).To(HaveKey("URIARGSFILTER_COMBINED"))
				Expect(string(foundSecret.Data["URIARGSFILTER_COMBINED"])).To(Equal("postgres://foobar?logging=true&sslmode=disable"))

				Expect(foundSecret.Data).NotTo(HaveKey("POSTGRES_URL"), "default keys must not be present when secretTemplate is set")
				Expect(foundSecret.Data).NotTo(HaveKey("ROLE"), "default keys must not be present when secretTemplate is set")
				Expect(foundSecret.Data).NotTo(HaveKey("HOST"), "default keys must not be present when secretTemplate is set")

			})
		})
	})

	Context("IAM authentication", func() {
		var (
			postgresDB   *dbv1alpha1.Postgres
			postgresUser *dbv1alpha1.PostgresUser
		)

		BeforeEach(func() {
			postgresDB = &dbv1alpha1.Postgres{
				ObjectMeta: metav1.ObjectMeta{
					Name:      databaseName,
					Namespace: namespace,
				},
				Spec: dbv1alpha1.PostgresSpec{Database: databaseName},
				Status: dbv1alpha1.PostgresStatus{
					Succeeded: true,
					Roles: dbv1alpha1.PostgresRoles{
						Owner:  databaseName + "-group",
						Reader: databaseName + "-reader",
						Writer: databaseName + "-writer",
					},
				},
			}

			postgresUser = &dbv1alpha1.PostgresUser{
				ObjectMeta: metav1.ObjectMeta{
					Name:      name,
					Namespace: namespace,
				},
				Spec: dbv1alpha1.PostgresUserSpec{
					Database:   databaseName,
					SecretName: secretName,
					Role:       roleName,
					Privileges: "WRITE",
				},
			}
		})

		AfterEach(func() {
			// Clean up any created secrets
			secretList := &corev1.SecretList{}
			Expect(cl.List(ctx, secretList, client.InNamespace(namespace))).To(Succeed())
			for _, secret := range secretList.Items {
				Expect(cl.Delete(ctx, &secret)).To(Succeed())
			}
		})

		It("grants rds_iam role when enableIamAuth is true", func() {
			// Create DB and user with IAM enabled
			initClient(postgresDB, nil, false)
			user := postgresUser.DeepCopy()
			user.Spec.AWS = &dbv1alpha1.PostgresUserAWSSpec{EnableIamAuth: true}
			Expect(cl.Create(ctx, user)).To(Succeed())

			var capturedRole string
			pg.EXPECT().GetDefaultDatabase().Return("postgres").AnyTimes()
			pg.EXPECT().CreateUserRole(gomock.Any(), gomock.Any()).DoAndReturn(
				func(role, password string) (string, error) {
					Expect(role).To(HavePrefix(roleName + "-"))
					capturedRole = role
					return role, nil
				})
			pg.EXPECT().GrantRole(databaseName+"-writer", gomock.Any()).Return(nil)
			pg.EXPECT().AlterDefaultLoginRole(gomock.Any(), gomock.Any()).Return(nil)
			pg.EXPECT().GrantRole(roleAws, gomock.Any()).DoAndReturn(
				func(role, grantee string) error {
					Expect(role).To(Equal(roleAws))
					Expect(grantee).To(Equal(capturedRole))
					return nil
				})

			err := runReconcile(rp, ctx, req)
			Expect(err).NotTo(HaveOccurred())

			foundUser := &dbv1alpha1.PostgresUser{}
			err = cl.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, foundUser)
			Expect(err).NotTo(HaveOccurred())
			Expect(foundUser.Status.EnableIamAuth).To(BeTrue())
		})

		It("does not flip status on grant error", func() {
			initClient(postgresDB, nil, false)
			user := postgresUser.DeepCopy()
			user.Spec.AWS = &dbv1alpha1.PostgresUserAWSSpec{EnableIamAuth: true}
			Expect(cl.Create(ctx, user)).To(Succeed())

			pg.EXPECT().GetDefaultDatabase().Return("postgres").AnyTimes()
			pg.EXPECT().CreateUserRole(gomock.Any(), gomock.Any()).Return(roleName+"-mock", nil)
			pg.EXPECT().GrantRole(databaseName+"-writer", gomock.Any()).Return(nil)
			pg.EXPECT().AlterDefaultLoginRole(gomock.Any(), gomock.Any()).Return(nil)
			pg.EXPECT().GrantRole(roleAws, gomock.Any()).Return(fmt.Errorf("grant failed"))

			err := runReconcile(rp, ctx, req)
			Expect(err).NotTo(HaveOccurred())

			foundUser := &dbv1alpha1.PostgresUser{}
			err = cl.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, foundUser)
			Expect(err).NotTo(HaveOccurred())
			Expect(foundUser.Status.EnableIamAuth).To(BeFalse())
		})

		It("revokes rds_iam role when enableIamAuth turns false", func() {
			// Pre-create a user with IAM already enabled in status
			user := postgresUser.DeepCopy()
			user.Spec.AWS = &dbv1alpha1.PostgresUserAWSSpec{EnableIamAuth: false}
			user.Status = dbv1alpha1.PostgresUserStatus{
				Succeeded:     true,
				PostgresGroup: databaseName + "-writer",
				PostgresRole:  roleName + "-exists",
				DatabaseName:  databaseName,
				EnableIamAuth: true,
				PostgresLogin: "login",
			}
			initClient(postgresDB, user, false)

			pg.EXPECT().RevokeRole(roleAws, roleName+"-exists").Return(nil)
			// Since Status.Succeeded=true and the secret does not yet exist, the reconciler
			// updates the password before creating the secret.
			pg.EXPECT().UpdatePassword(gomock.Any(), gomock.Any()).Return(nil)

			err := runReconcile(rp, ctx, req)
			Expect(err).NotTo(HaveOccurred())

			foundUser := &dbv1alpha1.PostgresUser{}
			err = cl.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, foundUser)
			Expect(err).NotTo(HaveOccurred())
			Expect(foundUser.Status.EnableIamAuth).To(BeFalse())
		})
	})
	Context("Secret creation with user-defined labels and annotations", func() {
		It("should create a secret with user-defined labels and annotations", func() {
			// Set up the reconciler with host setting
			rp.pgHost = "localhost"

			// Create a PostgresUser with custom labels and annotations
			cr := &dbv1alpha1.PostgresUser{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "myuser",
					Namespace: "myns",
				},
				Spec: dbv1alpha1.PostgresUserSpec{
					SecretName: "mysecret",
					Labels: map[string]string{
						"custom": "label",
						"foo":    "bar",
					},
				},
				Status: dbv1alpha1.PostgresUserStatus{
					DatabaseName: "somedb",
				},
			}

			// Call newSecretForCR with test values
			secret, err := rp.newSecretForCR(logr.Discard(), cr, "role1", "pass1", "login1")

			// Verify results
			Expect(err).NotTo(HaveOccurred())

			// Check labels
			expectedLabels := map[string]string{
				"app":    "myuser",
				"custom": "label",
				"foo":    "bar",
			}
			Expect(secret.Labels).To(Equal(expectedLabels))

			// Check name and namespace
			Expect(secret.Name).To(Equal("mysecret"))
			Expect(secret.Namespace).To(Equal("myns"))

			// Check secret data
			Expect(string(secret.Data["ROLE"])).To(Equal("role1"))
			Expect(string(secret.Data["PASSWORD"])).To(Equal("pass1"))
			Expect(string(secret.Data["LOGIN"])).To(Equal("login1"))
			Expect(string(secret.Data["DATABASE_NAME"])).To(Equal("somedb"))
			Expect(string(secret.Data["HOST"])).To(Equal("localhost"))
		})

		It("should handle empty labels map correctly", func() {
			// Set up the reconciler
			rp.pgHost = "localhost"

			// Create a PostgresUser with empty labels
			cr := &dbv1alpha1.PostgresUser{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "myuser2",
					Namespace: "myns2",
				},
				Spec: dbv1alpha1.PostgresUserSpec{
					SecretName: "mysecret2",
					Labels:     map[string]string{},
				},
				Status: dbv1alpha1.PostgresUserStatus{
					DatabaseName: "somedb2",
				},
			}

			// Call newSecretForCR
			secret, err := rp.newSecretForCR(logr.Discard(), cr, "role2", "pass2", "login2")

			// Verify results
			Expect(err).NotTo(HaveOccurred())

			// Check that default labels are applied
			expectedLabels := map[string]string{
				"app": "myuser2",
			}
			Expect(secret.Labels).To(Equal(expectedLabels))

			// Check name and namespace
			Expect(secret.Name).To(Equal("mysecret2"))
			Expect(secret.Namespace).To(Equal("myns2"))
		})

		It("should include only template keys when secretTemplate is set", func() {
			rp.pgHost = "dbhost:5432"

			cr := &dbv1alpha1.PostgresUser{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "tpluser",
					Namespace: "tplns",
				},
				Spec: dbv1alpha1.PostgresUserSpec{
					SecretName: "tplsecret",
					Labels:     map[string]string{},
					SecretTemplate: map[string]string{
						"DB_HOST": "{{.Hostname}}",
						"DB_PORT": "{{.Port}}",
						"DB_PASS": "{{.Password}}",
					},
				},
				Status: dbv1alpha1.PostgresUserStatus{
					DatabaseName: "tpldb",
				},
			}

			secret, err := rp.newSecretForCR(logr.Discard(), cr, "tplrole", "tplpass", "tplogin")
			Expect(err).NotTo(HaveOccurred())

			Expect(secret.Data).To(HaveLen(3))
			Expect(string(secret.Data["DB_HOST"])).To(Equal("dbhost"))
			Expect(string(secret.Data["DB_PORT"])).To(Equal("5432"))
			Expect(string(secret.Data["DB_PASS"])).To(Equal("tplpass"))

			Expect(secret.Data).NotTo(HaveKey("POSTGRES_URL"))
			Expect(secret.Data).NotTo(HaveKey("HOST"))
			Expect(secret.Data).NotTo(HaveKey("ROLE"))
			Expect(secret.Data).NotTo(HaveKey("PASSWORD"))
			Expect(secret.Data).NotTo(HaveKey("LOGIN"))
			Expect(secret.Data).NotTo(HaveKey("DATABASE_NAME"))
		})
	})
})
