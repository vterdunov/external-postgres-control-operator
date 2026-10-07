package postgres

import (
	"context"
	"time"

	epco "github.com/vterdunov/external-postgres-control-operator/api/v1alpha1"
)

const (
	AccessDenied             = "AccessDenied"
	OwnershipConflict        = "OwnershipConflict"
	DependenciesNotReady     = "DependenciesNotReady"
	UnsupportedFeature       = "UnsupportedFeature"
	ProtectedRoleReason      = "ProtectedRole"
	RoleNotFound             = "RoleNotFound"
	CredentialsMissing       = "CredentialsMissing"
	ConnectionSecretConflict = "ConnectionSecretConflict"
	InsufficientPrivileges   = "InsufficientPrivileges"
	DeletionBlocked          = "DeletionBlocked"
)

// RoleFactory opens isolated, bounded administrative role sessions.
type RoleFactory interface {
	Open(context.Context, *Config) (RoleSession, error)
}

// RoleSession keeps driver connections private to the PostgreSQL layer.
type RoleSession interface {
	Inspect(context.Context, string) (*RoleState, error)
	Apply(context.Context, RoleDesired) (*RoleApplyResult, error)
	Drop(context.Context, string) error
	Close(context.Context) error
}

// RoleState is an inspection snapshot, including grantors and role-wide settings.
type RoleState struct {
	Exists                                                                  bool
	Name, AdminUser                                                         string
	MutationGrantor, BootstrapUser                                          string
	ServerVersion, MaxIdentifierLength                                      int
	Login, Inherit, Superuser, CreateDB, CreateRole, Replication, BypassRLS bool
	ConnectionLimit                                                         int64
	Comment                                                                 string
	ValidUntil                                                              *time.Time
	ValidUntilNegativeInfinity                                              bool
	Memberships                                                             []epco.RoleMembershipBinding
	Parameters                                                              map[string]string
}

// RoleDesired contains one desired role and durable cleanup intent.
type RoleDesired struct {
	Name               string
	Spec               epco.DatabaseRoleSpec
	Password           *string
	TrackedMemberships []epco.RoleMembershipBinding
	TrackedParameters  []string
}

// RoleApplyResult contains only changes confirmed after complete reconciliation.
type RoleApplyResult struct {
	Memberships []epco.RoleMembershipBinding
	Parameters  []string
}

type roleFactory struct{}

// NewRoleFactory constructs the production administrative session factory.
func NewRoleFactory() RoleFactory { return &roleFactory{} }
