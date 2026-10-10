package controller

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"strings"

	epco "github.com/vterdunov/external-postgres-control-operator/api/v1alpha1"
	"github.com/vterdunov/external-postgres-control-operator/internal/postgres"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	GeneratedPasswordLabel           = "postgresql.epco.io/generated-password"
	GeneratedRoleUIDAnnotation       = "postgresql.epco.io/role-uid"
	GeneratedRoleNameAnnotation      = "postgresql.epco.io/role-name"
	GeneratedRoleResourceAnnotation  = "postgresql.epco.io/role-resource-name"
	GeneratedRoleNamespaceAnnotation = "postgresql.epco.io/role-resource-namespace"
)

func validatePasswordSource(role *epco.DatabaseRole, source *corev1.Secret) error {
	password, present := source.Data[corev1.BasicAuthPasswordKey]
	if source.Type != corev1.SecretTypeBasicAuth || !present || string(source.Data[corev1.BasicAuthUsernameKey]) != EffectiveRoleName(role) || strings.ContainsRune(string(password), 0) {
		return &postgres.Failure{Reason: "InvalidSecret", Message: "Password Secret must be basic-auth with matching username and password key"}
	}
	return nil
}

func generatedProvenance(role *epco.DatabaseRole, source *corev1.Secret) bool {
	return source.Namespace == role.Namespace && source.Labels[GeneratedPasswordLabel] == "true" &&
		source.Annotations[GeneratedRoleUIDAnnotation] != "" && source.Annotations[GeneratedRoleNameAnnotation] == EffectiveRoleName(role) &&
		source.Annotations[GeneratedRoleResourceAnnotation] == role.Name && source.Annotations[GeneratedRoleNamespaceAnnotation] == role.Namespace
}

func validGeneratedOwners(role *epco.DatabaseRole, source *corev1.Secret) bool {
	for _, owner := range source.OwnerReferences {
		if string(owner.UID) != source.Annotations[GeneratedRoleUIDAnnotation] || owner.APIVersion != epco.SchemeGroupVersion.String() || owner.Kind != "DatabaseRole" || owner.Name != role.Name {
			return false
		}
	}
	return true
}

func (r *DatabaseRoleReconciler) rolePassword(ctx context.Context, role *epco.DatabaseRole) (*string, *corev1.Secret, error) {
	if role.Spec.PasswordGeneration == nil && role.Spec.PasswordSecret == nil {
		return nil, nil, nil
	}
	if err := checkRoleSourceOutputReservation(ctx, r.Client, role); err != nil {
		return nil, nil, err
	}
	name := ""
	generated := role.Spec.PasswordGeneration != nil
	if generated {
		name = role.Spec.PasswordGeneration.SecretName
	} else {
		name = role.Spec.PasswordSecret.Name
	}
	source := &corev1.Secret{}
	err := r.Client.Get(ctx, client.ObjectKey{Namespace: role.Namespace, Name: name}, source)
	if apierrors.IsNotFound(err) {
		if !generated {
			return nil, nil, &postgres.Failure{Reason: "SecretNotFound", Message: "The password source does not exist"}
		}
		if role.Status.GeneratedPasswordInitialized {
			return nil, nil, &postgres.Failure{Reason: postgres.CredentialsMissing, Message: "The initialized password source is missing"}
		}
		// Even an unchanged status UPDATE carries the cached RV precondition. Never CREATE first.
		if err = r.Client.Status().Update(ctx, role); err != nil {
			return nil, nil, err
		}
		random := make([]byte, 32)
		if _, err = rand.Read(random); err != nil {
			return nil, nil, &postgres.Failure{Reason: postgres.InvalidConfiguration, Message: "Unable to generate role credentials"}
		}
		source = &corev1.Secret{Namespace: role.Namespace, Name: name, Type: corev1.SecretTypeBasicAuth,
			Labels:      map[string]string{GeneratedPasswordLabel: "true"},
			Annotations: map[string]string{GeneratedRoleUIDAnnotation: string(role.UID), GeneratedRoleNameAnnotation: EffectiveRoleName(role), GeneratedRoleResourceAnnotation: role.Name, GeneratedRoleNamespaceAnnotation: role.Namespace},
			Data:        map[string][]byte{corev1.BasicAuthUsernameKey: []byte(EffectiveRoleName(role)), corev1.BasicAuthPasswordKey: []byte(base64.RawURLEncoding.EncodeToString(random))}}
		if err = r.Client.Create(ctx, source); err != nil {
			return nil, nil, secretPayloadWriteError(err)
		}
	} else if err != nil {
		return nil, nil, err
	}
	if err = validatePasswordSource(role, source); err != nil {
		return nil, nil, err
	}
	if source.Labels[ConnectionSecretLabel] == connectionSecretLabelValue {
		return nil, nil, connectionConflict()
	}
	if generated {
		if !generatedProvenance(role, source) {
			return nil, nil, &postgres.Failure{Reason: postgres.ConnectionSecretConflict, Message: "Generated credentials have conflicting provenance"}
		}
		if !validGeneratedOwners(role, source) {
			return nil, nil, &postgres.Failure{Reason: postgres.ConnectionSecretConflict, Message: "Generated credentials have unrelated Kubernetes ownership"}
		}
		if source.Annotations[GeneratedRoleUIDAnnotation] != string(role.UID) {
			reader := r.APIReader
			if reader == nil {
				reader = r.Client
			}
			current := &epco.DatabaseRole{}
			if err = reader.Get(ctx, client.ObjectKeyFromObject(role), current); err != nil {
				return nil, nil, err
			}
			if current.UID != role.UID || string(current.UID) == source.Annotations[GeneratedRoleUIDAnnotation] || !current.DeletionTimestamp.IsZero() {
				return nil, nil, &postgres.Failure{Reason: postgres.ConnectionSecretConflict, Message: "Generated credentials still have an active owner"}
			}
			source.Annotations[GeneratedRoleUIDAnnotation] = string(role.UID)
			source.OwnerReferences = nil
			if err = r.Client.Update(ctx, source); err != nil {
				return nil, nil, secretPayloadWriteError(err)
			}
		} else if len(source.OwnerReferences) != 0 {
			return nil, nil, &postgres.Failure{Reason: postgres.ConnectionSecretConflict, Message: "Generated source ownership is unsafe for retention"}
		}
		if !role.Status.GeneratedPasswordInitialized {
			role.Status.GeneratedPasswordInitialized = true
			if err = r.Client.Status().Update(ctx, role); err != nil {
				return nil, nil, err
			}
		}
	}
	return new(string(source.Data[corev1.BasicAuthPasswordKey])), source, nil
}

func (r *DatabaseRoleReconciler) deleteGeneratedSource(ctx context.Context, role *epco.DatabaseRole) error {
	if role.Spec.PasswordGeneration == nil {
		return nil
	}
	source := &corev1.Secret{}
	if err := r.Client.Get(ctx, client.ObjectKey{Namespace: role.Namespace, Name: role.Spec.PasswordGeneration.SecretName}, source); err != nil {
		return client.IgnoreNotFound(err)
	}
	if !generatedProvenance(role, source) || source.Annotations[GeneratedRoleUIDAnnotation] != string(role.UID) || !validGeneratedOwners(role, source) {
		return nil
	}
	return client.IgnoreNotFound(secretPayloadWriteError(r.Client.Delete(ctx, source, client.Preconditions{UID: new(source.UID), ResourceVersion: new(source.ResourceVersion)})))
}
