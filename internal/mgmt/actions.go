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
func (s *Server) SetReplicas(ctx context.Context, ns, name string, replicas int32) error {
	if replicas < 0 {
		return fmt.Errorf("%w: replicas must not be negative", ErrInvalid)
	}
	return s.update(ctx, ns, name, fmt.Sprintf("replicas=%d", replicas), func(p *infrav1.K8sWorkerPool) error {
		p.Spec.Replicas = replicas
		return nil
	})
}

// SetFlavor changes the shape of the pool. Growing rolls one worker at a time;
// shrinking or naming another template replaces each worker in its own slot
// (0.9.0). Which of the two happens stays the controller's decision.
func (s *Server) SetFlavor(ctx context.Context, ns, name string, f Flavor) error {
	if f.CPUs == 0 && f.MemoryMB == 0 && f.OSDiskGB == 0 {
		return fmt.Errorf("%w: nothing to change", ErrInvalid)
	}
	if f.CPUs < 0 || f.MemoryMB < 0 || f.OSDiskGB < 0 {
		return fmt.Errorf("%w: flavor values must not be negative", ErrInvalid)
	}
	action := fmt.Sprintf("flavor=cpus:%d,memoryMB:%d,osDiskGB:%d", f.CPUs, f.MemoryMB, f.OSDiskGB)
	return s.update(ctx, ns, name, action, func(p *infrav1.K8sWorkerPool) error {
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

// DeleteWorkers appends names to spec.scaleDown.deleteWorkers. An entry is
// processed like a scale-down and leaves the pool one worker smaller; the
// controller drops unknown names with a warning event.
func (s *Server) DeleteWorkers(ctx context.Context, ns, name string, workers []string) error {
	if len(workers) == 0 {
		return fmt.Errorf("%w: no worker named", ErrInvalid)
	}
	action := "delete=" + strings.Join(workers, ",")
	return s.update(ctx, ns, name, action, func(p *infrav1.K8sWorkerPool) error {
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
func (s *Server) update(ctx context.Context, ns, name, action string, mutate func(*infrav1.K8sWorkerPool) error) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		p := &infrav1.K8sWorkerPool{}
		if err := s.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, p); err != nil {
			return err
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
