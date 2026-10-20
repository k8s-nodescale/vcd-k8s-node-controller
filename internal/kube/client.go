package kube

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
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
//
// kubeadm pins the SubjectPublicKeyInfo of the CA, not the certificate: its
// util/pubkeypin hashes x509.MarshalPKIXPublicKey(cert.PublicKey). Hashing
// cert.Raw instead yields a plausible looking sha256:... that kubeadm always
// rejects with `cluster CA found in cluster-info ConfigMap is invalid: none of
// the public keys ... are pinned`.
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
	spki, err := x509.MarshalPKIXPublicKey(cert.PublicKey)
	if err != nil {
		return "", fmt.Errorf("encode CA public key: %w", err)
	}
	sum := sha256.Sum256(spki)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;update,namespace=kube-public

// CreateBootstrapToken creates a short-lived kubeadm bootstrap token via the API
// and publishes the JWS signature kubeadm needs for token-based discovery.
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
			// kubeadm names the secret after the token id; keeping the same
			// convention makes the secret, the JWS entry and the join command
			// trivially traceable to each other.
			Name:      "bootstrap-token-" + id,
			Namespace: "kube-system",
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
	// The secret alone is not enough: kubeadm validates the API server identity
	// through the JWS entry in cluster-info and refuses to join without it.
	if err := c.publishClusterInfoJWS(ctx, id); err != nil {
		_ = c.cs.CoreV1().Secrets("kube-system").Delete(ctx, obj.Name, metav1.DeleteOptions{})
		return "", err
	}
	return id + "." + secret, nil
}

// publishClusterInfoJWS adds the jws-kubeconfig-<tokenID> entry kubeadm looks
// up during token-based discovery. The signature covers the exact kubeconfig
// bytes stored in the ConfigMap.
func (c *Client) publishClusterInfoJWS(ctx context.Context, tokenID string) error {
	var jws string
	// cluster-info is shared with anything else that creates tokens, so a
	// concurrent write is expected and must not fail the whole scale-up.
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cm, err := c.cs.CoreV1().ConfigMaps(clusterInfoNamespace).Get(ctx, clusterInfoName, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("read %s/%s for bootstrap token %s: %w", clusterInfoNamespace, clusterInfoName, tokenID, err)
		}
		kubeconfig, ok := cm.Data[clusterInfoKubeconfigKey]
		if !ok || kubeconfig == "" {
			return fmt.Errorf("%s/%s has no %q data key", clusterInfoNamespace, clusterInfoName, clusterInfoKubeconfigKey)
		}
		if jws == "" {
			jws, err = computeDetachedJWS([]byte(kubeconfig))
			if err != nil {
				return err
			}
		}
		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		cm.Data[jwsSignatureKeyPrefix+tokenID] = jws
		_, err = c.cs.CoreV1().ConfigMaps(clusterInfoNamespace).Update(ctx, cm, metav1.UpdateOptions{})
		return err
	})
	if err != nil {
		return fmt.Errorf("publish JWS for bootstrap token %s: %w", tokenID, err)
	}
	return nil
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

// SetLabels adds or updates the given labels. A merge patch keeps unrelated
// labels (kubelet, cloud provider) intact.
func (c *Client) SetLabels(ctx context.Context, name string, labels map[string]string) error {
	if len(labels) == 0 {
		return nil
	}
	node, err := c.cs.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	diff := make(map[string]string, len(labels))
	for k, v := range labels {
		// A label such as node-role.kubernetes.io/worker carries an empty
		// value, so a plain map lookup cannot tell "absent" from "present
		// and empty" and the label would never be added.
		if cur, ok := node.Labels[k]; !ok || cur != v {
			diff[k] = v
		}
	}
	if len(diff) == 0 {
		return nil
	}
	body, err := json.Marshal(map[string]interface{}{
		"metadata": map[string]interface{}{"labels": diff},
	})
	if err != nil {
		return err
	}
	_, err = c.cs.CoreV1().Nodes().Patch(ctx, name, types.MergePatchType, body, metav1.PatchOptions{})
	return err
}

// SetTaints replaces the node's taints. A merge patch is used so the controller
// only needs the patch verb on nodes.
func (c *Client) SetTaints(ctx context.Context, name string, taints []corev1.Taint) error {
	node, err := c.cs.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if taintsEqual(node.Spec.Taints, taints) {
		return nil
	}
	body, err := json.Marshal(map[string]interface{}{
		"spec": map[string]interface{}{"taints": taints},
	})
	if err != nil {
		return err
	}
	_, err = c.cs.CoreV1().Nodes().Patch(ctx, name, types.MergePatchType, body, metav1.PatchOptions{})
	return err
}

func taintsEqual(a, b []corev1.Taint) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[corev1.Taint]int, len(a))
	for _, t := range a {
		seen[t]++
	}
	for _, t := range b {
		if seen[t] == 0 {
			return false
		}
		seen[t]--
	}
	return true
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
