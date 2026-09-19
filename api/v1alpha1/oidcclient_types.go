package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// UpdatePolicy controls how the operator handles drift between the spec and
// the client registered in tsidp.
type UpdatePolicy string

const (
	// UpdatePolicyRecreate deletes and re-registers the client on drift.
	// The client_id and client_secret change; consuming workloads must
	// re-read the Secret.
	UpdatePolicyRecreate UpdatePolicy = "Recreate"
	// UpdatePolicyInPlace updates name and redirect URIs through tsidp's
	// admin form endpoint, preserving client_id and client_secret. This
	// relies on tsidp's HTML UI contract, which is not a stable API.
	UpdatePolicyInPlace UpdatePolicy = "InPlace"
	// UpdatePolicyBlock leaves the registration untouched and reports
	// Ready=False (reason DriftBlocked) until a human intervenes.
	UpdatePolicyBlock UpdatePolicy = "Block"
)

// DeletionPolicy controls what happens to the tsidp registration when the
// OIDCClient object is deleted.
type DeletionPolicy string

const (
	// DeletionPolicyDeregister deletes the client from tsidp, which also
	// revokes its outstanding auth codes and tokens.
	DeletionPolicyDeregister DeletionPolicy = "Deregister"
	// DeletionPolicyOrphan leaves the client registered in tsidp.
	DeletionPolicyOrphan DeletionPolicy = "Orphan"
)

// SecretTemplate customizes metadata on the generated credentials Secret.
type SecretTemplate struct {
	// +optional
	Labels map[string]string `json:"labels,omitempty"`
	// +optional
	Annotations map[string]string `json:"annotations,omitempty"`
}

// OIDCClientSpec defines the desired OIDC client registration in tsidp.
type OIDCClientSpec struct {
	// ClientName is the human-readable client name registered with tsidp.
	// Defaults to the object name. The operator appends an ownership
	// fingerprint of the form "[k8s:<namespace>/<name>/<uid8>]" to the
	// registered name so it can recognize and adopt its registrations.
	// +kubebuilder:validation:MaxLength=128
	// +kubebuilder:validation:XValidation:rule="!self.contains('[k8s:')",message="clientName must not contain the operator ownership marker '[k8s:'"
	// +optional
	ClientName string `json:"clientName,omitempty"`

	// RedirectURIs are the allowed OAuth redirect URIs. tsidp matches them
	// exactly and case-sensitively at /authorize, so entries must be the
	// final canonical strings the application sends.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=32
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:MaxLength=512
	// The scheme blocklist mirrors tsidp's own /edit validation exactly:
	// anything admitted here but rejected there would register fine and
	// then permanently wedge InPlace updates.
	// +kubebuilder:validation:XValidation:rule="self.all(u, !['javascript:','data:','file:','vbscript:','about:','blob:','filesystem:','chrome:','chrome-extension:','ftp:','mailto:'].exists(p, u.lowerAscii().startsWith(p)))",message="redirect URI scheme is not allowed (mirrors tsidp's blocklist: javascript, data, file, vbscript, about, blob, filesystem, chrome, chrome-extension, ftp, mailto)"
	// +kubebuilder:validation:XValidation:rule="self.all(u, u.contains(':'))",message="redirect URIs must be absolute URIs"
	RedirectURIs []string `json:"redirectUris"`

	// +kubebuilder:validation:Enum=client_secret_basic;client_secret_post;none
	// +kubebuilder:default=client_secret_basic
	// +optional
	TokenEndpointAuthMethod string `json:"tokenEndpointAuthMethod,omitempty"`

	// GrantTypes defaults to ["authorization_code"] server-side.
	// +optional
	GrantTypes []string `json:"grantTypes,omitempty"`

	// ResponseTypes defaults to ["code"] server-side.
	// +optional
	ResponseTypes []string `json:"responseTypes,omitempty"`

	// +optional
	Scope string `json:"scope,omitempty"`

	// +optional
	ClientURI string `json:"clientUri,omitempty"`

	// +optional
	LogoURI string `json:"logoUri,omitempty"`

	// +optional
	Contacts []string `json:"contacts,omitempty"`

	// +kubebuilder:validation:Enum=web;native
	// +kubebuilder:default=web
	// +optional
	ApplicationType string `json:"applicationType,omitempty"`

	// SecretName is the name of the Secret the credentials are written to,
	// created in the same namespace as this object. Defaults to
	// "<name>-oidc".
	// +optional
	SecretName string `json:"secretName,omitempty"`

	// +optional
	SecretTemplate *SecretTemplate `json:"secretTemplate,omitempty"`

	// +kubebuilder:validation:Enum=Recreate;InPlace;Block
	// +kubebuilder:default=Recreate
	// +optional
	UpdatePolicy UpdatePolicy `json:"updatePolicy,omitempty"`

	// +kubebuilder:validation:Enum=Deregister;Orphan
	// +kubebuilder:default=Deregister
	// +optional
	DeletionPolicy DeletionPolicy `json:"deletionPolicy,omitempty"`
}

// OIDCClientStatus reflects the observed state of the registration.
type OIDCClientStatus struct {
	// ClientID is the client_id assigned by tsidp. It is the durable link
	// between this object and the tsidp registration.
	// +optional
	ClientID string `json:"clientId,omitempty"`

	// SecretName is the Secret the credentials were written to.
	// +optional
	SecretName string `json:"secretName,omitempty"`

	// Issuer is the OIDC issuer URL reported by tsidp's discovery document.
	// +optional
	Issuer string `json:"issuer,omitempty"`

	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// +listType=map
	// +listMapKey=type
	// +patchStrategy=merge
	// +patchMergeKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
}

// ConditionReady is the single condition type set on OIDCClient status:
// True when registered and the Secret is in sync, False with a specific
// reason (TsidpUnreachable, DriftBlocked, InPlaceUpdateFailed, ...) when the
// operator cannot converge.
const ConditionReady = "Ready"

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Client ID",type=string,JSONPath=`.status.clientId`
// +kubebuilder:printcolumn:name="Secret",type=string,JSONPath=`.status.secretName`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// OIDCClient declares an OIDC client that the operator registers with tsidp,
// materializing the credentials into a Kubernetes Secret.
type OIDCClient struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   OIDCClientSpec   `json:"spec,omitempty"`
	Status OIDCClientStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// OIDCClientList contains a list of OIDCClient.
type OIDCClientList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []OIDCClient `json:"items"`
}

func init() {
	SchemeBuilder.Register(&OIDCClient{}, &OIDCClientList{})
}
