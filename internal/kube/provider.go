// Package kube defines the Kubernetes-side operations the reconciler needs.
package kube

import (
	"context"
	"errors"
	"time"

	corev1 "k8s.io/api/core/v1"
)

// ErrNotImplemented marks methods that still need a real implementation.
var ErrNotImplemented = errors.New("kube provider method not implemented")

// NodeInfo is a trimmed view of a Kubernetes Node.
type NodeInfo struct {
	Name           string
	Ready          bool
	KubeletVersion string
	CPU            int64 // millicores
	MemoryBytes    int64
	InternalIP     string
	Annotations    map[string]string
}

// Provider abstracts cluster operations required for worker scaling.
type Provider interface {
	CreateBootstrapToken(ctx context.Context, ttl time.Duration) (string, error)
	// RevokeBootstrapToken deletes a token and its cluster-info JWS entry. It is
	// called when a create fails after the token was minted, so a failed attempt
	// does not leave a usable credential behind.
	RevokeBootstrapToken(ctx context.Context, token string) error
	// RevokeBootstrapTokenID is RevokeBootstrapToken for a token id kept in
	// status, where the secret half never appears.
	RevokeBootstrapTokenID(ctx context.Context, id string) error
	// RenewBootstrapTokenID extends a live token's expiration so a guest that
	// is slow to reach kubeadm join does not find it expired. It is a no-op
	// when the token is gone or still has more than half its ttl left.
	RenewBootstrapTokenID(ctx context.Context, id string, ttl time.Duration) error
	CAHash(ctx context.Context) (string, error)
	ListNodes(ctx context.Context) ([]NodeInfo, error)
	WaitReady(ctx context.Context, name string, timeout time.Duration) error
	// Cordon marks the node unschedulable. It is idempotent.
	Cordon(ctx context.Context, name string) error
	// Uncordon clears the unschedulable mark. It is idempotent.
	Uncordon(ctx context.Context, name string) error
	// Drain evicts the evictable pods of a node through the eviction API, so
	// PodDisruptionBudgets are honoured. It returns ErrDrainIncomplete when
	// pods are still present when the (short) pass budget expires; the caller
	// retries until its own drainTimeout runs out. DaemonSet and mirror pods
	// are skipped.
	Drain(ctx context.Context, name string, timeout time.Duration) error
	// ForceEvict deletes the pods left on a node, bypassing the eviction API
	// and the PodDisruptionBudgets behind it. It is used once
	// scaleDown.drainTimeout is spent and the pool still wants the worker
	// gone.
	ForceEvict(ctx context.Context, name string) (EvictionReport, error)
	// WaitVolumesDetached blocks until no VolumeAttachment references the node,
	// so the VM is never destroyed while a volume is still attached to it.
	WaitVolumesDetached(ctx context.Context, name string, timeout time.Duration) error
	// NodeLocalVolumes returns Bound PVs whose data lives on this node
	// (hostPath volumes used by its pods, and PVs pinned to it by nodeAffinity).
	// Deleting such a node destroys the data, so the caller asks for force.
	NodeLocalVolumes(ctx context.Context, name string) ([]string, error)
	// DeleteNode removes the Node object. NotFound is not an error.
	DeleteNode(ctx context.Context, name string) error

	// SetTaints replaces the taints on a Node. It is a no-op when they already
	// match.
	SetTaints(ctx context.Context, name string, taints []corev1.Taint) error

	// SetLabels adds or updates labels on a Node. Other labels are left alone.
	SetLabels(ctx context.Context, name string, labels map[string]string) error
}
