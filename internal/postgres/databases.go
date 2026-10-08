package postgres

import (
	"context"

	epco "github.com/vterdunov/external-postgres-control-operator/api/v1alpha1"
)

const (
	ProtectedDatabase = "ProtectedDatabase"
	ProtectedSchema   = "ProtectedSchema"
	DatabaseNotFound  = "DatabaseNotFound"
)

// DatabaseFactory opens isolated administrative database sessions.
type DatabaseFactory interface {
	Open(context.Context, *Config) (DatabaseSession, error)
}

// DatabaseSession isolates database and target-schema operations from controllers.
type DatabaseSession interface {
	Inspect(context.Context, string) (*DatabaseState, error)
	InspectSchemas(context.Context, string, []epco.SchemaSpec) ([]SchemaState, error)
	Apply(context.Context, DatabaseDesired) (*DatabaseApplyResult, error)
	Drop(context.Context, string) error
	Close(context.Context) error
}

// DatabaseState describes the current catalog and administrative connection.
type DatabaseState struct {
	Exists                                            bool
	Name, Owner, AdminUser, AdminDatabase, Tablespace string
	ServerVersion, MaxIdentifierLength                int
	IsTemplate, AllowConnections                      bool
	ConnectionLimit                                   int64
}

// SchemaState describes a schema in the explicitly selected target database.
type SchemaState struct {
	Name, Owner string
	Exists      bool
}

// DatabaseDesired is the supported desired database and embedded schema state.
type DatabaseDesired struct {
	Name string
	Spec epco.DatabaseSpec
}

// DatabaseApplyResult confirms complete reconciliation and catalog readback.
type DatabaseApplyResult struct{}

type databaseFactory struct{}

// NewDatabaseFactory creates the production PostgreSQL database adapter.
func NewDatabaseFactory() DatabaseFactory { return &databaseFactory{} }
