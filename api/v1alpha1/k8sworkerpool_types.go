package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// NodePhase describes the lifecycle phase of a managed worker node.
// +kubebuilder:validation:Enum=Pending;Provisioning;Joining;Ready;Missing;Resizing;Draining;Deleting;Failed
type NodePhase string

const (
	NodePhasePending      NodePhase = "Pending"
	NodePhaseProvisioning NodePhase = "Provisioning"
	NodePhaseJoining      NodePhase = "Joining"
	NodePhaseReady        NodePhase = "Ready"
	NodePhaseDraining     NodePhase = "Draining"
	NodePhaseDeleting     NodePhase = "Deleting"
	NodePhaseFailed       NodePhase = "Failed"
	// NodePhaseMissing marks a slot whose VM is gone from vCD. The address
	// stays reserved and the name stays claimed; the pool reports the loss
	// instead of provisioning a replacement under a new name.
	NodePhaseMissing NodePhase = "Missing"
	// NodePhaseResizing marks a worker the rollout is taking through
	// cordon -> drain -> power off -> resize -> power on -> Ready.
	NodePhaseResizing NodePhase = "Resizing"
)

// Reconciliation policies.
const (
	// ReconciliationNotify reports a worker that disappeared outside the
	// controller and keeps its slot reserved. It never creates a VM for it.
	ReconciliationNotify = "Notify"
	// ReconciliationAutoHeal recreates a missing worker into its own slot
	// (same name, same address) with bounded retries.
	ReconciliationAutoHeal = "AutoHeal"
)

// Rollout strategies and orders.
const (
	RolloutStrategyRolling = "Rolling"
	RolloutStrategyManual  = "Manual"

	RolloutOrderDescending = "Descending"
	RolloutOrderAscending  = "Ascending"

	// AnnotationDomain is both the API group and the prefix of every node
	// annotation the controller reads. Nothing provider-specific belongs in
	// this namespace: these annotations describe a Kubernetes node, not a VM.
	AnnotationDomain = "k8s-nodescale.github.io"

	// ResizeAnnotation opts a single worker into a Manual rollout:
	// kubectl annotate node <worker> k8s-nodescale.github.io/resize=allow
	ResizeAnnotation = AnnotationDomain + "/resize"
)

// Rollout phases reported in status.rollout.
const (
	RolloutPhaseIdle        = "Idle"
	RolloutPhaseProgressing = "Progressing"
	RolloutPhasePaused      = "Paused"
	RolloutPhaseFailed      = "Failed"
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

	// Flavor describes the compute and disk shape of a worker. It is the
	// source of truth for a rollout: a worker whose VM does not match it is a
	// resize candidate (see spec.rollout).
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

	// Reconciliation controls what happens when a worker's VM disappears
	// outside the controller (deleted by hand in vCD, or lost with a vCD
	// failure). The default, Notify, keeps the slot reserved and reports it.
	// +optional
	Reconciliation ReconciliationPolicy `json:"reconciliation,omitempty"`

	// Rollout controls compute-spec changes (CPU, memory, system disk). The
	// zero value means a Rolling rollout, one worker at a time.
	// +optional
	Rollout RolloutPolicy `json:"rollout,omitempty"`

	// Provisioning bounds the window between "the VM exists" and "kubelet has
	// registered a Node". A worker that stays in that window longer is parked
	// as Failed so the pool reports it instead of waiting forever.
	// +optional
	Provisioning ProvisioningPolicy `json:"provisioning,omitempty"`
}

// ProvisioningPolicy bounds how long a created VM may run without its Node.
type ProvisioningPolicy struct {
	// Timeout is how long a worker may sit in Provisioning with a VM but no
	// Kubernetes Node object. When it elapses the worker's phase becomes
	// Failed and the pool reports Ready=False/WorkerFailed; the VM is left
	// running for inspection, and a late registration still promotes the
	// worker to Ready. The default is 15m; a zero value means the default.
	// +kubebuilder:default="15m"
	// +optional
	Timeout metav1.Duration `json:"timeout,omitempty"`
}

// ReconciliationPolicy decides what the controller does with a worker whose VM
// is gone from vCD.
type ReconciliationPolicy struct {
	// Policy is Notify or AutoHeal.
	// +kubebuilder:validation:Enum=Notify;AutoHeal
	// +kubebuilder:default=Notify
	// +optional
	Policy string `json:"policy,omitempty"`

	// AutoHealRetries is how many recreations AutoHeal attempts for a slot
	// before parking it as Failed. Only used with policy AutoHeal.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=3
	// +optional
	AutoHealRetries int32 `json:"autoHealRetries,omitempty"`

	// AutoHealBackoff is the minimum time between two AutoHeal attempts for
	// the same slot.
	// +kubebuilder:default="2m"
	// +optional
	AutoHealBackoff metav1.Duration `json:"autoHealBackoff,omitempty"`
}

