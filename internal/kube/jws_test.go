package kube

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

const testKubeconfig = "apiVersion: v1\nkind: Config\nclusters:\n- cluster:\n    server: https://192.168.100.2:6443\n  name: kubernetes\n"

// verifyDetachedJWS mirrors what kubeadm does for a joining node: parse the
// protected header, rebuild the ECDSA public key from the embedded JWK and
// check the signature over the signing input. It returns the payload.
func verifyDetachedJWS(t *testing.T, token string) []byte {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("JWS must have 3 parts, got %d", len(parts))
	}
	head, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		t.Fatalf("decode header: %v", err)
	}
	var header struct {
		Alg string `json:"alg"`
		JWK struct {
			Kty string `json:"kty"`
			Crv string `json:"crv"`
			X   string `json:"x"`
			Y   string `json:"y"`
		} `json:"jwk"`
	}
	if err := json.Unmarshal(head, &header); err != nil {
		t.Fatalf("parse header: %v", err)
	}
	if header.Alg != "ES256" || header.JWK.Kty != "EC" || header.JWK.Crv != "P-256" {
		t.Fatalf("unexpected header %s", head)
	}
	if header.JWK.X == "" || header.JWK.Y == "" {
		t.Fatalf("header carries no JWK coordinates: %s", head)
	}

	x, err := base64.RawURLEncoding.DecodeString(header.JWK.X)
	if err != nil {
		t.Fatalf("decode x: %v", err)
	}
	y, err := base64.RawURLEncoding.DecodeString(header.JWK.Y)
	if err != nil {
		t.Fatalf("decode y: %v", err)
	}
	if len(x) != 32 || len(y) != 32 {
		t.Fatalf("JWK coordinates must be 32 bytes, got %d/%d", len(x), len(y))
	}
	pub := &ecdsa.PublicKey{Curve: elliptic.P256(), X: new(big.Int).SetBytes(x), Y: new(big.Int).SetBytes(y)}

	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("decode signature: %v", err)
	}
	if len(sig) != 64 {
		t.Fatalf("ES256 signature must be 64 bytes, got %d", len(sig))
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if !ecdsa.Verify(pub, sum[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
		t.Fatal("signature does not verify with the JWK from the header")
	}

	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	return payload
}

func TestComputeDetachedJWSIsVerifiable(t *testing.T) {
	jws, err := computeDetachedJWS([]byte(testKubeconfig))
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	payload := verifyDetachedJWS(t, jws)
	if string(payload) != testKubeconfig {
		t.Fatalf("payload mismatch:\n%s", payload)
	}
}

func TestCreateBootstrapTokenPublishesClusterInfoJWS(t *testing.T) {
	cs := fake.NewSimpleClientset(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: clusterInfoName, Namespace: clusterInfoNamespace},
		Data:       map[string]string{clusterInfoKubeconfigKey: testKubeconfig},
	})
	c := NewClient(cs)

	token, err := c.CreateBootstrapToken(context.Background(), time.Hour)
	if err != nil {
		t.Fatalf("create token: %v", err)
	}
	id := strings.SplitN(token, ".", 2)[0]

	cm, err := cs.CoreV1().ConfigMaps(clusterInfoNamespace).Get(context.Background(), clusterInfoName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get cluster-info: %v", err)
	}
	jws, ok := cm.Data[jwsSignatureKeyPrefix+id]
	if !ok {
		t.Fatalf("no %s%s entry in cluster-info, keys: %v", jwsSignatureKeyPrefix, id, keys(cm.Data))
	}
	if payload := verifyDetachedJWS(t, jws); string(payload) != testKubeconfig {
		t.Fatal("JWS payload is not the cluster-info kubeconfig")
	}

	secret, err := cs.CoreV1().Secrets("kube-system").Get(context.Background(), "bootstrap-token-"+id, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("token secret: %v", err)
	}
	// The API server moves StringData into Data; the fake clientset keeps
	// StringData as is, so accept either.
	idValue := string(secret.Data["token-id"])
	if idValue == "" {
		idValue = secret.StringData["token-id"]
	}
	if idValue != id {
		t.Fatalf("secret token-id = %q, want %q", idValue, id)
	}

	// A second token must add its own entry and keep the first one.
	second, err := c.CreateBootstrapToken(context.Background(), time.Hour)
	if err != nil {
		t.Fatalf("create second token: %v", err)
	}
	id2 := strings.SplitN(second, ".", 2)[0]
	cm, _ = cs.CoreV1().ConfigMaps(clusterInfoNamespace).Get(context.Background(), clusterInfoName, metav1.GetOptions{})
	if _, ok := cm.Data[jwsSignatureKeyPrefix+id]; !ok {
		t.Fatal("first JWS entry was dropped")
	}
	if _, ok := cm.Data[jwsSignatureKeyPrefix+id2]; !ok {
		t.Fatal("second JWS entry is missing")
	}
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
