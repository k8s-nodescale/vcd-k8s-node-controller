package mgmt

import (
	"strings"

	infrav1 "github.com/k8s-nodescale/vcd-k8s-node-controller/api/v1alpha1"
)

// activity is the one word the page shows about what the pool is doing. It
// exists because status.rollout.phase answers a narrower question: it tracks a
// rolling replacement of workers and stays Idle during a plain scale-out. Next
// to a worker that is being created that reads as "nothing is happening"
// (found live on 2026-11-20, during a 3 -> 4 scale-out), and a status field
// that is true but useless is worse than none.
//
// A running replacement wins: it is the longer operation and the one that
// changes every worker. Otherwise the transient phase of a worker wins, then
// the reason of a Ready condition that is not True, and only then Idle.
func activity(p *infrav1.K8sWorkerPool) string {
	if ph := strings.TrimSpace(p.Status.Rollout.Phase); ph != "" &&
		!strings.EqualFold(ph, infrav1.RolloutPhaseIdle) {
		return ph
	}
	for i := range p.Status.Nodes {
		switch p.Status.Nodes[i].Phase {
		case infrav1.NodePhaseProvisioning, infrav1.NodePhaseJoining,
			infrav1.NodePhaseDraining, infrav1.NodePhaseDeleting,
			infrav1.NodePhaseResizing, infrav1.NodePhasePending,
			infrav1.NodePhaseQueued:
			return string(p.Status.Nodes[i].Phase)
		}
	}
	// Nothing transient: a pool that is not converged still has a reason, e.g.
	// a worker that never answered, and that is what the reader needs.
	for _, c := range p.Status.Conditions {
		if c.Type == "Ready" && c.Status != "True" {
			if c.Reason != "" {
				return c.Reason
			}
			return "NotReady"
		}
	}
	return infrav1.RolloutPhaseIdle
}
