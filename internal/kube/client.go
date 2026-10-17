package kube

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Client is a client-go-backed kube.Provider.
type Client struct {
	cs kubernetes.Interface
}

var _ Provider = (*Client)(nil)

// NewClient wraps a Kubernetes clientset.
func NewClient(cs kubernetes.Interface) *Client {
	return &Client{cs: cs}
}

// ListNodes returns a trimmed view of all cluster nodes.
func (c *Client) ListNodes(ctx context.Context) ([]NodeInfo, error) {
	list, err := c.cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	out := make([]NodeInfo, 0, len(list.Items))
	for i := range list.Items {
		n := &list.Items[i]
		info := NodeInfo{Name: n.Name, KubeletVersion: n.Status.NodeInfo.KubeletVersion}
		for _, cond := range n.Status.Conditions {
			if cond.Type == corev1.NodeReady {
				info.Ready = cond.Status == corev1.ConditionTrue
			}
		}
		if cpu := n.Status.Allocatable.Cpu(); cpu != nil {
			info.CPU = cpu.MilliValue()
		}
		if mem := n.Status.Allocatable.Memory(); mem != nil {
			info.MemoryBytes = mem.Value()
		}
		for _, addr := range n.Status.Addresses {
			if addr.Type == corev1.NodeInternalIP {
				info.InternalIP = addr.Address
			}
		}
		out = append(out, info)
	}
	return out, nil
}

// CAHash computes the kubeadm --discovery-token-ca-cert-hash from the cluster CA.
func (c *Client) CAHash(ctx context.Context) (string, error) {
	var pemBytes []byte
	for _, ns := range []string{"kube-public", "kube-system"} {
		cm, err := c.cs.CoreV1().ConfigMaps(ns).Get(ctx, "kube-root-ca.crt", metav1.GetOptions{})
		if err == nil {
			pemBytes = []byte(cm.Data["ca.crt"])
			break
		}
	}
	if len(pemBytes) == 0 {
		return "", fmt.Errorf("could not read kube-root-ca.crt from kube-public/kube-system")
	}
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return "", fmt.Errorf("failed to decode CA PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(cert.Raw)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// CreateBootstrapToken creates a short-lived kubeadm bootstrap token via the API.
// The returned value is suitable for `kubeadm join --token`.
func (c *Client) CreateBootstrapToken(ctx context.Context, ttl time.Duration) (string, error) {
	if ttl <= 0 {
		ttl = time.Hour
	}
	id, err := randomString(6)
	if err != nil {
		return "", err
	}
	secret, err := randomString(16)
	if err != nil {
		return "", err
	}
	obj := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "bootstrap-token-",
			Namespace:    "kube-system",
		},
		Type: corev1.SecretType("bootstrap.kubernetes.io/token"),
		StringData: map[string]string{
			"token-id":                       id,
			"token-secret":                   secret,
			"usage-bootstrap-authentication": "true",
			"usage-bootstrap-signing":        "true",
			"auth-extra-groups":              "system:bootstrappers:kubeadm:default-node-token",
			"expiration":                     time.Now().Add(ttl).UTC().Format(time.RFC3339),
		},
	}
	if _, err := c.cs.CoreV1().Secrets("kube-system").Create(ctx, obj, metav1.CreateOptions{}); err != nil {
		return "", err
	}
	return id + "." + secret, nil
}

// WaitReady polls until the named Node reports Ready.
func (c *Client) WaitReady(ctx context.Context, name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		n, err := c.cs.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
		if err == nil {
			for _, cond := range n.Status.Conditions {
				if cond.Type == corev1.NodeReady && cond.Status == corev1.ConditionTrue {
					return nil
				}
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("node %s did not become Ready within %s", name, timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Second):
		}
	}
}

// Drain TODO: cordon + eviction-API drain (respect PDBs, skip DaemonSets/mirror pods).
func (c *Client) Drain(ctx context.Context, name string, timeout time.Duration) error {
	return ErrNotImplemented
}

// DeleteNode removes the Node object from the cluster.
func (c *Client) DeleteNode(ctx context.Context, name string) error {
	return c.cs.CoreV1().Nodes().Delete(ctx, name, metav1.DeleteOptions{})
}

func randomString(n int) (string, error) {
	const charset = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	for i := range b {
		b[i] = charset[int(b[i])%len(charset)]
	}
	return string(b), nil
}
