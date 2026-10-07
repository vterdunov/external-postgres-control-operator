package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// EnsureOption declares the desired presence of a SQL object or privilege.
// +kubebuilder:validation:Enum=present;absent
type EnsureOption string

const (
	EnsurePresent EnsureOption = "present"
	EnsureAbsent  EnsureOption = "absent"
)

// ReclaimPolicy controls SQL cleanup when the Kubernetes resource is deleted.
// +kubebuilder:validation:Enum=retain;delete
type ReclaimPolicy string

const (
	ReclaimRetain ReclaimPolicy = "retain"
	ReclaimDelete ReclaimPolicy = "delete"
)

// ManagementPolicy selects reconciliation or read-only observation.
// +kubebuilder:validation:Enum=manage;observe
type ManagementPolicy string

const (
	ManagementManage  ManagementPolicy = "manage"
	ManagementObserve ManagementPolicy = "observe"
)

// SQLName is a nonempty PostgreSQL identifier. Server-specific byte limits are
// checked by the reconciler; identifiers are never truncated.
// +kubebuilder:validation:MinLength=1
// +kubebuilder:validation:Pattern="^[^\x00]+$"
type SQLName string

// LocalReference names a resource in the referring resource's namespace.
type LocalReference struct {
	// Name is the Kubernetes resource name.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
}

// ClusterReference identifies a PostgresCluster connection descriptor.
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="cluster reference is immutable"
type ClusterReference struct {
	// Name is the PostgresCluster resource name.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
	// Namespace defaults to the referring resource's namespace.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +optional
	Namespace string `json:"namespace,omitempty"`
}

// ResourceStatus reports reconciliation without including credentials or DSNs.
type ResourceStatus struct {
	// ObservedGeneration is the latest generation inspected by the reconciler.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Applied reports whether reconciliation succeeded.
	// +optional
	Applied *bool `json:"applied,omitempty"`
	// Message describes the most recent reconciliation result.
	// +optional
	Message string `json:"message,omitempty"`
	// EffectiveName is the resolved SQL name, including metadata.name fallback.
	// +optional
	EffectiveName string `json:"effectiveName,omitempty"`
	// AppliedGeneration is the latest successfully reconciled generation.
	// +optional
	AppliedGeneration int64 `json:"appliedGeneration,omitempty"`
	// LastSuccessfulReconcileTime records the most recent successful reconciliation.
	// +optional
	LastSuccessfulReconcileTime *metav1.Time `json:"lastSuccessfulReconcileTime,omitempty"`
	// Conditions report readiness and failures, keyed by condition type.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}
