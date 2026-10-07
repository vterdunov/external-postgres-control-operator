package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// DatabaseRoleSpec defines PostgreSQL role attributes and password sources.
// +kubebuilder:validation:XValidation:rule="!(has(self.passwordSecret) && has(self.passwordGeneration))",message="passwordSecret and passwordGeneration are mutually exclusive"
// +kubebuilder:validation:XValidation:rule="!self.disablePassword || (!has(self.passwordSecret) && !has(self.passwordGeneration))",message="disablePassword cannot be combined with a password source"
// +kubebuilder:validation:XValidation:rule="self.managementPolicy != 'observe' || self.databaseRoleReclaimPolicy == 'retain'",message="observe requires retain reclaim policy"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.passwordGeneration) || (has(self.passwordGeneration) && self.passwordGeneration.secretName == oldSelf.passwordGeneration.secretName)",message="generated password source is immutable once configured"
type DatabaseRoleSpec struct {
	// Cluster identifies the PostgreSQL connection descriptor.
	Cluster ClusterReference `json:"cluster"`
	// Name is the SQL role name; omission resolves to metadata.name.
	// +optional
	Name SQLName `json:"name,omitempty"`
	// Comment is the SQL role description.
	// +optional
	Comment *string `json:"comment,omitempty"`
	// Login allows the role to log in. It is never enabled implicitly.
	// +kubebuilder:default=false
	// +optional
	Login bool `json:"login,omitempty"`
	// Inherit controls the role attribute, not existing PG16+ membership options.
	// +kubebuilder:default=true
	// +optional
	Inherit *bool `json:"inherit,omitempty"`
	// Superuser grants PostgreSQL superuser status when the server permits it.
	// +kubebuilder:default=false
	// +optional
	Superuser bool `json:"superuser,omitempty"`
	// CreateDB allows database creation.
	// +kubebuilder:default=false
	// +optional
	CreateDB bool `json:"createdb,omitempty"`
	// CreateRole allows role administration subject to server authorization.
	// +kubebuilder:default=false
	// +optional
	CreateRole bool `json:"createrole,omitempty"`
	// Replication allows replication connections.
	// +kubebuilder:default=false
	// +optional
	Replication bool `json:"replication,omitempty"`
	// BypassRLS bypasses row-level security.
	// +kubebuilder:default=false
	// +optional
	BypassRLS bool `json:"bypassrls,omitempty"`
	// ConnectionLimit is the role's connection limit; -1 means unlimited.
	// +kubebuilder:validation:Minimum=-1
	// +kubebuilder:default=-1
	// +optional
	ConnectionLimit *int64 `json:"connectionLimit,omitempty"`
	// ValidUntil is the password expiration timestamp. Omission means no expiration.
	// +optional
	ValidUntil *metav1.Time `json:"validUntil,omitempty"`
	// DisablePassword sets PASSWORD NULL.
	// +kubebuilder:default=false
	// +optional
	DisablePassword bool `json:"disablePassword,omitempty"`
	// PasswordSecret selects a supplied local basic-auth Secret.
	// +optional
	PasswordSecret *LocalReference `json:"passwordSecret,omitempty"`
	// PasswordGeneration requests a persisted, reusable generated password.
	// +optional
	PasswordGeneration *PasswordGeneration `json:"passwordGeneration,omitempty"`
	// InRoles contains SQL parent role names, using CNPG membership semantics.
	// +listType=set
	// +optional
	InRoles []SQLName `json:"inRoles,omitempty"`
	// Parameters contains the ALTER ROLE SET parameters managed by this resource.
	// +optional
	Parameters map[string]string `json:"parameters,omitempty"`
	// MembershipPolicy controls authoritative or operator-managed memberships.
	// +kubebuilder:default="authoritative"
	// +optional
	MembershipPolicy MembershipPolicy `json:"membershipPolicy,omitempty"`
	// ManagementPolicy chooses reconciliation or observation.
	// +kubebuilder:default="manage"
	// +optional
	ManagementPolicy ManagementPolicy `json:"managementPolicy,omitempty"`
	// ReclaimPolicy controls role deletion when this CR is deleted.
	// +kubebuilder:default="retain"
	// +optional
	ReclaimPolicy ReclaimPolicy `json:"databaseRoleReclaimPolicy,omitempty"`
}

