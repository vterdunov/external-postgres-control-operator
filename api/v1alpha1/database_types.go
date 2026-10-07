package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// DatabaseSpec defines a PostgreSQL database and its managed SQL objects and ACLs.
// +kubebuilder:validation:XValidation:rule="self.managementPolicy != 'manage' || has(self.owner)",message="owner is required in manage mode"
// +kubebuilder:validation:XValidation:rule="self.managementPolicy != 'observe' || (self.ensure == 'present' && self.databaseReclaimPolicy == 'retain')",message="observe requires ensure=present and retain reclaim policy"
// +kubebuilder:validation:XValidation:rule="!has(self.icuLocale) || (has(self.localeProvider) && self.localeProvider == 'icu')",message="icuLocale requires localeProvider=icu"
// +kubebuilder:validation:XValidation:rule="!has(self.icuRules) || (has(self.localeProvider) && self.localeProvider == 'icu')",message="icuRules requires localeProvider=icu"
// +kubebuilder:validation:XValidation:rule="!has(self.builtinLocale) || (has(self.localeProvider) && self.localeProvider == 'builtin')",message="builtinLocale requires localeProvider=builtin"
// +kubebuilder:validation:XValidation:rule="has(self.template) == has(oldSelf.template) && (!has(self.template) || self.template == oldSelf.template)",message="template is immutable"
// +kubebuilder:validation:XValidation:rule="has(self.encoding) == has(oldSelf.encoding) && (!has(self.encoding) || self.encoding == oldSelf.encoding)",message="encoding is immutable"
// +kubebuilder:validation:XValidation:rule="has(self.locale) == has(oldSelf.locale) && (!has(self.locale) || self.locale == oldSelf.locale)",message="locale is immutable"
// +kubebuilder:validation:XValidation:rule="has(self.localeProvider) == has(oldSelf.localeProvider) && (!has(self.localeProvider) || self.localeProvider == oldSelf.localeProvider)",message="localeProvider is immutable"
// +kubebuilder:validation:XValidation:rule="has(self.localeCollate) == has(oldSelf.localeCollate) && (!has(self.localeCollate) || self.localeCollate == oldSelf.localeCollate)",message="localeCollate is immutable"
// +kubebuilder:validation:XValidation:rule="has(self.localeCType) == has(oldSelf.localeCType) && (!has(self.localeCType) || self.localeCType == oldSelf.localeCType)",message="localeCType is immutable"
// +kubebuilder:validation:XValidation:rule="has(self.icuLocale) == has(oldSelf.icuLocale) && (!has(self.icuLocale) || self.icuLocale == oldSelf.icuLocale)",message="icuLocale is immutable"
// +kubebuilder:validation:XValidation:rule="has(self.icuRules) == has(oldSelf.icuRules) && (!has(self.icuRules) || self.icuRules == oldSelf.icuRules)",message="icuRules is immutable"
// +kubebuilder:validation:XValidation:rule="has(self.builtinLocale) == has(oldSelf.builtinLocale) && (!has(self.builtinLocale) || self.builtinLocale == oldSelf.builtinLocale)",message="builtinLocale is immutable"
// +kubebuilder:validation:XValidation:rule="has(self.collationVersion) == has(oldSelf.collationVersion) && (!has(self.collationVersion) || self.collationVersion == oldSelf.collationVersion)",message="collationVersion is immutable"
type DatabaseSpec struct {
	// Cluster identifies the connection descriptor.
	Cluster ClusterReference `json:"cluster"`
	// Name is the SQL database name; omission resolves to metadata.name.
	// +optional
	Name SQLName `json:"name,omitempty"`
	// Owner is the SQL owner role, required in manage mode.
	// +optional
	Owner SQLName `json:"owner,omitempty"`
	// Ensure requests database presence or explicit removal regardless of reclaim policy.
	// +kubebuilder:default="present"
	// +optional
	Ensure EnsureOption `json:"ensure,omitempty"`
	// Template is a creation-only PostgreSQL database option.
	// +optional
	Template SQLName `json:"template,omitempty"`
	// Encoding is a creation-only PostgreSQL database option.
	// +kubebuilder:validation:MinLength=1
	// +optional
	Encoding string `json:"encoding,omitempty"`
	// Locale is a creation-only PostgreSQL database option.
	// +kubebuilder:validation:MinLength=1
	// +optional
	Locale string `json:"locale,omitempty"`
	// LocaleProvider is a creation-only PostgreSQL database option.
	// +kubebuilder:validation:Enum=libc;icu;builtin
	// +kubebuilder:validation:MinLength=1
	// +optional
	LocaleProvider string `json:"localeProvider,omitempty"`
	// LocaleCollate is a creation-only PostgreSQL database option.
	// +kubebuilder:validation:MinLength=1
	// +optional
	LocaleCollate string `json:"localeCollate,omitempty"`
	// LocaleCType is a creation-only PostgreSQL database option.
	// +kubebuilder:validation:MinLength=1
	// +optional
	LocaleCType string `json:"localeCType,omitempty"`
	// ICULocale is a creation-only PostgreSQL database option.
	// +kubebuilder:validation:MinLength=1
	// +optional
	ICULocale string `json:"icuLocale,omitempty"`
	// ICURules is a creation-only PostgreSQL database option.
	// +kubebuilder:validation:MinLength=1
	// +optional
	ICURules string `json:"icuRules,omitempty"`
	// BuiltinLocale is a creation-only PostgreSQL database option.
	// +kubebuilder:validation:MinLength=1
	// +optional
	BuiltinLocale string `json:"builtinLocale,omitempty"`
	// CollationVersion is a creation-only PostgreSQL database option.
	// +kubebuilder:validation:MinLength=1
	// +optional
	CollationVersion string `json:"collationVersion,omitempty"`
	// IsTemplate changes template status only when explicitly specified.
	// +optional
	IsTemplate *bool `json:"isTemplate,omitempty"`
	// AllowConnections changes connection admission only when explicitly specified.
	// +optional
	AllowConnections *bool `json:"allowConnections,omitempty"`
	// ConnectionLimit changes the database limit only when explicitly specified.
	// +kubebuilder:validation:Minimum=-1
	// +optional
	ConnectionLimit *int64 `json:"connectionLimit,omitempty"`
	// Tablespace is the SQL tablespace used by the database.
	// +optional
	Tablespace SQLName `json:"tablespace,omitempty"`
	// Permissions declares database CONNECT, CREATE, and TEMPORARY grants.
	// +optional
	Permissions *DatabasePermissions `json:"permissions,omitempty"`
	// Schemas contains schemas and their permissions.
	// +listType=map
	// +listMapKey=name
	// +optional
	Schemas []SchemaSpec `json:"schemas,omitempty"`
	// Extensions contains managed extensions.
	// +listType=map
	// +listMapKey=name
	// +optional
	Extensions []ExtensionSpec `json:"extensions,omitempty"`
	// FDWs contains foreign data wrappers in CNPG format.
	// +listType=map
	// +listMapKey=name
	// +optional
	FDWs []FDWSpec `json:"fdws,omitempty"`
	// Servers contains foreign servers in CNPG format.
	// +listType=map
	// +listMapKey=name
	// +optional
	Servers []ServerSpec `json:"servers,omitempty"`
	// DefaultPrivileges contains global defaults for actual object creator roles.
	// +listType=map
	// +listMapKey=forRole
	// +optional
	DefaultPrivileges []DefaultPrivileges `json:"defaultPrivileges,omitempty"`
	// ConnectionSecrets declares application connection exports in this namespace.
	// +listType=map
	// +listMapKey=name
	// +optional
	ConnectionSecrets []ConnectionSecret `json:"connectionSecrets,omitempty"`
	// ManagementPolicy selects reconciliation or read-only observation.
	// +kubebuilder:default="manage"
	// +optional
	ManagementPolicy ManagementPolicy `json:"managementPolicy,omitempty"`
	// ReclaimPolicy controls SQL cleanup when this CR is deleted.
	// +kubebuilder:default="retain"
	// +optional
	ReclaimPolicy ReclaimPolicy `json:"databaseReclaimPolicy,omitempty"`
}

