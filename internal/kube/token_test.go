package kube

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func tokenTestClient() *Client {
	cs := fake.NewSimpleClientset(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: clusterInfoName, Namespace: clusterInfoNamespace},
		Data:       map[string]string{clusterInfoKubeconfigKey: testKubeconfig},
	})
	return NewClient(cs)
}

// normalizeTokenSecret copies the create-time StringData into Data: the real
// API server does that conversion, the fake clientset does not.
func normalizeTokenSecret(t *testing.T, c *Client, id string) *corev1.Secret {
	t.Helper()
	ctx := context.Background()
	secrets := c.cs.CoreV1().Secrets("kube-system")
	s, err := secrets.Get(ctx, "bootstrap-token-"+id, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if s.Data == nil {
		s.Data = map[string][]byte{}
	}
	for k, v := range s.StringData {
		if _, ok := s.Data[k]; !ok {
			s.Data[k] = []byte(v)
		}
	}
	s.StringData = nil
	if _, err := secrets.Update(ctx, s, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	return s
}

// A guest that is slow to reach kubeadm join must not find its token expired:
// the expiration is pushed out by one ttl, and the JWS entry stays.
func TestRenewBootstrapTokenExtendsAShortExpiration(t *testing.T) {
	ctx := context.Background()
	c := tokenTestClient()
	token, err := c.CreateBootstrapToken(ctx, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	id, _, _ := strings.Cut(token, ".")

	s := normalizeTokenSecret(t, c, id)
	s.Data["expiration"] = []byte(time.Now().Add(5 * time.Minute).UTC().Format(time.RFC3339))
	if _, err := c.cs.CoreV1().Secrets("kube-system").Update(ctx, s, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}

	if err := c.RenewBootstrapTokenID(ctx, id, time.Hour); err != nil {
		t.Fatal(err)
	}

	s, err = c.cs.CoreV1().Secrets("kube-system").Get(ctx, "bootstrap-token-"+id, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	exp, err := time.Parse(time.RFC3339, string(s.Data["expiration"]))
	if err != nil {
		t.Fatal(err)
	}
	if remaining := time.Until(exp); remaining < 50*time.Minute {
		t.Fatalf("expiration = %s (%s left), want about an hour", s.Data["expiration"], remaining)
	}
	cm, err := c.cs.CoreV1().ConfigMaps(clusterInfoNamespace).Get(ctx, clusterInfoName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if cm.Data[jwsSignatureKeyPrefix+id] == "" {
		t.Fatal("the JWS entry disappeared during renewal")
	}
}

// Renewal is idempotent: a token with more than half its ttl left is not
// rewritten, so a reconcile pass does not touch the Secret for nothing.
func TestRenewBootstrapTokenLeavesAFreshTokenAlone(t *testing.T) {
	ctx := context.Background()
	c := tokenTestClient()
	token, err := c.CreateBootstrapToken(ctx, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	id, _, _ := strings.Cut(token, ".")

	before := string(normalizeTokenSecret(t, c, id).Data["expiration"])
	if err := c.RenewBootstrapTokenID(ctx, id, time.Hour); err != nil {
		t.Fatal(err)
	}
	s, err := c.cs.CoreV1().Secrets("kube-system").Get(ctx, "bootstrap-token-"+id, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if string(s.Data["expiration"]) != before {
		t.Fatalf("expiration changed from %s to %s, want it left alone", before, s.Data["expiration"])
	}
}

// A token that already went away (expired, revoked, or from another cluster)
// is not an error: nothing to renew.
func TestRenewBootstrapTokenIgnoresAMissingToken(t *testing.T) {
	c := tokenTestClient()
	if err := c.RenewBootstrapTokenID(context.Background(), "gone", time.Hour); err != nil {
		t.Fatalf("renew a missing token: %v", err)
	}
}
