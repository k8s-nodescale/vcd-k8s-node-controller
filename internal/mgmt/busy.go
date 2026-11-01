package mgmt

import (
	"fmt"
	"strings"

	infrav1 "github.com/k8s-nodescale/vcd-k8s-node-controller/api/v1alpha1"
)

// busyReason says why no new operation may be started on this pool, or returns
// "" when the pool is at rest. One operation at a time is not a limitation of
// the page: a resize and a scale-down both drain, both spend the same
// disruption budget, and stacking them is how an operator loses a worker they
// meant to keep. The controller would serialise them anyway; the page and the
// API refuse instead, so the operator sees the reason before clicking.
func busyReason(p *infrav1.K8sWorkerPool) string {
	if ph := strings.TrimSpace(p.Status.Rollout.Phase); ph != "" &&
		!strings.EqualFold(ph, infrav1.RolloutPhaseIdle) {
		return fmt.Sprintf("a rollout is %s", strings.ToLower(ph))
	}
	for i := range p.Status.Nodes {
		n := &p.Status.Nodes[i]
		if n.Resize != nil {
			return fmt.Sprintf("%s is being resized (%s)", n.Name, n.Resize.Phase)
		}
		switch n.Phase {
		case infrav1.NodePhaseProvisioning, infrav1.NodePhaseJoining,
			infrav1.NodePhaseDraining, infrav1.NodePhaseDeleting,
			infrav1.NodePhaseResizing:
			return fmt.Sprintf("%s is %s", n.Name, strings.ToLower(string(n.Phase)))
		}
	}
	// A replica count that does not match is not enough on its own: it is true
	// for a moment after every write, and a scale always shows up as a worker in
	// Provisioning, Joining, Draining or Deleting above.
	return ""
}
