// Package kube defines the Kubernetes-side operations the reconciler needs.
package kube

import (
	"context"
	"errors"
	"time"
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
}

// Provider abstracts cluster operations required for worker scaling.
type Provider interface {
	CreateBootstrapToken(ctx context.Context, ttl time.Duration) (string, error)
	CAHash(ctx context.Context) (string, error)
	ListNodes(ctx context.Context) ([]NodeInfo, error)
	WaitReady(ctx context.Context, name string, timeout time.Duration) error
	Drain(ctx context.Context, name string, timeout time.Duration) error
	DeleteNode(ctx context.Context, name string) error
}
