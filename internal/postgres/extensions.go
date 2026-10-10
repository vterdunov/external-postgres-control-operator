package postgres

import (
	"context"
	"strconv"

	epco "github.com/vterdunov/external-postgres-control-operator/api/v1alpha1"
)

// DatabaseExtensionSession explicitly confirms support for embedded extensions.
// Lifecycle-only adapters cannot silently accept requested extension work.
type DatabaseExtensionSession interface {
	InspectExtensions(context.Context, string, []epco.ExtensionSpec) ([]ExtensionState, error)
	ReconcileExtensions(context.Context, string, []epco.ExtensionSpec) (*ExtensionResult, error)
}

// ExtensionState is installed catalog state on the selected target database.
type ExtensionState struct {
	Name, Owner, Version, Schema string
	Exists, Relocatable          bool
}

// ExtensionResult contains catalog-confirmed extension outcomes.
type ExtensionResult struct {
	States []ExtensionState
}

// ExtensionFailure adds safe extension context to a classified failure.
// PostgreSQL details are classified before construction and never included.
type ExtensionFailure struct {
	Name    string
	Failure *Failure
}

func (e *ExtensionFailure) Error() string {
	return "Extension " + strconv.Quote(e.Name) + ": " + e.Failure.Message
}

func (e *ExtensionFailure) Unwrap() error { return e.Failure }
