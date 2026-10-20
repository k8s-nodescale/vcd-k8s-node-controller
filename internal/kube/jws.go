package kube

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
)

const (
	clusterInfoNamespace     = "kube-public"
	clusterInfoName          = "cluster-info"
	clusterInfoKubeconfigKey = "kubeconfig"
	jwsSignatureKeyPrefix    = "jws-kubeconfig-"
)

// computeDetachedJWS returns a detached JWS (ES256) over payload in the exact
// shape kubeadm stores in the cluster-info ConfigMap: three base64url parts,
// with the signing key published as a JWK in the protected header.
//
// kubeadm accepts a bootstrap token only if the ConfigMap carries a matching
// jws-kubeconfig-<token-id> entry, even when the join command pins the CA with
// --discovery-token-ca-cert-hash. A token created straight through the API has
// no such entry, so the join fails with "could not find a JWS signature in the
// cluster-info ConfigMap for token ID ...".
func computeDetachedJWS(payload []byte) (string, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", fmt.Errorf("generate JWS key: %w", err)
	}

	// JWS ES256 requires fixed-width 32-byte coordinates and a 64-byte
	// r||s signature, so pad instead of using big.Int.Bytes().
	x := make([]byte, 32)
	y := make([]byte, 32)
	key.PublicKey.X.FillBytes(x)
	key.PublicKey.Y.FillBytes(y)

	header, err := json.Marshal(map[string]any{
		"alg": "ES256",
		"jwk": map[string]string{
			"kty": "EC",
			"crv": "P-256",
			"x":   base64.RawURLEncoding.EncodeToString(x),
			"y":   base64.RawURLEncoding.EncodeToString(y),
		},
	})
	if err != nil {
		return "", fmt.Errorf("encode JWS header: %w", err)
	}

	protected := base64.RawURLEncoding.EncodeToString(header)
	signingInput := protected + "." + base64.RawURLEncoding.EncodeToString(payload)
	sum := sha256.Sum256([]byte(signingInput))
	r, s, err := ecdsa.Sign(rand.Reader, key, sum[:])
	if err != nil {
		return "", fmt.Errorf("sign JWS: %w", err)
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])

	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}