// RolloutPolicy controls how spec.flavor changes are applied to existing workers.
type RolloutPolicy struct {
	// Strategy is Rolling (resize automatically) or Manual (only workers
	// annotated k8s-nodescale.github.io/resize=allow are resized).
	// +kubebuilder:validation:Enum=Rolling;Manual
	// +kubebuilder:default=Rolling
	// +optional
	Strategy string `json:"strategy,omitempty"`

	// MaxUnavailable bounds how many workers may be out of service at once.
	// It is an upper bound only: PodDisruptionBudgets are the real brake.
	// +kubebuilder:default=1
	// +optional
	MaxUnavailable intstr.IntOrString `json:"maxUnavailable,omitempty"`

	// Order picks which end of the pool is rolled first. Descending (the
	// default) resizes the highest ordinal first, so wrk-01 moves last.
	// +kubebuilder:validation:Enum=Descending;Ascending
	// +kubebuilder:default=Descending
	// +optional
	Order string `json:"order,omitempty"`

	// PauseOnFailure stops the rollout when a worker does not come back.
	// +kubebuilder:default=true
	// +optional
	PauseOnFailure *bool `json:"pauseOnFailure,omitempty"`

	// ReadyTimeout bounds how long a resized worker may take to become Ready.
	// +kubebuilder:default="10m"
	// +optional
	ReadyTimeout metav1.Duration `json:"readyTimeout,omitempty"`
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

	// DVMOffset is the address offset of the deployment VM, the bootstrap
	// host Terraform creates. The controller reserves that address so no
	// worker is ever handed it. It lives here rather than in spec.ipPlan
	// because only the vCD bootstrap has a DVM; the address plan itself is
	// provider-neutral.
	// +optional
	DVMOffset int `json:"dvmOffset,omitempty"`
}

// IPPlan mirrors Terraform's ip_plan variable. It carries only the
// provider-neutral address plan.
type IPPlan struct {
	CIDR        string   `json:"cidr"`
	MNodeOffset int      `json:"mNodeOffset,omitempty"`
	WNodeOffset int      `json:"wNodeOffset,omitempty"`
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
	// MinReplicas is the floor for the workers that can serve traffic. A
	// removal that would take a serving worker out and leave fewer than this
	// many is refused; writing off a slot whose VM is already gone takes
	// nothing out of service and is never refused by the floor. It defaults to
	// 1: emptying the pool has to be asked for with minReplicas: 0.
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=0
	MinReplicas  int32           `json:"minReplicas,omitempty"`
	DrainTimeout metav1.Duration `json:"drainTimeout,omitempty"`

	// DeleteWorkers names specific workers to remove, in order, regardless of
	// their position in the pool. An entry is processed like a scale-down:
	// cordon, ingress-pool removal, PDB-aware drain, volume-detach check, then
	// node and VM deletion. When the worker is gone the entry is removed from
	// this list and spec.replicas is decremented, so the worker is not
	// recreated. Unknown names are dropped with a warning event; naming a
	// worker that is already gone only aligns spec.replicas with reality.
	// +optional
	DeleteWorkers []string `json:"deleteWorkers,omitempty"`

	// Force skips the data-loss guard for node-local volumes (hostPath/local
	// PVs that would become unreadable). It never skips PDBs or the detach
	// check; a budget is bypassed only by the escalation described on
	// WaitForPDB. The same can be requested per node with the annotation
	// k8s-nodescale.github.io/force-delete=true.
	// +optional
	Force bool `json:"force,omitempty"`

	// WaitForPDB keeps the worker in Failed when drainTimeout is spent and a
	// PodDisruptionBudget still allows no disruption. By default the removal
	// escalates instead: the pods that are left are deleted through the API
	// (not the eviction API), so a single-replica workload whose
	// minAvailable equals its replica count cannot pin a worker the pool has
	// to remove. The event and the status message name every budget that was
	// bypassed. Set this where a PDB must never be overridden and a human
	// should decide.
	// +optional
	WaitForPDB bool `json:"waitForPDB,omitempty"`

	// AllowLastWorker permits a request that would leave the pool with zero
	// workers. Emptying the pool is a deliberate owner decision: every pod on
	// the last worker is evicted, and a budget that blocks the drain is
	// bypassed after drainTimeout unless waitForPDB asks for a human. It
	// requires minReplicas: 0; a spec that sets this flag with a higher floor
	// is rejected as InvalidSpec instead of accepting a flag that could never
	// be honoured. Two refusals survive the flag because they are not policy:
	// the worker this controller itself runs on, and a worker holding
	// node-local volumes.
	// +optional
	AllowLastWorker bool `json:"allowLastWorker,omitempty"`
}

