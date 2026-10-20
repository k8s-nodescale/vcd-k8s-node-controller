package kube

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// testCA is a throwaway self-signed CA in the shape kubeadm produces.
const testCA = `-----BEGIN CERTIFICATE-----
MIIBhzCCAS6gAwIBAgIEATUlBTAKBggqhkjOPQQDAjAqMRMwEQYDVQQKEwprdWJl
cm5ldGVzMRMwEQYDVQQDEwprdWJlcm5ldGVzMB4XDTI2MDEwMTAwMDAwMFoXDTM2
MDEwMTAwMDAwMFowKjETMBEGA1UEChMKa3ViZXJuZXRlczETMBEGA1UEAxMKa3Vi
ZXJuZXRlczBZMBMGByqGSM49AgEGCCqGSM49AwEHA0IABANDOxhx8eyS49Uflp1L
iQNIHrfuqH27IFU7jaYr7NEv3hJdvMbHTMHNq+IaDgfU78YnL+Of8nYcni10UB8z
vcOjQjBAMA4GA1UdDwEB/wQEAwIChDAPBgNVHRMBAf8EBTADAQH/MB0GA1UdDgQW
BBTDL0m8G4T5tHYh47jw8JM99kIAfTAKBggqhkjOPQQDAgNHADBEAiAwALwkUFh+
fIrgJJx+tLq/guWyaS15H3tDj5q02vsLBAIgZGMyj+BHMRoCiJGYAiY48GIII5Ff
s9RVJowMwTzvECk=
-----END CERTIFICATE-----
`

// testCASPKIHash is sha256(x509.MarshalPKIXPublicKey(ca.PublicKey)): what
// kubeadm writes into a join command as --discovery-token-ca-cert-hash.
const testCASPKIHash = "sha256:d8a7562da2e545ff2078f7cdc67d0f887d88c6e6b642cad676637b781a87e303"

// testCADERHash is sha256(ca.Raw), the value CAHash used to return. kubeadm
// never accepts it, so pinning it here keeps the regression visible.
const testCADERHash = "sha256:bfc05a7d4fdf9f6af5ab6acffb0ffbe0012f1a5a49eabbf3a1c8b6527987e6db"

func TestCAHashPinsThePublicKey(t *testing.T) {
	cs := fake.NewSimpleClientset(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "kube-root-ca.crt", Namespace: "kube-public"},
		Data:       map[string]string{"ca.crt": testCA},
	})

	got, err := NewClient(cs).CAHash(context.Background())
	if err != nil {
		t.Fatalf("CAHash: %v", err)
	}
	if got != testCASPKIHash {
		t.Fatalf("CAHash = %q, want the public-key hash %q", got, testCASPKIHash)
	}
	if got == testCADERHash {
		t.Fatalf("CAHash returned the certificate hash %q, which kubeadm rejects", testCADERHash)
	}
}

func TestCAHashFallsBackToKubeSystem(t *testing.T) {
	cs := fake.NewSimpleClientset(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "kube-root-ca.crt", Namespace: "kube-system"},
		Data:       map[string]string{"ca.crt": testCA},
	})

	got, err := NewClient(cs).CAHash(context.Background())
	if err != nil {
		t.Fatalf("CAHash: %v", err)
	}
	if got != testCASPKIHash {
		t.Fatalf("CAHash = %q, want %q", got, testCASPKIHash)
	}
}

func TestCAHashReportsMissingCA(t *testing.T) {
	_, err := NewClient(fake.NewSimpleClientset()).CAHash(context.Background())
	if err == nil {
		t.Fatal("expected an error when no kube-root-ca.crt exists")
	}
	if !strings.Contains(err.Error(), "kube-root-ca.crt") {
		t.Fatalf("unexpected error: %v", err)
	}
}
