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
	OID                                                                     uint32
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
	Name     string
	Spec     epco.DatabaseRoleSpec
	Password *string
	// PreviousPasswordWrite must match the current inputs and be recent. The SQL target is checked separately.
	PreviousPasswordWrite *epco.RolePasswordWrite
	TrackedMemberships    []epco.RoleMembershipBinding
	TrackedParameters     []string
	// CheckpointMembership persists an exact candidate before SQL. A false
	// pending value retires only an absent, rolled-back candidate.
	CheckpointMembership func(context.Context, epco.RoleMembershipBinding, bool) error
}

// RoleApplyResult contains only changes confirmed after complete reconciliation.
type RoleApplyResult struct {
	Memberships   []epco.RoleMembershipBinding
	Parameters    []string
	PasswordWrite *RolePasswordTarget
}

// RolePasswordTarget identifies the role and server of a committed password write.
type RolePasswordTarget struct {
	RoleOID              uint32
	ServerStartUnixMicro int64
}

type roleFactory struct{}

// NewRoleFactory constructs the production administrative session factory.
func NewRoleFactory() RoleFactory { return &roleFactory{} }
