package v1alpha1

// Permission declares a SQL grantee's desired privilege state.
type Permission struct {
	// Name is the SQL grantee; public represents PostgreSQL PUBLIC.
	Name SQLName `json:"name"`
	// Ensure selects presence or explicit removal of this grant only.
	// +kubebuilder:default="present"
	// +optional
	Ensure EnsureOption `json:"ensure,omitempty"`
}

// DatabasePermissions declares database privileges.
type DatabasePermissions struct {
	// Connect declares CONNECT grants.
	// +listType=map
	// +listMapKey=name
	// +optional
	Connect []Permission `json:"connect,omitempty"`
	// Create declares CREATE grants.
	// +listType=map
	// +listMapKey=name
	// +optional
	Create []Permission `json:"create,omitempty"`
	// Temporary declares TEMPORARY grants.
	// +listType=map
	// +listMapKey=name
	// +optional
	Temporary []Permission `json:"temporary,omitempty"`
}

// TablePermissions declares table privileges.
type TablePermissions struct {
	// Select declares SELECT grants.
	// +listType=map
	// +listMapKey=name
	// +optional
	Select []Permission `json:"select,omitempty"`
	// Insert declares INSERT grants.
	// +listType=map
	// +listMapKey=name
	// +optional
	Insert []Permission `json:"insert,omitempty"`
	// Update declares UPDATE grants.
	// +listType=map
	// +listMapKey=name
	// +optional
	Update []Permission `json:"update,omitempty"`
	// Delete declares DELETE grants.
	// +listType=map
	// +listMapKey=name
	// +optional
	Delete []Permission `json:"delete,omitempty"`
	// Truncate declares TRUNCATE grants.
	// +listType=map
	// +listMapKey=name
	// +optional
	Truncate []Permission `json:"truncate,omitempty"`
	// References declares REFERENCES grants.
	// +listType=map
	// +listMapKey=name
	// +optional
	References []Permission `json:"references,omitempty"`
	// Trigger declares TRIGGER grants.
	// +listType=map
	// +listMapKey=name
	// +optional
	Trigger []Permission `json:"trigger,omitempty"`
	// Maintain declares MAINTAIN grants.
	// +listType=map
	// +listMapKey=name
	// +optional
	Maintain []Permission `json:"maintain,omitempty"`
}

// SequencePermissions declares sequence privileges.
type SequencePermissions struct {
	// Usage declares USAGE grants.
	// +listType=map
	// +listMapKey=name
	// +optional
	Usage []Permission `json:"usage,omitempty"`
	// Select declares SELECT grants.
	// +listType=map
	// +listMapKey=name
	// +optional
	Select []Permission `json:"select,omitempty"`
	// Update declares UPDATE grants.
	// +listType=map
	// +listMapKey=name
	// +optional
	Update []Permission `json:"update,omitempty"`
}

// FunctionPermissions declares function privileges.
type FunctionPermissions struct {
	// Execute declares EXECUTE grants.
	// +listType=map
	// +listMapKey=name
	// +optional
	Execute []Permission `json:"execute,omitempty"`
}

// TypePermissions declares type privileges.
type TypePermissions struct {
	// Usage declares USAGE grants.
	// +listType=map
	// +listMapKey=name
	// +optional
	Usage []Permission `json:"usage,omitempty"`
}

// SchemaPermissions declares schema and schema-wide relation privileges.
type SchemaPermissions struct {
	// Usage declares schema USAGE grants.
	// +listType=map
	// +listMapKey=name
	// +optional
	Usage []Permission `json:"usage,omitempty"`
	// Create declares schema CREATE grants.
	// +listType=map
	// +listMapKey=name
	// +optional
	Create []Permission `json:"create,omitempty"`
	// Tables manages grants on all existing tables in this schema.
	// +optional
	Tables *TablePermissions `json:"tables,omitempty"`
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

// NamedTablePermissions declares privileges on one existing table.
type NamedTablePermissions struct {
	// Name is the existing SQL table name, without schema qualification.
	Name SQLName `json:"name"`
	// Permissions declares grants without creating or dropping the table.
	Permissions TablePermissions `json:"permissions"`
}
