package postgres

import (
	"context"

	epco "github.com/vterdunov/external-postgres-control-operator/api/v1alpha1"
)

const (
	TableNotFound        = "TableNotFound"
	ACLBindingUnresolved = "ACLBindingUnresolved"
	StatusSizeExceeded   = "StatusSizeExceeded"
)

// DatabaseACLSession is the explicit supported ACL capability of a database adapter.
// Lifecycle-only adapters must not report success for requested ACL work.
type DatabaseACLSession interface {
	PreflightACL(context.Context, DatabaseDesired) error
	PrepareACL(context.Context, ACLDesired) (*ACLResult, error)
	ReconcileACL(context.Context, ACLDesired) (*ACLResult, error)
}

// ACLDesired carries current declarations and durable possibly applied bindings.
type ACLDesired struct {
	Name            string
	Spec            epco.DatabaseSpec
	Targets         []epco.ManagedACLTarget
	Target          epco.ManagedACLTarget
	BindingRevision string
	Generation      int64
	Observe         bool
	Checkpoint      func(context.Context, []epco.ManagedACLTarget) error
}

// ACLResult reports only catalog-confirmed outcomes and retained responsibilities.
type ACLResult struct {
	Targets     []epco.ManagedACLTarget
	Permissions []epco.PermissionStatus
}

const (
	aclClassDatabase        = "database"
	aclClassSchema          = "schema"
	aclClassTable           = "table"
	aclClassSequence        = "sequence"
	aclClassFunction        = "function"
	aclClassType            = "type"
	aclScopeDefault         = "default"
	aclScopeTables          = "tables"
	aclScopeSequences       = "sequences"
	aclPublic               = "PUBLIC"
	aclConnect              = "CONNECT"
	aclUsage                = "USAGE"
	aclMaintain             = "MAINTAIN"
	aclPendingGrant         = "grant"
	aclPendingRevoke        = "revoke"
	aclPendingOwner         = "owner"
	databaseOwnerRole       = "pg_database_owner"
	databaseNotFoundMessage = "The target SQL database does not exist"
)

const (
	aclSelect = "SELECT"
	aclCreate = "CREATE"
)

const aclUpdate = "UPDATE"