// MembershipPolicy controls reconciliation of memberships not in inRoles.
// +kubebuilder:validation:Enum=authoritative;managed
type MembershipPolicy string

const (
	MembershipAuthoritative MembershipPolicy = "authoritative"
	MembershipManaged       MembershipPolicy = "managed"
)

// PasswordGeneration requests a generated basic-auth Secret.
type PasswordGeneration struct {
	// SecretName is the immutable local name of the generated password source.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	SecretName string `json:"secretName"`
}

// DatabaseRoleStatus reports role reconciliation and the applied password version.
type DatabaseRoleStatus struct {
	ResourceStatus `json:",inline"`
	// HasManagedSQL records successful SQL management and remains true during observation or temporary failure.
	// +optional
	HasManagedSQL bool `json:"hasManagedSQL,omitzero"`
	// GeneratedPasswordInitialized records that a persisted generated source was accepted. Source loss must not regenerate it.
	// +optional
	GeneratedPasswordInitialized bool `json:"generatedPasswordInitialized,omitzero"`
	// ManagedEndpoints preserves cleanup intent for each configured endpoint within this resource UID.
	// +optional
	ManagedEndpoints []RoleEndpointState `json:"managedEndpoints,omitempty"`
	// SecretResourceVersion identifies the password source version applied to SQL.
	// +optional
	SecretResourceVersion string `json:"secretResourceVersion,omitempty"`
}

// RoleEndpointState stores managed or possibly applied changes for one configured endpoint, not a physical server identity.
type RoleEndpointState struct {
	// Host is the normalized configured hostname.
	Host string `json:"host"`
	// Port is the effective configured TCP port.
	Port int32 `json:"port"`
	// Memberships records grantor-specific application and administrative bindings for retry cleanup.
	// +optional
	Memberships []RoleMembershipBinding `json:"memberships,omitempty"`
	// Parameters records role-wide setting names managed or possibly applied by this resource.
	// +optional
	Parameters []string `json:"parameters,omitempty"`
}

// RoleMembershipBinding identifies one SQL membership without storing transient role OIDs.
type RoleMembershipBinding struct {
	// Role is the parent SQL role.
	Role string `json:"role"`
	// Member is the SQL grantee.
	Member string `json:"member"`
	// Grantor is the SQL role that granted this binding.
	Grantor string `json:"grantor"`
	// AdminOption records administrative authority on this binding.
	// +optional
	AdminOption bool `json:"adminOption,omitzero"`
	// Administrative distinguishes operator administration from application membership ownership.
	// +optional
	Administrative bool `json:"administrative,omitzero"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=pgr
// +kubebuilder:printcolumn:name="Cluster",type=string,JSONPath=".spec.cluster.name"
// +kubebuilder:printcolumn:name="SQL Name",type=string,JSONPath=".status.effectiveName"
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"
// +kubebuilder:validation:XValidation:rule="(has(self.spec.name) ? self.spec.name : self.metadata.name) == (has(oldSelf.spec.name) ? oldSelf.spec.name : oldSelf.metadata.name)",message="effective SQL name is immutable"

// DatabaseRole is the API for a PostgreSQL role on an external server.
type DatabaseRole struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitzero"`
	Spec              DatabaseRoleSpec `json:"spec"`
	// +optional
	Status DatabaseRoleStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// DatabaseRoleList contains DatabaseRole resources.
type DatabaseRoleList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []DatabaseRole `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &DatabaseRole{}, &DatabaseRoleList{})
		return nil
	})
}
