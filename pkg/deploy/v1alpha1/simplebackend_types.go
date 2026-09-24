package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SimpleBackendSpec is one container, the entry deploy tier: the app owner
// brings a built, pinned image and a handful of facts about how to run it.
//
// Field-by-field merge decisions (recorded here so the next reader does not
// have to re-derive them):
//
//   - Image: pinned AND registry-qualified (forge's invariant), validated in
//     Go so it holds on every path. Which registries are ALLOWED is hosted
//     policy and stays in control-plane.
//   - Replicas: absent. Both sides agreed, and forge's reason is the right
//     one. StorageGiB renders a ReadWriteOnce PVC, and a multi-replica
//     Deployment mounting one RWO volume either wedges on rolling update or
//     refuses to schedule across nodes. The tier cannot express a
//     StatefulSet, so a replicas knob would silently break exactly the
//     workloads that set StorageGiB. Horizontal scale is the next tier up.
//     control-plane's deployrender Replicas was the drift, not a
//     competing design.
//   - Domains replaces forge's single required `domain` (synthesis). A
//     public backend no longer NEEDS a hostname from its author. Hosted
//     allocates one (control-plane's collision-safe model) and self-hosted
//     derives one from the env's gateway. Either way it is reported in
//     status. Requiring every public backend to invent a domain violated
//     forge's own working-defaults rule, and on shared infrastructure a
//     user-asserted hostname is a takeover vector unless it is verified.
//     Custom domains are still expressible: hosted verifies them and
//     self-hosted trusts them.
//   - Cluster/Namespace: absent — target facts, see the package doc.
type SimpleBackendSpec struct {
	// Image is the container image, FULLY QUALIFIED (explicit registry host)
	// and PINNED (a tag or a digest). forge neither builds it nor prefixes a
	// registry onto it.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=500
	Image string `json:"image"`

	// Ports are the TCP ports the container listens on: container ports
	// always, Service ports unless Network is none. The first port is the
	// one a hostname routes to.
	// +optional
	// +kubebuilder:validation:MaxItems=16
	// +kubebuilder:validation:items:Minimum=1
	// +kubebuilder:validation:items:Maximum=65535
	Ports []int32 `json:"ports,omitempty"`

	// Network is the addressability mode. Default private.
	// +optional
	// +kubebuilder:default=private
	Network Network `json:"network,omitempty"`

	// Domains are CUSTOM hostnames, served in addition to the default one.
	// Only meaningful for a public backend.
	// +optional
	// +kubebuilder:validation:MaxItems=8
	Domains []string `json:"domains,omitempty"`

	// Env is the container environment.
	// +optional
	// +listType=map
	// +listMapKey=name
	Env []EnvVar `json:"env,omitempty"`

	// Resources is the compute request and limit. Omitted means the entry
	// shape (250m / 1 GiB).
	// +optional
	Resources Resources `json:"resources,omitempty"`

	// HealthCheck configures liveness and readiness. Omitted means no
	// probes, which is correct for a worker that listens on nothing.
	// +optional
	HealthCheck *HealthCheck `json:"healthCheck,omitempty"`

	// StorageGiB provisions a ReadWriteOnce PersistentVolumeClaim of this
	// many GiB, mounted at /data. Zero means a stateless backend.
	//
	// A bound PVC keeps existing while the workload is scaled to zero, and
	// on hosted it keeps billing. That is deliberate: it is what the cloud
	// itself charges.
	// +optional
	// +kubebuilder:validation:Minimum=0
	StorageGiB int32 `json:"storageGiB,omitempty"`
}

// SimpleBackendStatus is the observed state of a SimpleBackend.
type SimpleBackendStatus struct {
	WorkloadStatus `json:",inline"`

	// ServiceName is the Service a hostname routes to. Empty when Network is
	// none.
	// +optional
	ServiceName string `json:"serviceName,omitempty"`

	// WorkloadName is the Deployment carrying the pod.
	// +optional
	WorkloadName string `json:"workloadName,omitempty"`

	// ReadyReplicas is the API server's own observed ready count. It is
	// this resource's liveness proof, and it is why a backend that never
	// started bills nothing.
	// +optional
	// +kubebuilder:validation:Minimum=0
	ReadyReplicas int32 `json:"readyReplicas,omitempty"`

	// ObservedImage is the image the running workload was CONFIRMED to
	// carry. It lags spec.image until a rollout completes, and that gap is
	// the drift signal.
	// +optional
	ObservedImage string `json:"observedImage,omitempty"`

	// LastReadyAt is nil until the backend has had a ready replica at least
	// once. A nil value is the machine-readable form of "declared, never
	// ran".
	// +optional
	LastReadyAt *metav1.Time `json:"lastReadyAt,omitempty"`
}

// SimpleBackend is a single container running in a cluster forge did not
// create.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=sb,categories=forge
// +kubebuilder:printcolumn:name="Network",type=string,JSONPath=`.spec.network`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Hostname",type=string,JSONPath=`.status.hostname`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type SimpleBackend struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SimpleBackendSpec   `json:"spec"`
	Status SimpleBackendStatus `json:"status,omitempty"`
}

// SimpleBackendList contains a list of SimpleBackend.
//
// +kubebuilder:object:root=true
type SimpleBackendList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SimpleBackend `json:"items"`
}

// EffectiveNetwork resolves an unset Network to private.
//
// An UNRECOGNIZED value also resolves to private rather than public. Failing
// toward LESS exposure is the only safe direction for a value that Validate
// or the API server should already have refused.
func (s SimpleBackendSpec) EffectiveNetwork() Network {
	switch s.Network {
	case NetworkPublic, NetworkNone:
		return s.Network
	default:
		return NetworkPrivate
	}
}

// ServesTraffic reports whether the backend gets a Service object.
func (s SimpleBackendSpec) ServesTraffic() bool { return s.EffectiveNetwork() != NetworkNone }

// IsPublic reports whether the backend gets a hostname.
func (s SimpleBackendSpec) IsPublic() bool { return s.EffectiveNetwork() == NetworkPublic }

// RoutedPort is the port a hostname routes to: the first declared port, or 0.
func (s SimpleBackendSpec) RoutedPort() int32 {
	if len(s.Ports) == 0 {
		return 0
	}
	return s.Ports[0]
}
