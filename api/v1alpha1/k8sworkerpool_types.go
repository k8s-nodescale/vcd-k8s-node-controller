package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// NodePhase describes the lifecycle phase of a managed worker node.
// +kubebuilder:validation:Enum=Pending;Provisioning;Joining;Ready;Draining;Deleting;Failed
type NodePhase string

const (
	NodePhasePending      NodePhase = "Pending"
	NodePhaseProvisioning NodePhase = "Provisioning"
	NodePhaseJoining      NodePhase = "Joining"
	NodePhaseReady        NodePhase = "Ready"
	NodePhaseDraining     NodePhase = "Draining"
	NodePhaseDeleting     NodePhase = "Deleting"
	NodePhaseFailed       NodePhase = "Failed"
)

// K8sWorkerPoolSpec defines the desired worker pool.
type K8sWorkerPoolSpec struct {
	// Replicas is the desired number of worker nodes.
	// +kubebuilder:validation:Minimum=0
	Replicas int32 `json:"replicas"`

	// NamePrefix is the DNS-safe prefix used to build worker names (e.g. myorg-cluster-prod).
	NamePrefix string `json:"namePrefix"`

	// Bootstrap carries OS/cloud-init inputs used to render new worker user-data.
	Bootstrap BootstrapConfig `json:"bootstrap,omitempty"`

	// Flavor describes the compute and disk shape of a worker.
	Flavor NodeFlavor `json:"flavor"`

	// VCD points at the target vCloud Director objects.
	VCD VCDRef `json:"vcd"`

	// IPPlan mirrors the Terraform ip_plan variable so the controller
	// never collides with Terraform-managed infrastructure or reserved VIPs.
	IPPlan IPPlan `json:"ipPlan"`

	// Join controls how new nodes join the cluster.
	Join JoinConfig `json:"join,omitempty"`

	// IngressPool describes the load-balancer pool new workers join once Ready.
	IngressPool IngressPool `json:"ingressPool,omitempty"`

	// ScaleDown controls safe removal of workers.
	ScaleDown ScaleDownPolicy `json:"scaleDown,omitempty"`
}

// BootstrapConfig carries the OS-level inputs for generated cloud-init.
type BootstrapConfig struct {
	OSUser           string   `json:"osUser,omitempty"`
	OSPasswordHash   string   `json:"osPasswordHash,omitempty"`
	SSHAuthorizedKey string   `json:"sshAuthorizedKey,omitempty"`
	NICName          string   `json:"nicName,omitempty"`
	DNS              []string `json:"dns,omitempty"`
	K8sVersion       string   `json:"k8sVersion,omitempty"`
	K8sMinorRepo     string   `json:"k8sMinorRepo,omitempty"`
}

// NodeFlavor describes a worker VM shape.
type NodeFlavor struct {
	// +kubebuilder:validation:Minimum=1
	CPUs int32 `json:"cpus"`

	// +kubebuilder:validation:Minimum=512
	MemoryMB int32 `json:"memoryMB"`

	// OSDiskGB overrides the system disk (bus 0/unit 0) of the cloned
	// template. Zero keeps the template size. Terraform-created workers use
	// disks_config.osdisk.sizegb; without this field a cloned worker inherits
	// the catalog template disk (10 GB on Ubuntu-24.04-ENG-Server).
	// +kubebuilder:validation:Minimum=0
	// +optional
	OSDiskGB int32 `json:"osDiskGB,omitempty"`

	// Template is the vCD catalog template name.
	Template string `json:"template,omitempty"`

	// Disks are additional data disks (mirrors Terraform diskw1/diskw2).
	Disks []DiskSpec `json:"disks,omitempty"`
}

// DiskSpec describes an internal disk attached to a worker VM.
type DiskSpec struct {
	SizeGB         int32  `json:"sizeGB"`
	Mount          string `json:"mount,omitempty"`
	BusType        string `json:"busType,omitempty"`
	BusNumber      int32  `json:"busNumber,omitempty"`
	UnitNumber     int32  `json:"unitNumber,omitempty"`
	StorageProfile string `json:"storageProfile,omitempty"`
}

// VCDRef identifies the vCloud Director placement.
type VCDRef struct {
	Org        string `json:"org"`
	VDC        string `json:"vdc"`
	VApp       string `json:"vapp"`
	OrgNetwork string `json:"orgNetwork"`
	Catalog    string `json:"catalog,omitempty"`
	CatalogOrg string `json:"catalogOrg,omitempty"`
}

// IPPlan mirrors Terraform's ip_plan variable.
type IPPlan struct {
	CIDR        string   `json:"cidr"`
	MNodeOffset int      `json:"mNodeOffset,omitempty"`
	WNodeOffset int      `json:"wNodeOffset,omitempty"`
	DVMOffset   int      `json:"dvmOffset,omitempty"`
	KubeAPILB   int      `json:"kubeapiLB,omitempty"`
	IngressLB   int      `json:"ingressLB,omitempty"`
	MasterCount int32    `json:"masterCount,omitempty"`
	Reserved    []string `json:"reserved,omitempty"`
}

