package mgmt

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1 "github.com/k8s-nodescale/vcd-k8s-node-controller/api/v1alpha1"
)

type Collector struct {
	Client         client.Client
	Metrics        MetricsAPI
	ExporterLabels string
	ExporterPort   int
	Timeout        time.Duration
	Refresh        time.Duration
	Now            func() time.Time

	mu   sync.Mutex
	last map[string]exporterSample
	snap []Pool
	at   time.Time
	err  error
}

func (c *Collector) CollectOnce(ctx context.Context) ([]Pool, error) {
	pools, err := c.collectPools(ctx)
	c.mu.Lock()
	c.at = c.now()
	c.err = err
	if err == nil {
		c.snap = pools
	}
	c.mu.Unlock()
	return pools, err
}

func (c *Collector) Snapshot() ([]Pool, time.Time, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.snap, c.at, c.err
}

var logf = func(format string, args ...any) {}

func SetLogger(f func(format string, args ...any)) { logf = f }

func (c *Collector) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Collector) Run(ctx context.Context) {
	period := c.Refresh
	if period <= 0 {
		period = 15 * time.Second
	}
	if _, err := c.CollectOnce(ctx); err != nil {
		logf("first collection failed: %v", err)
	}
	t := time.NewTicker(period)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, err := c.CollectOnce(ctx); err != nil {
				logf("collection failed: %v", err)
			}
		}
	}
}

