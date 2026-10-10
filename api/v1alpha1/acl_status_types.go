package v1alpha1

// ManagedACLTarget records concrete ACL responsibility on one configured target.
// Inactive targets are retained but never contacted for cleanup.
type ManagedACLTarget struct {
	Host     string `json:"host"`
	Port     int32  `json:"port"`
	Database string `json:"database"`
	// BindingRevision invalidates catalog identity after routing/descriptor changes.
	// +optional
	BindingRevision string `json:"bindingRevision,omitempty"`
	// +optional
	Bindings []ACLBinding `json:"bindings,omitempty"`
}

// ACLRoleIdentity is the catalog identity and last confirmed name of a SQL role.
type ACLRoleIdentity struct {
	Name string `json:"name"`
	// +optional
	OID uint32 `json:"oid,omitzero"`
}

// ACLGrantIdentity preserves the narrow pre-operation grantor provenance.
type ACLGrantIdentity struct {
	Grantor ACLRoleIdentity `json:"grantor"`
	// Grantee is recorded when owner transfer can also rewrite the grantee.
	// +optional
	Grantee *ACLRoleIdentity `json:"grantee,omitempty"`
	// +optional
	Grantable bool `json:"grantable,omitzero"`
}

// ACLBinding is one concrete object/privilege/grantee/grantor responsibility.
// Pending operations are possibly issued work, never commands to replay blindly.
type ACLBinding struct {
	// BindingRevision records the target epoch in which catalog identities were resolved.
	// +optional
	BindingRevision string `json:"bindingRevision,omitempty"`
	Declaration     string `json:"declaration"`
	Scope           string `json:"scope"`
	Class           string `json:"class"`
	Privilege       string `json:"privilege"`
	Grantee         string `json:"grantee"`
	// +optional
	Schema string `json:"schema,omitempty"`
	// +optional
	Table string `json:"table,omitempty"`
	// +optional
	Creator string `json:"creator,omitempty"`
	// +optional
	Grantor string `json:"grantor,omitempty"`
	// +optional
	Owner string `json:"owner,omitempty"`
	// +optional
	DatabaseOID uint32 `json:"databaseOID,omitzero"`
	// +optional
	SchemaOID uint32 `json:"schemaOID,omitzero"`
	// +optional
	ObjectOID uint32 `json:"objectOID,omitzero"`
	// +optional
	CreatorOID uint32 `json:"creatorOID,omitzero"`
	// +optional
	GranteeOID uint32 `json:"granteeOID,omitzero"`
	// +optional
	GrantorOID uint32 `json:"grantorOID,omitzero"`
	// +optional
	OwnerOID uint32 `json:"ownerOID,omitzero"`
	// +optional
	Pending string `json:"pending,omitempty"`
	// +optional
	Generation int64 `json:"generation,omitzero"`
	// +optional
	Administrative bool `json:"administrative,omitzero"`
	// +optional
	GrantorCandidates []ACLRoleIdentity `json:"grantorCandidates,omitempty"`
	// Before contains the matching direct rows before a possibly issued operation.
	// +optional
	Before []ACLGrantIdentity `json:"before,omitempty"`
	// +optional
	PreviousOwner *ACLRoleIdentity `json:"previousOwner,omitempty"`
	// +optional
	DesiredOwner *ACLRoleIdentity `json:"desiredOwner,omitempty"`
}
