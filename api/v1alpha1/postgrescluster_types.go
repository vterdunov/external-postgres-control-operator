package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// PostgresClusterSpec defines a connection to an external writable PostgreSQL server.
type PostgresClusterSpec struct {
	// Connection identifies the administrative endpoint and database.
	Connection PostgresConnection `json:"connection"`
	// CredentialsSecret supplies a local basic-auth Secret with username/password.
	CredentialsSecret LocalReference `json:"credentialsSecret"`
	// TLS configures transport security. Omission uses verify-full and system trust.
	// +optional
	TLS *PostgresTLS `json:"tls,omitempty"`
	// Access optionally selects authorized Namespace objects. Without it, only
	// references from the PostgresCluster namespace are authorized.
	// +optional
	Access *ClusterAccess `json:"access,omitempty"`
	// ReconcileInterval is the interval between periodic reconciliations.
	// +kubebuilder:default="5m"
	// +optional
	ReconcileInterval PositiveDuration `json:"reconcileInterval,omitempty"`
	// ConnectTimeout limits connection establishment.
	// +kubebuilder:default="10s"
	// +optional
	ConnectTimeout PositiveDuration `json:"connectTimeout,omitempty"`
	// StatementTimeout limits each SQL statement.
	// +kubebuilder:default="30s"
	// +optional
	StatementTimeout PositiveDuration `json:"statementTimeout,omitempty"`
}

// PositiveDuration is a positive Go duration expressed with h, m, s, ms, us, or ns.
// +kubebuilder:validation:MaxLength=64
// +kubebuilder:validation:Pattern="^([0-9]+([.][0-9]+)?(h|m|s|ms|us|ns))+$"
// +kubebuilder:validation:XValidation:rule="duration(self) > duration('0s')",message="duration must be positive"
type PositiveDuration string

// PostgresConnection identifies the administrative connection target.
type PostgresConnection struct {
	// Host is the PostgreSQL hostname or IP address.
	// +kubebuilder:validation:MinLength=1
	Host string `json:"host"`
	// Port is the PostgreSQL TCP port.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +kubebuilder:default=5432
	// +optional
	Port int32 `json:"port,omitempty"`
	// Database is the administrative database used for cluster-wide operations.
	// +kubebuilder:default="postgres"
	// +optional
	Database SQLName `json:"database,omitempty"`
}

// PostgresTLS controls the operator's administrative TLS connection.
type PostgresTLS struct {
	// Mode is the PostgreSQL sslmode. Defaults to verify-full.
	// +kubebuilder:validation:Enum=disable;allow;prefer;require;verify-ca;verify-full
	// +kubebuilder:default="verify-full"
	// +optional
	Mode string `json:"mode,omitempty"`
	// CASecret identifies the local Secret key containing trusted CA certificates.
	// +optional
	CASecret *SecretKeyReference `json:"caSecret,omitempty"`
	// ClientCertificateSecret contains tls.crt/tls.key for the operator only.
	// +optional
	ClientCertificateSecret *LocalReference `json:"clientCertificateSecret,omitempty"`
}

// SecretKeyReference selects a key in a namespace-local Secret.
type SecretKeyReference struct {
	// Name is the Secret name.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
	// Key is the Secret data key containing CA certificates.
	// +kubebuilder:validation:MinLength=1
	Key string `json:"key"`
}

// ClusterAccess authorizes references by namespace labels.
type ClusterAccess struct {
	// NamespaceSelector selects both local and remote namespaces. Nil selects
	// none; an explicitly empty selector selects all namespaces.
	// +optional
	NamespaceSelector *metav1.LabelSelector `json:"namespaceSelector,omitempty"`
}

// PostgresClusterStatus defines the observed connection state.
type PostgresClusterStatus struct {
	ResourceStatus `json:",inline"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=pgc
// +kubebuilder:printcolumn:name="Host",type=string,JSONPath=".spec.connection.host"
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// PostgresCluster is the API for an external PostgreSQL connection descriptor.
type PostgresCluster struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitzero"`
	Spec              PostgresClusterSpec `json:"spec"`
	// +optional
	Status PostgresClusterStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// PostgresClusterList contains PostgresCluster resources.
type PostgresClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []PostgresCluster `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &PostgresCluster{}, &PostgresClusterList{})
		return nil
	})
}
