package mgmt

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"

	infrav1 "github.com/k8s-nodescale/vcd-k8s-node-controller/api/v1alpha1"
)

// lastActionAnnotation records who changed the pool and what they asked for, so
// kubectl describe shows the same trail as the log.
const lastActionAnnotation = "k8s-nodescale.github.io/last-action-by"

// ErrInvalid marks a request the sidecar refuses before it writes anything, as
// opposed to one the API server or the controller refuses.
var ErrInvalid = errors.New("invalid request")

// SetReplicas writes spec.replicas. Every gate the controller enforces -
// minReplicas, allowLastWorker, the last-worker and node-local-volume refusals,
// the drain - still applies, because this is the same object kubectl would edit.
func (s *Server) SetReplicas(ctx context.Context, name string, replicas int32) error {
	if replicas < 0 {
		return fmt.Errorf("%w: replicas must not be negative", ErrInvalid)
	}
	return s.update(ctx, name, fmt.Sprintf("replicas=%d", replicas), func(p *infrav1.K8sWorkerPool) error {
		p.Spec.Replicas = replicas
		return nil
	})
}

// SetFlavor changes the shape of the pool. Growing rolls one worker at a time;
// shrinking or naming another template replaces each worker in its own slot
// (0.9.0). Which of the two happens stays the controller's decision.
func (s *Server) SetFlavor(ctx context.Context, name string, f Flavor) error {
	if f.CPUs == 0 && f.MemoryMB == 0 && f.OSDiskGB == 0 {
		return fmt.Errorf("%w: nothing to change", ErrInvalid)
	}
	if f.CPUs < 0 || f.MemoryMB < 0 || f.OSDiskGB < 0 {
		return fmt.Errorf("%w: flavor values must not be negative", ErrInvalid)
	}
	action := fmt.Sprintf("flavor=cpus:%d,memoryMB:%d,osDiskGB:%d", f.CPUs, f.MemoryMB, f.OSDiskGB)
	return s.update(ctx, name, action, func(p *infrav1.K8sWorkerPool) error {
		if f.CPUs > 0 {
			p.Spec.Flavor.CPUs = f.CPUs
		}
		if f.MemoryMB > 0 {
			p.Spec.Flavor.MemoryMB = f.MemoryMB
		}
		if f.OSDiskGB > 0 {
			p.Spec.Flavor.OSDiskGB = f.OSDiskGB
		}
		return nil
	})
}

// DiskTarget is one disk the page wants to grow, addressed the way the
// controller and the platform address it.
type DiskTarget struct {
	BusNumber  int32 `json:"busNumber"`
	UnitNumber int32 `json:"unitNumber"`
	SizeGB     int32 `json:"sizeGB"`
}

// SetDisks grows the system disk and the data disks. A shrink is refused here as
// well as in the controller: vCD cannot shrink a disk, and the only way to get a
// smaller one is a new VM, which an operator has to ask for deliberately
// (delete the worker), never by accident.
func (s *Server) SetDisks(ctx context.Context, name string, osDiskGB int32, disks []DiskTarget) error {
	if osDiskGB <= 0 && len(disks) == 0 {
		return fmt.Errorf("%w: nothing to change", ErrInvalid)
	}
	parts := []string{}
	if osDiskGB > 0 {
		parts = append(parts, fmt.Sprintf("os=%d", osDiskGB))
	}
	for _, d := range disks {
		if d.SizeGB > 0 {
			parts = append(parts, fmt.Sprintf("unit%d=%d", d.UnitNumber, d.SizeGB))
		}
	}
	action := "disks=" + strings.Join(parts, ",")
	return s.update(ctx, name, action, func(p *infrav1.K8sWorkerPool) error {
		if osDiskGB > 0 {
			if osDiskGB < p.Spec.Flavor.OSDiskGB {
				return fmt.Errorf("%w: the OS disk is %d GB and a disk cannot shrink; replace the worker instead",
					ErrInvalid, p.Spec.Flavor.OSDiskGB)
			}
			p.Spec.Flavor.OSDiskGB = osDiskGB
		}
		for _, want := range disks {
			if want.SizeGB <= 0 {
				continue
			}
			idx := -1
			for i := range p.Spec.Flavor.Disks {
				if p.Spec.Flavor.Disks[i].UnitNumber == want.UnitNumber {
					idx = i
					break
				}
			}
			if idx < 0 {
				return fmt.Errorf("%w: the pool has no data disk at unit %d", ErrInvalid, want.UnitNumber)
			}
			cur := p.Spec.Flavor.Disks[idx].SizeGB
			if want.SizeGB < cur {
				where := p.Spec.Flavor.Disks[idx].Mount
				if where == "" {
					where = fmt.Sprintf("unit %d", want.UnitNumber)
				}
				return fmt.Errorf("%w: %s is %d GB and a disk cannot shrink; replace the worker instead",
					ErrInvalid, where, cur)
			}
			p.Spec.Flavor.Disks[idx].SizeGB = want.SizeGB
		}
		return nil
	})
}

// DeleteWorkers appends names to spec.scaleDown.deleteWorkers. An entry is
// processed like a scale-down and leaves the pool one worker smaller; the
// controller drops unknown names with a warning event.
func (s *Server) DeleteWorkers(ctx context.Context, name string, workers []string) error {
	if len(workers) == 0 {
		return fmt.Errorf("%w: no worker named", ErrInvalid)
	}
	action := "delete=" + strings.Join(workers, ",")
	return s.update(ctx, name, action, func(p *infrav1.K8sWorkerPool) error {
		for _, w := range workers {
			if w == "" || containsName(p.Spec.ScaleDown.DeleteWorkers, w) {
				continue
			}
			p.Spec.ScaleDown.DeleteWorkers = append(p.Spec.ScaleDown.DeleteWorkers, w)
		}
		return nil
	})
}

func containsName(list []string, name string) bool {
	for _, v := range list {
		if v == name {
			return true
		}
	}
	return false
}

// update reads, mutates and writes one pool, retrying a lost race with the
// controller's own status writes instead of failing the operator's click.
func (s *Server) update(ctx context.Context, name, action string, mutate func(*infrav1.K8sWorkerPool) error) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		p := &infrav1.K8sWorkerPool{}
		if err := s.Client.Get(ctx, types.NamespacedName{Name: name}, p); err != nil {
			return err
		}
		// One operation at a time: a resize, a scale and a removal all drain
		// the same workers and share the same budget.
		if why := busyReason(p); why != "" {
			return fmt.Errorf("%w: an operation is already in progress: %s", ErrInvalid, why)
		}
		if err := mutate(p); err != nil {
			return err
		}
		if actor, ok := actorFrom(ctx); ok && actor.Username != "" {
			if p.Annotations == nil {
				p.Annotations = map[string]string{}
			}
			p.Annotations[lastActionAnnotation] = fmt.Sprintf("%s (%s) %s at %s",
				actor.Username, actor.Role, action, time.Now().UTC().Format(time.RFC3339))
		}
		return s.Client.Update(ctx, p)
	})
}
