package v1alpha1

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
)

// Permission declares a SQL grantee's desired privilege state.
type Permission struct {
	// Role is the SQL grantee; public represents PostgreSQL PUBLIC.
	Role SQLName `json:"role"`
	// Ensure selects presence or explicit removal of this grant only.
	// +kubebuilder:default="present"
	// +optional
	Ensure EnsureOption `json:"ensure,omitempty"`
}

// DatabasePermissions declares database privileges.
type DatabasePermissions struct {
	// Connect declares CONNECT grants.
	// +listType=map
	// +listMapKey=role
	// +optional
	Connect []Permission `json:"connect,omitempty"`
	// Create declares CREATE grants.
	// +listType=map
	// +listMapKey=role
	// +optional
	Create []Permission `json:"create,omitempty"`
	// Temporary declares TEMPORARY grants.
	// +listType=map
	// +listMapKey=role
	// +optional
	Temporary []Permission `json:"temporary,omitempty"`
}

// TablePermissions declares table privileges.
type TablePermissions struct {
	// Select declares SELECT grants.
	// +listType=map
	// +listMapKey=role
	// +optional
	Select []Permission `json:"select,omitempty"`
	// Insert declares INSERT grants.
	// +listType=map
	// +listMapKey=role
	// +optional
	Insert []Permission `json:"insert,omitempty"`
	// Update declares UPDATE grants.
	// +listType=map
	// +listMapKey=role
	// +optional
	Update []Permission `json:"update,omitempty"`
	// Delete declares DELETE grants.
	// +listType=map
	// +listMapKey=role
	// +optional
	Delete []Permission `json:"delete,omitempty"`
	// Truncate declares TRUNCATE grants.
	// +listType=map
	// +listMapKey=role
	// +optional
	Truncate []Permission `json:"truncate,omitempty"`
	// References declares REFERENCES grants.
	// +listType=map
	// +listMapKey=role
	// +optional
	References []Permission `json:"references,omitempty"`
	// Trigger declares TRIGGER grants.
	// +listType=map
	// +listMapKey=role
	// +optional
	Trigger []Permission `json:"trigger,omitempty"`
	// Maintain declares MAINTAIN grants.
	// +listType=map
	// +listMapKey=role
	// +optional
	Maintain []Permission `json:"maintain,omitempty"`
}

// SequencePermissions declares sequence privileges.
type SequencePermissions struct {
	// Usage declares USAGE grants.
	// +listType=map
	// +listMapKey=role
	// +optional
	Usage []Permission `json:"usage,omitempty"`
	// Select declares SELECT grants.
	// +listType=map
	// +listMapKey=role
	// +optional
	Select []Permission `json:"select,omitempty"`
	// Update declares UPDATE grants.
	// +listType=map
	// +listMapKey=role
	// +optional
	Update []Permission `json:"update,omitempty"`
}

// FunctionPermissions declares function privileges.
type FunctionPermissions struct {
	// Execute declares EXECUTE grants.
	// +listType=map
	// +listMapKey=role
	// +optional
	Execute []Permission `json:"execute,omitempty"`
}

// TypePermissions declares type privileges.
type TypePermissions struct {
	// Usage declares USAGE grants.
	// +listType=map
	// +listMapKey=role
	// +optional
	Usage []Permission `json:"usage,omitempty"`
}

// SchemaPermissions declares schema and schema-wide relation privileges.
type SchemaPermissions struct {
	// Usage declares schema USAGE grants.
	// +listType=map
	// +listMapKey=role
	// +optional
	Usage []Permission `json:"usage,omitempty"`
	// Create declares schema CREATE grants.
	// +listType=map
	// +listMapKey=role
	// +optional
	Create []Permission `json:"create,omitempty"`
	// Sequences manages grants on all existing sequences in this schema.
	// +optional
	Sequences *SequencePermissions `json:"sequences,omitempty"`
}

// DefaultPrivileges declares future-object privileges for the actual SQL creator.
type DefaultPrivileges struct {
	// ForRole is the SQL role creating the future objects.
	ForRole SQLName `json:"forRole"`
	// Tables declares defaults for future tables.
	// +optional
	Tables *TablePermissions `json:"tables,omitempty"`
	// Sequences declares defaults for future sequences.
	// +optional
	Sequences *SequencePermissions `json:"sequences,omitempty"`
	// Functions declares defaults for future functions.
	// +optional
	Functions *FunctionPermissions `json:"functions,omitempty"`
	// Types declares defaults for future types.
	// +optional
	Types *TypePermissions `json:"types,omitempty"`
}

// TablePrivilege is one supported table privilege.
// +kubebuilder:validation:Enum=select;insert;update;delete;truncate;references;trigger;maintain
type TablePrivilege string

// TablePermissionRule declares one role's privileges on selected existing tables.
type TablePermissionRule struct {
	// Role is the SQL grantee; public represents PostgreSQL PUBLIC.
	Role SQLName `json:"role"`
	// Tables contains literal names in the containing schema; omission selects all tables.
	// Explicit null is retained as an invalid selection for rejection before SQL.
	// +kubebuilder:validation:MinItems=1
	// +listType=set
	// +nullable
	// +optional
	Tables []SQLName `json:"tables,omitzero"`
	// Privileges lists individual lowercase table privileges.
	// +kubebuilder:validation:MinItems=1
	// +listType=set
	Privileges []TablePrivilege `json:"privileges"`
	// Ensure selects presence or explicit removal of the listed grants only.
	// +kubebuilder:default="present"
	// +optional
	Ensure EnsureOption `json:"ensure,omitempty"`
}

// UnmarshalJSON preserves explicit null as an invalid nonnil empty selection.
// The containing rule decoder also runs when Kubernetes converts unstructured maps.
func (p *TablePermissionRule) UnmarshalJSON(data []byte) error {
	type plain TablePermissionRule
	var decoded plain
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	var selector struct {
		Tables jsontext.Value `json:"tables"`
	}
	if err := json.Unmarshal(data, &selector); err != nil {
		return err
	}
	if bytes.Equal(bytes.TrimSpace(selector.Tables), []byte("null")) {
		decoded.Tables = []SQLName{}
	}
	*p = TablePermissionRule(decoded)
	return nil
}