// collectPools is one pass: pools, nodes, pods, then the real usage.
func (c *Collector) collectPools(ctx context.Context) ([]Pool, error) {
	pools := &infrav1.K8sWorkerPoolList{}
	if err := c.Client.List(ctx, pools); err != nil {
		return nil, err
	}
	nodes := &corev1.NodeList{}
	if err := c.Client.List(ctx, nodes); err != nil {
		return nil, err
	}
	pods := &corev1.PodList{}
	if err := c.Client.List(ctx, pods); err != nil {
		return nil, err
	}

	byNode := make(map[string]*corev1.Node, len(nodes.Items))
	for i := range nodes.Items {
		byNode[strings.ToLower(nodes.Items[i].Name)] = &nodes.Items[i]
	}
	used := map[string]*nodeUsage{}
	for i := range pods.Items {
		addPod(used, &pods.Items[i])
	}

	// Volumes are the one thing a node cannot tell us about itself. A failure
	// to list them degrades the detail view, not the page.
	volumes := map[string][]Volume{}
	pvs := &corev1.PersistentVolumeList{}
	if err := c.Client.List(ctx, pvs); err == nil {
		for i := range pvs.Items {
			pv := &pvs.Items[i]
			if pv.Spec.ClaimRef == nil {
				continue
			}
			key := pv.Spec.ClaimRef.Namespace + "/" + pv.Spec.ClaimRef.Name
			v := Volume{Name: pv.Name, Claim: key, StorageClass: pv.Spec.StorageClassName}
			if q, ok := pv.Spec.Capacity[corev1.ResourceStorage]; ok {
				v.Capacity = q.String()
			}
			volumes[key] = append(volumes[key], v)
		}
	}

	exporters := c.exporterEndpoints(ctx)
	var real map[string]realUsage
	if c.Metrics != nil {
		real = c.metricsUsage(ctx)
	}

	out := make([]Pool, 0, len(pools.Items))
	for i := range pools.Items {
		out = append(out, c.poolView(ctx, &pools.Items[i], byNode, used, volumes, exporters, real))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

func (c *Collector) poolView(ctx context.Context, p *infrav1.K8sWorkerPool, byNode map[string]*corev1.Node,
	used map[string]*nodeUsage, volumes map[string][]Volume, exporters map[string]string, real map[string]realUsage) Pool {

	view := Pool{
		Namespace:       p.Namespace,
		Name:            p.Name,
		Replicas:        p.Spec.Replicas,
		Observed:        p.Status.ObservedReplicas,
		Ready:           readyCount(p.Status.Nodes),
		Phase:           p.Status.Rollout.Phase,
		MinReplicas:     p.Spec.ScaleDown.MinReplicas,
		AllowLastWorker: p.Spec.ScaleDown.AllowLastWorker,
		DeleteWorkers:   append([]string(nil), p.Spec.ScaleDown.DeleteWorkers...),
		Flavor: Flavor{
			CPUs:     p.Spec.Flavor.CPUs,
			MemoryMB: p.Spec.Flavor.MemoryMB,
			OSDiskGB: p.Spec.Flavor.OSDiskGB,
			Template: p.Spec.Flavor.Template,
		},
		Nodes: []Node{},
	}
	for _, cond := range p.Status.Conditions {
		if cond.Type == "Ready" && cond.Status != "True" {
			view.Warning = cond.Message
			break
		}
	}

	if cap := p.Status.Capacity; cap != nil {
		view.Capacity = &Capacity{
			Model:           cap.Model,
			CPUMHz:          cap.CPUMHz,
			CPULimitMHz:     cap.CPULimitMHz,
			MemoryMB:        cap.MemoryMB,
			MemoryLimitMB:   cap.MemoryLimitMB,
			StorageName:     cap.StorageName,
			StorageMB:       cap.StorageMB,
			StorageLimitMB:  cap.StorageLimitMB,
			PodSubnet:       cap.PodSubnet,
			PodSubnetsTotal: cap.PodSubnetsTotal,
			PodSubnetsUsed:  cap.PodSubnetsUsed,
			At:              cap.At.UTC().Format(time.RFC3339),
			CPU:             shareOf(cap.CPUMHz, cap.CPULimitMHz),
			Memory:          shareOf(cap.MemoryMB, cap.MemoryLimitMB),
			Storage:         shareOf(cap.StorageMB, cap.StorageLimitMB),
		}
	}

	removing := map[string]bool{}
	for _, name := range p.Spec.ScaleDown.DeleteWorkers {
		removing[name] = true
	}

	for i := range p.Status.Nodes {
		n := &p.Status.Nodes[i]
		row := Node{
			Name:       n.Name,
			NodeName:   n.NodeName,
			Phase:      string(n.Phase),
			IP:         n.IP,
			ProviderID: n.ProviderID,
			Deleting:   removing[n.Name],
		}
		kn := byNode[strings.ToLower(nodeKey(n))]
		if kn != nil {
			row.Ready = isNodeReady(kn)
			row.Cordoned = kn.Spec.Unschedulable
			row.Allocatable = allocatableOf(kn)
			for _, t := range kn.Spec.Taints {
				row.Taints = append(row.Taints, formatTaint(t))
			}
		}
		if u := used[strings.ToLower(nodeKey(n))]; u != nil {
			row.Requests = u.requests
			row.Limits = u.limits
			names := make([]string, 0, len(u.statefulSets))
			for name := range u.statefulSets {
				names = append(names, name)
			}
			sort.Strings(names)
			for _, name := range names {
				row.StatefulSets = append(row.StatefulSets, StatefulSetOnNode{Name: name, Pods: u.statefulSets[name]})
			}
			keys := make([]string, 0, len(u.claims))
			for key := range u.claims {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			seen := map[string]bool{}
			for _, key := range keys {
				if len(volumes[key]) == 0 {
					// A claim whose PV this account cannot see (RBAC, or a
					// volume the API server does not bind) still holds data.
					row.PersistentVolumes = append(row.PersistentVolumes, Volume{Claim: key})
					continue
				}
				for _, v := range volumes[key] {
					if !seen[v.Name] {
						seen[v.Name] = true
						row.PersistentVolumes = append(row.PersistentVolumes, v)
					}
				}
			}
		}
		row.Requests.CPU = NewShare(row.Requests.Millicores, row.Allocatable.Millicores)
		row.Requests.Memory = NewShare(row.Requests.MemoryMB, row.Allocatable.MemoryMB)
		row.Limits.CPU = NewShare(row.Limits.Millicores, row.Allocatable.Millicores)
		row.Limits.Memory = NewShare(row.Limits.MemoryMB, row.Allocatable.MemoryMB)
		row.Real, row.RealSource, row.Disks = c.realUsage(ctx, kn, row.Allocatable, exporters, real)
		view.Nodes = append(view.Nodes, row)
	}
	return view
}

// shareOf is NewShare that returns a pointer, for the capacity view where a
// missing number is a dash and not a zero-valued struct.
func shareOf(used, limit int64) *Share {
	if limit <= 0 {
		return nil
	}
	sh := NewShare(used, limit)
	return &sh
}

// readyCount is what the pool reports as READY: the slots a Node backs.
func readyCount(nodes []infrav1.WorkerNodeStatus) int32 {
	var n int32
	for i := range nodes {
		if nodes[i].Phase == infrav1.NodePhaseReady {
			n++
		}
	}
	return n
}

// formatTaint is a taint the way kubectl describes it.
func formatTaint(t corev1.Taint) string {
	if t.Value == "" {
		return t.Key + ":" + string(t.Effect)
	}
	return t.Key + "=" + t.Value + ":" + string(t.Effect)
}

// nodeKey is the Kubernetes node name of a slot. The pool lowercases it when it
// creates the Node, but a slot adopted from an older run may only carry its own
// name, which is the same thing in lowercase.
func nodeKey(n *infrav1.WorkerNodeStatus) string {
	if n.NodeName != "" {
		return n.NodeName
	}
	return n.Name
}

func isNodeReady(n *corev1.Node) bool {
	for _, cond := range n.Status.Conditions {
		if cond.Type == corev1.NodeReady {
			return cond.Status == corev1.ConditionTrue
		}
	}
	return false
}

func allocatableOf(n *corev1.Node) Allocatable {
	return Allocatable{
		Millicores: n.Status.Allocatable.Cpu().MilliValue(),
		MemoryMB:   n.Status.Allocatable.Memory().Value() / (1 << 20),
	}
}

// nodeUsage is the requests and limits of everything scheduled on one node.
type nodeUsage struct {
	requests Usage
	limits   Usage
	// statefulSets counts the StatefulSet pods on the node, by name.
	statefulSets map[string]int32
	// claims are the PersistentVolumeClaims the node's pods mount, by
	// namespace/name.
	claims map[string]bool
}

// addPod adds one pod to its node. A finished pod holds nothing; an init
// container runs alone, so it raises the floor rather than adding to the sum.
func addPod(m map[string]*nodeUsage, p *corev1.Pod) {
	if p.Spec.NodeName == "" || p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
		return
	}
	u := m[p.Spec.NodeName]
	if u == nil {
		u = &nodeUsage{}
		m[p.Spec.NodeName] = u
	}
	if u.statefulSets == nil {
		u.statefulSets = map[string]int32{}
		u.claims = map[string]bool{}
	}
	for i := range p.OwnerReferences {
		if p.OwnerReferences[i].Kind == "StatefulSet" {
			u.statefulSets[p.OwnerReferences[i].Name]++
		}
	}
	for i := range p.Spec.Volumes {
		if pvc := p.Spec.Volumes[i].PersistentVolumeClaim; pvc != nil {
			u.claims[p.Namespace+"/"+pvc.ClaimName] = true
		}
	}

	var reqCPU, reqMem, limCPU, limMem int64
	for i := range p.Spec.Containers {
		cnt := &p.Spec.Containers[i]
		reqCPU += cnt.Resources.Requests.Cpu().MilliValue()
		reqMem += cnt.Resources.Requests.Memory().Value() / (1 << 20)
		limCPU += cnt.Resources.Limits.Cpu().MilliValue()
		limMem += cnt.Resources.Limits.Memory().Value() / (1 << 20)
	}
	for i := range p.Spec.InitContainers {
		cnt := &p.Spec.InitContainers[i]
		if v := cnt.Resources.Requests.Cpu().MilliValue(); v > reqCPU {
			reqCPU = v
		}
		if v := cnt.Resources.Requests.Memory().Value() / (1 << 20); v > reqMem {
			reqMem = v
		}
		if v := cnt.Resources.Limits.Cpu().MilliValue(); v > limCPU {
			limCPU = v
		}
		if v := cnt.Resources.Limits.Memory().Value() / (1 << 20); v > limMem {
			limMem = v
		}
	}

	u.requests.Millicores += reqCPU
	u.requests.MemoryMB += reqMem
	u.requests.Pods++
	u.limits.Millicores += limCPU
	u.limits.MemoryMB += limMem
	u.limits.Pods++
}
