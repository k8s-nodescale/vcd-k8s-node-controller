package kube

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
)

var (
	// ErrDrainIncomplete means the node still carries pods when the short
	// eviction pass ends. The caller keeps the worker in Draining and retries
	// until scaleDown.drainTimeout is spent.
	ErrDrainIncomplete = errors.New("drain incomplete")
	// ErrVolumesAttached means a VolumeAttachment still points at the node, so
	// the VM must not be destroyed yet.
	ErrVolumesAttached = errors.New("volumes still attached")
)

const (
	// drainPassBudget bounds one reconcile pass: a drain can take minutes, and
	// holding the work queue for that long would starve other pools. Progress
	// is persisted between passes through status.nodes[].phase.
	drainPassBudget = 20 * time.Second
)

// drainPollEvery is a variable so tests do not wait for real seconds.
var drainPollEvery = 2 * time.Second

// Cordon marks the node unschedulable. It is idempotent.
func (c *Client) Cordon(ctx context.Context, name string) error {
	node, err := c.cs.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("get node %s: %w", name, err)
	}
	if node.Spec.Unschedulable {
		return nil
	}
	patch := []byte("{\"spec\":{\"unschedulable\":true}}")
	if _, err := c.cs.CoreV1().Nodes().Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("cordon node %s: %w", name, err)
	}
	return nil
}

// Uncordon clears the unschedulable mark. The rollout calls it once a resized
// worker is Ready again.
func (c *Client) Uncordon(ctx context.Context, name string) error {
	node, err := c.cs.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("get node %s: %w", name, err)
	}
	if !node.Spec.Unschedulable {
		return nil
	}
	patch := []byte("{\"spec\":{\"unschedulable\":false}}")
	if _, err := c.cs.CoreV1().Nodes().Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("uncordon node %s: %w", name, err)
	}
	return nil
}

// Drain evicts pods through the eviction API. That is what makes
// PodDisruptionBudgets meaningful: the API server answers 429 TooManyRequests
// while a budget has no disruptions left, and the pod stays put instead of
// being deleted behind the budget's back.
//
// DaemonSet pods and static (mirror) pods are skipped: they cannot be moved and
// disappear with the node. Pods using emptyDir are evicted (their data goes
// with them, which is the point of deleting the node).
func (c *Client) Drain(ctx context.Context, name string, timeout time.Duration) error {
	if err := c.Cordon(ctx, name); err != nil {
		return err
	}

	budget := drainPassBudget
	if timeout > 0 && timeout < budget {
		budget = timeout
	}
	deadline := time.Now().Add(budget)

	for {
		pods, err := c.podsToEvict(ctx, name)
		if err != nil {
			return err
		}
		if len(pods) == 0 {
			return nil
		}

		var blocked []string
		for i := range pods {
			pod := &pods[i]
			if pod.DeletionTimestamp != nil {
				continue
			}
			err := c.evictPod(ctx, pod)
			switch {
			case err == nil, apierrors.IsNotFound(err):
			case apierrors.IsTooManyRequests(err):
				blocked = append(blocked, c.blockingPDBs(ctx, pod)...)
			default:
				return fmt.Errorf("evict pod %s/%s: %w", pod.Namespace, pod.Name, err)
			}
		}

		if time.Now().After(deadline) {
			msg := fmt.Sprintf("%d pod(s) still on %s", len(pods), name)
			if len(blocked) > 0 {
				msg += "; blocked by PodDisruptionBudget(s): " + strings.Join(uniqueStrings(blocked), ", ")
			}
			return fmt.Errorf("%w: %s", ErrDrainIncomplete, msg)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(drainPollEvery):
		}
	}
}

// evictPod asks the API server to evict one pod.
func (c *Client) evictPod(ctx context.Context, pod *corev1.Pod) error {
	grace := int64(30)
	if pod.Spec.TerminationGracePeriodSeconds != nil {
		grace = *pod.Spec.TerminationGracePeriodSeconds
	}
	eviction := &policyv1.Eviction{
		ObjectMeta:    metav1.ObjectMeta{Name: pod.Name, Namespace: pod.Namespace},
		DeleteOptions: &metav1.DeleteOptions{GracePeriodSeconds: &grace},
	}
	return c.cs.PolicyV1().Evictions(pod.Namespace).Evict(ctx, eviction)
}

// podsToEvict lists the pods that must leave the node.
func (c *Client) podsToEvict(ctx context.Context, node string) ([]corev1.Pod, error) {
	pods, err := c.podsOnNode(ctx, node)
	if err != nil {
		return nil, err
	}
	out := make([]corev1.Pod, 0, len(pods))
	for i := range pods {
		pod := pods[i]
		if podTerminal(&pod) || isDaemonSetPod(&pod) || isMirrorPod(&pod) {
			continue
		}
		out = append(out, pod)
	}
	return out, nil
}

func (c *Client) podsOnNode(ctx context.Context, node string) ([]corev1.Pod, error) {
	list, err := c.cs.CoreV1().Pods(metav1.NamespaceAll).List(ctx, metav1.ListOptions{
		FieldSelector: "spec.nodeName=" + node,
	})
	if err != nil {
		return nil, fmt.Errorf("list pods on node %s: %w", node, err)
	}
	return list.Items, nil
}