// JoinConfig controls cluster join behaviour.
type JoinConfig struct {
	// Endpoint is the control-plane address, e.g. 192.168.100.2:6443.
	Endpoint string `json:"endpoint,omitempty"`

	// TokenTTL is how long the on-demand bootstrap token remains valid.
	TokenTTL metav1.Duration `json:"tokenTTL,omitempty"`

	// Labels applied to the Node after join.
	Labels map[string]string `json:"labels,omitempty"`

	// Taints applied to the Node after join (e.g. "key=value:NoSchedule").
	Taints []string `json:"taints,omitempty"`
}

// IngressPool describes the LB pool new workers are added to.
type IngressPool struct {
	// +kubebuilder:validation:Enum=nsxv;nsxt;""
	Type string `json:"type,omitempty"`
	Name string `json:"name,omitempty"`
	// Port is the node port the pool forwards to (not the virtual-server
	// port): NSX-V pool members point at the NodePort of the ingress
	// controller.
	Port int32 `json:"port,omitempty"`
	// Edge is the NSX-V edge gateway that owns the pool. NSX-V pools belong to
	// an edge gateway, so member removal needs it.
	Edge string `json:"edge,omitempty"`
}

// ScaleDownPolicy controls safe removal of workers.
type ScaleDownPolicy struct {
	// +kubebuilder:validation:Minimum=0
	MinReplicas  int32           `json:"minReplicas,omitempty"`
	DrainTimeout metav1.Duration `json:"drainTimeout,omitempty"`

	// DeleteWorkers names specific workers to remove, in order, regardless of
	// their position in the pool. An entry is processed like a scale-down:
	// cordon, ingress-pool removal, PDB-aware drain, volume-detach check, then
	// node and VM deletion. When the worker is gone the entry is removed from
	// this list and spec.replicas is decremented, so the worker is not
	// recreated. Unknown names are dropped with a warning event.
	// +optional
	DeleteWorkers []string `json:"deleteWorkers,omitempty"`

	// Force skips the data-loss guard for node-local volumes (hostPath/local
	// PVs that would become unreadable). It never skips PDBs or the detach
	// check. The same can be requested per node with the annotation
	// infra.example.com/force-delete=true.
	// +optional
	Force bool `json:"force,omitempty"`
}

// K8sWorkerPoolStatus is the observed state.
type K8sWorkerPoolStatus struct {
	ObservedReplicas int32              `json:"observedReplicas"`
	Ledger           IPLedger           `json:"ledger,omitempty"`
	Nodes            []WorkerNodeStatus `json:"nodes,omitempty"`
	Conditions       []metav1.Condition `json:"conditions,omitempty"`
}

// IPLedger is the persistent IP allocation ledger.
type IPLedger struct {
	NextIndex int      `json:"nextIndex,omitempty"`
	Used      []string `json:"used,omitempty"`
	Free      []string `json:"free,omitempty"`
}

// WorkerNodeStatus describes a single managed worker.
type WorkerNodeStatus struct {
	// Name is the vCD VM name, e.g. KION-WEB-DEV-wrk-03: it keeps the case of
	// spec.namePrefix because that is how vCD stores it.
	Name string `json:"name"`
	// NodeName is the name of the Kubernetes Node object as kubelet registered
	// it, always lower case. Every Kubernetes call (cordon, drain, volume
	// detach wait, Node delete) must use this name: passing Name asks for an
	// object that does not exist and the call silently does nothing.
	// +optional
	NodeName       string    `json:"nodeName,omitempty"`
	IP             string    `json:"ip,omitempty"`
	VMID           string    `json:"vmID,omitempty"`
	Phase          NodePhase `json:"phase,omitempty"`
	CPU            int32     `json:"cpu,omitempty"`
	MemoryMB       int32     `json:"memoryMB,omitempty"`
	DiskGB         int32     `json:"diskGB,omitempty"`
	K8sReady       bool      `json:"k8sReady,omitempty"`
	KubeletVersion string    `json:"kubeletVersion,omitempty"`
	Message        string    `json:"message,omitempty"`

	// DeleteStartedAt anchors the scaleDown.drainTimeout budget for a worker
	// that is being removed. It is cleared when the worker leaves status.
	// +optional
	DeleteStartedAt *metav1.Time `json:"deleteStartedAt,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=k8swp,scope=Namespaced
// +kubebuilder:printcolumn:name="Desired",type=integer,JSONPath=`.spec.replicas`
// +kubebuilder:printcolumn:name="Observed",type=integer,JSONPath=`.status.observedReplicas`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// K8sWorkerPool is the Schema for the k8sworkerpools API.
type K8sWorkerPool struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   K8sWorkerPoolSpec   `json:"spec,omitempty"`
	Status K8sWorkerPoolStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// K8sWorkerPoolList contains a list of K8sWorkerPool.
type K8sWorkerPoolList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []K8sWorkerPool `json:"items"`
}

func init() {
	SchemeBuilder.Register(&K8sWorkerPool{}, &K8sWorkerPoolList{})
}
