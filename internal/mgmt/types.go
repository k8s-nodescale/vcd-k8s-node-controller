// Package mgmt serves the optional management UI: pool state, per-node
// consumption and the four actions an operator needs, all expressed as edits to
// the K8sWorkerPool object so the behaviour is the one kubectl would get.
package mgmt

// Share is one cell of the table: a percentage of the node's allocatable
// capacity and the colour it is painted with. Percent is nil when no source
// could answer - the first sample of a rate, or no node-exporter and no
// metrics-server on that node - and the page shows a dash rather than a colour.
type Share struct {
	Percent *float64 `json:"percent,omitempty"`
	Level   string   `json:"level,omitempty"`
}

// Colours. The boundaries are the operator's: green below 80%, orange from 80%
// to 90%, red at 90% and above. Every column group (requests, limits, real) is
// coloured on its own, so a node can be green on requests and red on real
// utilisation - which is exactly the case worth looking at.
const (
	LevelOK   = "ok"
	LevelWarn = "warn"
	LevelHigh = "high"
)

// Level maps a share to a colour. An unknown share has no colour.
func Level(p *float64) string {
	if p == nil {
		return ""
	}
	switch {
	case *p >= 90:
		return LevelHigh
	case *p >= 80:
		return LevelWarn
	default:
		return LevelOK
	}
}

// NewShare turns a used/total pair into a cell. A total of zero means the node
// has not reported its capacity yet, which is not the same as zero usage.
func NewShare(used, total int64) Share {
	if total <= 0 {
		return Share{}
	}
	p := float64(used) / float64(total) * 100
	return Share{Percent: &p, Level: Level(&p)}
}

// Usage is one column group of the node table.
type Usage struct {
	Millicores int64 `json:"millicores"`
	MemoryMB   int64 `json:"memoryMB"`
	Pods       int32 `json:"pods,omitempty"`
	CPU        Share `json:"cpu"`
	Memory     Share `json:"memory"`
}

// Allocatable is the node's schedulable capacity: the denominator of every
// share, because that is what the scheduler compares a request with.
type Allocatable struct {
	Millicores int64 `json:"millicores"`
	MemoryMB   int64 `json:"memoryMB"`
}

// Node is one row of the table.
type Node struct {
	Name       string `json:"name"`
	NodeName   string `json:"nodeName,omitempty"`
	Phase      string `json:"phase"`
	IP         string `json:"ip,omitempty"`
	ProviderID string `json:"providerID,omitempty"`
	Ready      bool   `json:"ready"`
	// Cordoned is true when the Kubernetes Node is unschedulable, which is a
	// state a Ready worker is in during every rollout and whenever an operator
	// parks one.
	Cordoned    bool        `json:"cordoned,omitempty"`
	Allocatable Allocatable `json:"allocatable"`
	Requests    Usage       `json:"requests"`
	Limits      Usage       `json:"limits"`
	Real        Usage       `json:"real"`
	// RealSource names where Real came from: "node-exporter",
	// "metrics-server", or empty when neither answered.
	RealSource string `json:"realSource,omitempty"`
	// Deleting is true while the pool is already removing this worker, so the
	// page does not offer the action twice.
	Deleting bool `json:"deleting,omitempty"`

	// The facts that decide whether removing this worker is safe. They come
	// from the API server, not from the platform: a node knows nothing about
	// its volumes, and the platform knows nothing about taints.
	Taints            []string            `json:"taints,omitempty"`
	StatefulSets      []StatefulSetOnNode `json:"statefulSets,omitempty"`
	PersistentVolumes []Volume            `json:"persistentVolumes,omitempty"`

	// Disks is the storage a worker can fill, one entry per mount, always in
	// the same order so the columns line up.
	Disks []Disk `json:"disks,omitempty"`
}

// StatefulSetOnNode is how many pods of one StatefulSet run on a worker. A
// StatefulSet is the workload that notices a node leaving: its replicas are
// pinned to volumes, so they come back one at a time and only after the
// volume is free again.
type StatefulSetOnNode struct {
	Name string `json:"name"`
	Pods int32  `json:"pods"`
}

// Volume is one PersistentVolume bound to a claim that a pod on this node
// mounts. The claim is what has to be free before the pod can start elsewhere.
type Volume struct {
	Name         string `json:"name"`
	Claim        string `json:"claim,omitempty"`
	Capacity     string `json:"capacity,omitempty"`
	StorageClass string `json:"storageClass,omitempty"`
}

// Flavor is the shape the pool asks for.
type Flavor struct {
	CPUs     int32  `json:"cpus"`
	MemoryMB int32  `json:"memoryMB"`
	OSDiskGB int32  `json:"osDiskGB,omitempty"`
	Template string `json:"template,omitempty"`
}

// Pool is everything the page needs about one pool.
type Pool struct {
	Namespace       string    `json:"namespace"`
	Name            string    `json:"name"`
	Replicas        int32     `json:"replicas"`
	Observed        int32     `json:"observedReplicas"`
	Ready           int32     `json:"readyReplicas"`
	Phase           string    `json:"phase,omitempty"`
	Message         string    `json:"message,omitempty"`
	MinReplicas     int32     `json:"minReplicas"`
	AllowLastWorker bool      `json:"allowLastWorker"`
	DeleteWorkers   []string  `json:"deleteWorkers,omitempty"`
	Flavor          Flavor    `json:"flavor"`
	Capacity        *Capacity `json:"capacity,omitempty"`
	Nodes           []Node    `json:"nodes"`
	// Warning carries the first condition message when the pool is not
	// converged, so the page can say why without the operator opening kubectl.
	Warning string `json:"warning,omitempty"`
}

// Disk is one filesystem on a worker: the mount, how big it is and how full.
// The size comes from the filesystem, not from spec.flavor, because the OS
// disk is an LVM volume that is smaller than the disk vCD was given.
type Disk struct {
	Mount   string   `json:"mount"`
	SizeGB  float64  `json:"sizeGB,omitempty"`
	UsedGB  float64  `json:"usedGB,omitempty"`
	Percent *float64 `json:"percent,omitempty"`
	Level   string   `json:"level,omitempty"`
}

// Capacity is the platform snapshot the controller publishes in status: what
// the vDC counts as used, what it caps, and how much pod network is left. The
// percentages are computed here like every other share, so the page only paints.
type Capacity struct {
	Model          string `json:"model,omitempty"`
	CPUMHz         int64  `json:"cpuMHz,omitempty"`
	CPULimitMHz    int64  `json:"cpuLimitMHz,omitempty"`
	MemoryMB       int64  `json:"memoryMB,omitempty"`
	MemoryLimitMB  int64  `json:"memoryLimitMB,omitempty"`
	StorageName    string `json:"storageName,omitempty"`
	StorageMB      int64  `json:"storageMB,omitempty"`
	StorageLimitMB int64  `json:"storageLimitMB,omitempty"`
	PodCIDR        string `json:"podCIDR,omitempty"`
	PodIPs         int64  `json:"podIPs,omitempty"`
	PodNodes       int32  `json:"podNodes,omitempty"`
	PodRemaining   int32  `json:"podRemainingNodes,omitempty"`
	PodBlocks      int32  `json:"podBlocks,omitempty"`
	PodBlocksUsed  int32  `json:"podBlocksUsed,omitempty"`
	At             string `json:"at,omitempty"`

	CPU     *Share `json:"cpu,omitempty"`
	Memory  *Share `json:"memory,omitempty"`
	Storage *Share `json:"storage,omitempty"`
}