// K8sWorkerPoolStatus is the observed state.
type K8sWorkerPoolStatus struct {
	ObservedReplicas int32              `json:"observedReplicas"`
	Ledger           IPLedger           `json:"ledger,omitempty"`
	Nodes            []WorkerNodeStatus `json:"nodes,omitempty"`
	Conditions       []metav1.Condition `json:"conditions,omitempty"`
	Rollout          RolloutStatus      `json:"rollout,omitempty"`
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
	NodeName string `json:"nodeName,omitempty"`
	IP       string `json:"ip,omitempty"`
	// ProviderID is the VM's identifier in the underlying platform, in the
	// form that platform owns (today: the vCD VM id, urn:vcloud:vm:...).
	// The controller only hands it back to the provider, so a second
	// platform needs no change to this field.
	// +optional
	ProviderID     string    `json:"providerID,omitempty"`
	Phase          NodePhase `json:"phase,omitempty"`
	CPU            int32     `json:"cpu,omitempty"`
	MemoryMB       int32     `json:"memoryMB,omitempty"`
	DiskGB         int32     `json:"diskGB,omitempty"`
	K8sReady       bool      `json:"k8sReady,omitempty"`
	KubeletVersion string    `json:"kubeletVersion,omitempty"`
	Message        string    `json:"message,omitempty"`

	// Flavor is what the VM actually runs with, read back from vCD. It is the
	// rollout's observed side: spec.flavor is the desired side.
	// +optional
	Flavor *FlavorStatus `json:"flavor,omitempty"`

	// DeleteStartedAt anchors the scaleDown.drainTimeout budget for a worker
	// that is being removed. It is cleared when the worker leaves status.
	// +optional
	DeleteStartedAt *metav1.Time `json:"deleteStartedAt,omitempty"`

	// HealAttempts counts AutoHeal attempts for a missing slot. It is reset
	// when the slot becomes Ready again.
	// +optional
	HealAttempts int32 `json:"healAttempts,omitempty"`

	// HealLastAttempt is when AutoHeal last acted on this slot.
	// +optional
	HealLastAttempt *metav1.Time `json:"healLastAttempt,omitempty"`

	// Resize tracks the rollout of this worker, when one is in progress.
	// +optional
	Resize *ResizeStatus `json:"resize,omitempty"`

	// BootstrapTokenID is the id of the bootstrap token this worker's guest was
	// given at create time: the half of "id.secret" that is not a credential by
	// itself. It lets the controller renew a slow install and revoke the token
	// once the Node is Ready without keeping the secret half in status.
	// +optional
	BootstrapTokenID string `json:"bootstrapTokenID,omitempty"`

	// ProvisioningStartedAt is when this worker entered Provisioning, i.e.
	// when the controller began waiting for kubelet to register a Node for an
	// existing VM. It anchors spec.provisioning.timeout and is cleared once
	// the Node exists. A worker adopted with a registered Node never gets it.
	// +optional
	ProvisioningStartedAt *metav1.Time `json:"provisioningStartedAt,omitempty"`
}

// FlavorStatus is the compute shape a VM runs with, as reported by vCD.
type FlavorStatus struct {
	CPUs     int32 `json:"cpus,omitempty"`
	MemoryMB int32 `json:"memoryMB,omitempty"`
	OSDiskGB int32 `json:"osDiskGB,omitempty"`
}

// ResizeStatus is the per-worker rollout state.
type ResizeStatus struct {
	// Phase is Cordoned, Draining, Stopped, Resizing, Booting, WaitingReady,
	// Done or Failed.
	Phase     string       `json:"phase,omitempty"`
	StartedAt *metav1.Time `json:"startedAt,omitempty"`
	Message   string       `json:"message,omitempty"`
}

// RolloutStatus is the pool-wide rollout state.
type RolloutStatus struct {
	// Phase is Idle, Progressing, Paused or Failed.
	Phase         string        `json:"phase,omitempty"`
	TargetFlavor  *FlavorStatus `json:"targetFlavor,omitempty"`
	WorkersTotal  int32         `json:"workersTotal,omitempty"`
	WorkersDone   int32         `json:"workersDone,omitempty"`
	CurrentWorker string        `json:"currentWorker,omitempty"`
	LastError     string        `json:"lastError,omitempty"`
	StartedAt     *metav1.Time  `json:"startedAt,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:subresource:scale:specpath=.spec.replicas,statuspath=.status.observedReplicas
// +kubebuilder:resource:shortName=k8swp,scope=Namespaced
// +kubebuilder:printcolumn:name="Desired",type=integer,JSONPath=`.spec.replicas`
// +kubebuilder:printcolumn:name="Observed",type=integer,JSONPath=`.status.observedReplicas`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Rollout",type=string,JSONPath=`.status.rollout.phase`
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