// ConnectionSecret declares one derived application basic-auth Secret.
type ConnectionSecret struct {
	// Name is the output Secret name in the Database namespace.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
	// RoleRef identifies a local DatabaseRole with a password source.
	RoleRef LocalReference `json:"roleRef"`
	// SSLRootCertPath is the application's mounted CA path. When omitted and a
	// cluster CA is configured, the reconciler uses /etc/postgresql/ca.crt.
	// +kubebuilder:validation:MinLength=1
	// +optional
	SSLRootCertPath string `json:"sslRootCertPath,omitempty"`
}

// DatabaseObjectSpec contains the lifecycle fields shared by SQL objects.
type DatabaseObjectSpec struct {
	// Name is the SQL object name.
	Name SQLName `json:"name"`
	// Ensure requests presence or DROP with RESTRICT; omission means present.
	// +kubebuilder:default="present"
	// +optional
	Ensure EnsureOption `json:"ensure,omitempty"`
}

// SchemaSpec declares a schema and its managed ACL scopes.
type SchemaSpec struct {
	DatabaseObjectSpec `json:",inline"`
	// Owner is applied explicitly; new schemas otherwise use the database owner.
	// +optional
	Owner SQLName `json:"owner,omitempty"`
	// Permissions manages schema, all-table, and all-sequence grants.
	// +optional
	Permissions *SchemaPermissions `json:"permissions,omitempty"`
	// DefaultPrivileges contains schema-local defaults for actual creator roles.
	// +listType=map
	// +listMapKey=forRole
	// +optional
	DefaultPrivileges []DefaultPrivileges `json:"defaultPrivileges,omitempty"`
	// TablePermissions targets existing named tables without managing their DDL.
	// +listType=map
	// +listMapKey=name
	// +optional
	TablePermissions []NamedTablePermissions `json:"tablePermissions,omitempty"`
}

