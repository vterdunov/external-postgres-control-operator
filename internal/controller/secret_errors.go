package controller

import (
	"context"
	"errors"

	"github.com/vterdunov/external-postgres-control-operator/internal/postgres"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Secret admission responses may echo submitted credentials. Preserve only
// retry classification, never their original message or wrapped causes.
func secretPayloadWriteError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	resource := schema.GroupResource{Resource: "secrets"}
	switch {
	case apierrors.IsConflict(err):
		return apierrors.NewConflict(resource, "", errors.New("secret write conflicted with current state"))
	case apierrors.IsAlreadyExists(err):
		return apierrors.NewAlreadyExists(resource, "")
	case apierrors.IsNotFound(err):
		return apierrors.NewNotFound(resource, "")
	default:
		return &postgres.Failure{Reason: postgres.DependenciesNotReady, Message: "A requested Secret could not be persisted"}
	}
}