// blockingPDBs returns the budgets that currently allow no disruption and
// select the pod. It only feeds the error message; the eviction API is the
// authority.
func (c *Client) blockingPDBs(ctx context.Context, pod *corev1.Pod) []string {
	budgets, err := c.cs.PolicyV1().PodDisruptionBudgets(pod.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil
	}
	var out []string
	for i := range budgets.Items {
		pdb := &budgets.Items[i]
		if pdb.Status.DisruptionsAllowed > 0 {
			continue
		}
		selector, err := metav1.LabelSelectorAsSelector(pdb.Spec.Selector)
		if err != nil || selector.Empty() {
			continue
		}
		if selector.Matches(labels.Set(pod.Labels)) {
			out = append(out, pdb.Name)
		}
	}
	return out
}

// WaitVolumesDetached waits until no VolumeAttachment references the node.
// Deleting a VM while a volume is still attached strands the volume in the
// storage backend and can leave the PV bound forever.
func (c *Client) WaitVolumesDetached(ctx context.Context, name string, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	deadline := time.Now().Add(timeout)
	for {
		attachments, err := c.cs.StorageV1().VolumeAttachments().List(ctx, metav1.ListOptions{})
		if err != nil {
			return fmt.Errorf("list volume attachments: %w", err)
		}
		var attached []string
		for i := range attachments.Items {
			va := &attachments.Items[i]
			if va.Spec.NodeName != name {
				continue
			}
			attached = append(attached, va.Name)
		}
		if len(attached) == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			sort.Strings(attached)
			return fmt.Errorf("%w: %s", ErrVolumesAttached, strings.Join(attached, ", "))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(drainPollEvery):
		}
	}
}

// NodeLocalVolumes returns the Bound PVs whose data physically lives on this
// node: hostPath volumes used by its pods and PVs pinned to it with
// nodeAffinity (local volumes). Destroying the VM destroys that data, so the
// caller only proceeds with force.
func (c *Client) NodeLocalVolumes(ctx context.Context, name string) ([]string, error) {
	node, err := c.cs.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get node %s: %w", name, err)
	}

	volumes, err := c.cs.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list persistent volumes: %w", err)
	}

	// hostPath PVs are only attributable to a node through the pods that mount
	// them, so collect the PVCs of the pods still on the node.
	claims := map[string]bool{}
	pods, err := c.podsOnNode(ctx, name)
	if err != nil {
		return nil, err
	}
	for i := range pods {
		for _, volume := range pods[i].Spec.Volumes {
			if volume.PersistentVolumeClaim == nil {
				continue
			}
			pvc, err := c.cs.CoreV1().PersistentVolumeClaims(pods[i].Namespace).Get(ctx, volume.PersistentVolumeClaim.ClaimName, metav1.GetOptions{})
			if err != nil {
				continue
			}
			if pvc.Spec.VolumeName != "" {
				claims[pvc.Spec.VolumeName] = true
			}
		}
	}

	seen := map[string]bool{}
	var out []string
	for i := range volumes.Items {
		pv := &volumes.Items[i]
		if pv.Status.Phase != corev1.VolumeBound || seen[pv.Name] {
			continue
		}
		local := pv.Spec.HostPath != nil && claims[pv.Name]
		if !local && pv.Spec.NodeAffinity != nil {
			local = pvMatchesNode(pv, node.Name)
		}
		if local {
			seen[pv.Name] = true
			out = append(out, pv.Name)
		}
	}
	sort.Strings(out)
	return out, nil
}

// pvMatchesNode reports whether a PV's required node affinity pins it to the
// node. Only the hostname key and the metadata.name field are considered; those
// are what the local-volume provisioners and the scheduler use.
func pvMatchesNode(pv *corev1.PersistentVolume, nodeName string) bool {
	if pv.Spec.NodeAffinity == nil || pv.Spec.NodeAffinity.Required == nil {
		return false
	}
	for _, term := range pv.Spec.NodeAffinity.Required.NodeSelectorTerms {
		if nodeSelectorTermMatches(term, nodeName) {
			return true
		}
	}
	return false
}

func nodeSelectorTermMatches(term corev1.NodeSelectorTerm, nodeName string) bool {
	matched := false
	for _, expr := range term.MatchExpressions {
		if expr.Key != "kubernetes.io/hostname" {
			continue
		}
		switch expr.Operator {
		case corev1.NodeSelectorOpIn:
			for _, value := range expr.Values {
				if value == nodeName {
					matched = true
				}
			}
		case corev1.NodeSelectorOpNotIn:
			matched = true
			for _, value := range expr.Values {
				if value == nodeName {
					matched = false
				}
			}
		}
	}
	for _, field := range term.MatchFields {
		if field.Key != "metadata.name" {
			continue
		}
		switch field.Operator {
		case corev1.NodeSelectorOpIn:
			for _, value := range field.Values {
				if value == nodeName {
					matched = true
				}
			}
		case corev1.NodeSelectorOpNotIn:
			matched = true
			for _, value := range field.Values {
				if value == nodeName {
					matched = false
				}
			}
		}
	}
	return matched
}

func isDaemonSetPod(pod *corev1.Pod) bool {
	for _, owner := range pod.OwnerReferences {
		if owner.Kind == "DaemonSet" {
			return true
		}
	}
	return false
}

func isMirrorPod(pod *corev1.Pod) bool {
	if _, ok := pod.Annotations[corev1.MirrorPodAnnotationKey]; ok {
		return true
	}
	for _, owner := range pod.OwnerReferences {
		if owner.Kind == "Node" {
			return true
		}
	}
	return false
}

func podTerminal(pod *corev1.Pod) bool {
	return pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed
}

func uniqueStrings(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