// ExtensionSpec declares a PostgreSQL extension.
type ExtensionSpec struct {
	DatabaseObjectSpec `json:",inline"`
	// Version selects the extension version; omission uses the server default.
	// +optional
	Version string `json:"version,omitempty"`
	// Schema selects the extension object schema when relocation is supported.
	// +optional
	Schema SQLName `json:"schema,omitempty"`
}

// FDWSpec declares a foreign data wrapper in CNPG format.
type FDWSpec struct {
	DatabaseObjectSpec `json:",inline"`
	// Handler is the SQL handler function name.
	// +optional
	Handler SQLName `json:"handler,omitempty"`
	// Validator is the SQL validator function name.
	// +optional
	Validator SQLName `json:"validator,omitempty"`
	// Owner is the SQL owner role.
	// +optional
	Owner SQLName `json:"owner,omitempty"`
	// Options declares managed FDW options.
	// +listType=map
	// +listMapKey=name
	// +optional
	Options []OptionSpec `json:"options,omitempty"`
	// Usage uses CNPG's grant/revoke format rather than permission ensure entries.
	// +listType=map
	// +listMapKey=name
	// +optional
	Usage []UsageSpec `json:"usage,omitempty"`
}

// ServerSpec declares a foreign server in CNPG format.
type ServerSpec struct {
	DatabaseObjectSpec `json:",inline"`
	// FDW is the SQL foreign data wrapper name.
	FDW SQLName `json:"fdw"`
	// Options declares managed server options.
	// +listType=map
	// +listMapKey=name
	// +optional
	Options []OptionSpec `json:"options,omitempty"`
	// Usage uses CNPG's grant/revoke format.
	// +listType=map
	// +listMapKey=name
	// +optional
	Usage []UsageSpec `json:"usage,omitempty"`
}

// OptionSpec declares a foreign wrapper/server option.
type OptionSpec struct {
	// Name is the SQL option name.
	Name SQLName `json:"name"`
	// Value is the option value, including an explicitly empty value.
	Value string `json:"value"`
	// Ensure selects presence or removal.
	// +kubebuilder:default="present"
	// +optional
	Ensure EnsureOption `json:"ensure,omitempty"`
}

// UsageSpec declares a CNPG-compatible FDW/server usage operation.
type UsageSpec struct {
	// Name is the SQL grantee name.
	Name SQLName `json:"name"`
	// Type selects the CNPG grant/revoke operation.
	// +kubebuilder:validation:Enum=grant;revoke
	// +kubebuilder:default="grant"
	// +optional
	Type string `json:"type,omitempty"`
}

// DatabaseStatus reports database reconciliation and per-grant results.
type DatabaseStatus struct {
	ResourceStatus `json:",inline"`
	// Permissions identifies observed outcomes for schema/table/grantee tuples.
	// +optional
	Permissions []PermissionStatus `json:"permissions,omitempty"`
}

// PermissionStatus identifies the result of one permission reconciliation.
type PermissionStatus struct {
	// Schema identifies the SQL schema when applicable.
	// +optional
	Schema string `json:"schema,omitempty"`
	// Table identifies an explicitly named SQL table when applicable.
	// +optional
	Table string `json:"table,omitempty"`
	// Grantee identifies the SQL role or PUBLIC grantee.
	Grantee string `json:"grantee"`
	// Privilege identifies the reconciled SQL privilege.
	Privilege string `json:"privilege"`
	// Applied indicates whether the desired permission state was confirmed.
	// +optional
	Applied bool `json:"applied,omitempty"`
	// Message describes the observed outcome without credentials.
	// +optional
	Message string `json:"message,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=pgdb
// +kubebuilder:printcolumn:name="Cluster",type=string,JSONPath=".spec.cluster.name"
// +kubebuilder:printcolumn:name="SQL Name",type=string,JSONPath=".status.effectiveName"
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"
// +kubebuilder:validation:XValidation:rule="(has(self.spec.name) ? self.spec.name : self.metadata.name) == (has(oldSelf.spec.name) ? oldSelf.spec.name : oldSelf.metadata.name)",message="effective SQL name is immutable"

// Database is the API for a PostgreSQL database, SQL objects, and permissions.
type Database struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitzero"`
	Spec              DatabaseSpec `json:"spec"`
	// +optional
	Status DatabaseStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// DatabaseList contains Database resources.
type DatabaseList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []Database `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &Database{}, &DatabaseList{})
		return nil
	})
}
