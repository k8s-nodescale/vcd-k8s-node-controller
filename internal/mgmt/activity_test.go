package mgmt

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	infrav1 "github.com/k8s-nodescale/vcd-k8s-node-controller/api/v1alpha1"
)

// TestActivityDuringScaleOut is the case the owner hit on 2026-11-20: a 3 -> 4
// scale-out with status.rollout.phase = Idle (no worker is being replaced) and
// a fourth worker in Provisioning. The page said Idle while a VM was being
// created, which is true of the rollout and useless as a status line.
func TestActivityDuringScaleOut(t *testing.T) {
	p := testPool(3)
	p.Status.Rollout.Phase = infrav1.RolloutPhaseIdle
	p.Status.Nodes = []infrav1.WorkerNodeStatus{
		{Name: "w-1", Phase: infrav1.NodePhaseReady},
		{Name: "w-2", Phase: infrav1.NodePhaseReady},
		{Name: "w-3", Phase: infrav1.NodePhaseReady},
		{Name: "w-4", Phase: infrav1.NodePhaseProvisioning},
	}
	if got := activity(p); got != string(infrav1.NodePhaseProvisioning) {
		t.Fatalf("activity = %q during a scale-out, want Provisioning", got)
	}
}

// A running replacement is the longer operation and wins over a worker step.
func TestActivityPrefersTheRollout(t *testing.T) {
	p := testPool(2)
	p.Status.Rollout.Phase = infrav1.RolloutPhaseProgressing
	p.Status.Nodes = []infrav1.WorkerNodeStatus{{Name: "w-1", Phase: infrav1.NodePhaseJoining}}
	if got := activity(p); got != infrav1.RolloutPhaseProgressing {
		t.Fatalf("activity = %q, want the rollout phase", got)
	}
}

func TestActivityIsIdleWhenEveryWorkerIsReady(t *testing.T) {
	p := testPool(2)
	p.Status.Rollout.Phase = infrav1.RolloutPhaseIdle
	p.Status.Nodes = []infrav1.WorkerNodeStatus{
		{Name: "w-1", Phase: infrav1.NodePhaseReady},
		{Name: "w-2", Phase: infrav1.NodePhaseReady},
	}
	if got := activity(p); got != infrav1.RolloutPhaseIdle {
		t.Fatalf("activity = %q, want Idle", got)
	}
}

// With no transient worker the Ready condition still has something to say, e.g.
// a node that never answered.
func TestActivityFallsBackToTheConditionReason(t *testing.T) {
	p := testPool(1)
	p.Status.Nodes = []infrav1.WorkerNodeStatus{{Name: "w-1", Phase: infrav1.NodePhaseReady}}
	p.Status.Conditions = []metav1.Condition{{
		Type: "Ready", Status: metav1.ConditionFalse, Reason: "WorkerNotReady",
		Message: "1/1 workers Ready",
	}}
	if got := activity(p); got != "WorkerNotReady" {
		t.Fatalf("activity = %q, want the condition reason", got)
	}
}
