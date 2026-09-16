package kubernetes

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"
	"time"
)

// keypairPEM generates a throwaway certificate and key, so the
// client-certificate path is exercised against real PEM rather than a
// hand-written blob that tls.X509KeyPair would reject for its own reasons.
func keypairPEM(t *testing.T) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "kraai-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("creating certificate: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshalling key: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
}

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

// TestParseConfigReadsAK3sStyleKubeconfig covers the shape kraai will
// actually retrieve from a node it provisioned: one cluster, one user, a CA
// and a client keypair, all inline.
func TestParseConfigReadsAK3sStyleKubeconfig(t *testing.T) {
	certPEM, keyPEM := keypairPEM(t)
	raw := `apiVersion: v1
kind: Config
current-context: default
clusters:
- name: default
  cluster:
    server: https://10.0.1.5:6443/
    certificate-authority-data: ` + b64(certPEM) + `
users:
- name: default
  user:
    client-certificate-data: ` + b64(certPEM) + `
    client-key-data: ` + b64(keyPEM) + `
contexts:
- name: default
  context:
    cluster: default
    user: default
`
	cfg, err := ParseConfig([]byte(raw))
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	// The trailing slash must go, or every request path doubles it.
	if cfg.Server != "https://10.0.1.5:6443" {
		t.Fatalf("Server = %q", cfg.Server)
	}
	if len(cfg.TLS.Certificates) != 1 {
		t.Fatal("no client keypair was loaded, so every request would be unauthenticated")
	}
	if cfg.TLS.RootCAs == nil {
		t.Fatal("no CA was loaded, so the server certificate could not be verified")
	}
	if cfg.TLS.InsecureSkipVerify {
		t.Fatal("verification was skipped without the kubeconfig asking for it")
	}
}

func TestParseConfigReadsATokenUser(t *testing.T) {
	raw := `current-context: c
clusters: [{name: c, cluster: {server: "https://api:6443"}}]
users: [{name: u, user: {token: sa-token-value}}]
contexts: [{name: c, context: {cluster: c, user: u}}]
`
	cfg, err := ParseConfig([]byte(raw))
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	if cfg.BearerToken != "sa-token-value" {
		t.Fatalf("BearerToken = %q", cfg.BearerToken)
	}
}

// TestParseConfigRefusesExecPlugins: a managed cluster's kubeconfig mints its
// credential by running a binary. Ignoring the block would send every request
// unauthenticated and fail as a 401 with nothing pointing at the cause.
func TestParseConfigRefusesExecPlugins(t *testing.T) {
	raw := `current-context: c
clusters: [{name: c, cluster: {server: "https://api:6443"}}]
users: [{name: u, user: {exec: {command: aws}}}]
contexts: [{name: c, context: {cluster: c, user: u}}]
`
	_, err := ParseConfig([]byte(raw))
	if err == nil {
		t.Fatal("an exec-plugin kubeconfig was accepted and would fail unauthenticated")
	}
	for _, want := range []string{"aws", "exec credential plugins"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want it to mention %q", err, want)
		}
	}
}

func TestParseConfigRejectsIncompleteFiles(t *testing.T) {
	for _, tc := range []struct{ name, raw, wantIn string }{
		{
			"no current-context",
			"clusters: []\nusers: []\n",
			"current-context",
		},
		{
			"current-context names nothing",
			"current-context: missing\ncontexts: [{name: other, context: {cluster: c, user: u}}]\n",
			"names no context",
		},
		{
			"context names an absent cluster",
			"current-context: c\ncontexts: [{name: c, context: {cluster: gone, user: u}}]\nclusters: []\n",
			"no cluster named",
		},
		{
			"no credential at all",
			`current-context: c
clusters: [{name: c, cluster: {server: "https://api:6443"}}]
users: [{name: u, user: {}}]
contexts: [{name: c, context: {cluster: c, user: u}}]
`,
			"neither a token nor a client certificate",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseConfig([]byte(tc.raw))
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Fatalf("error = %q, want it to mention %q", err, tc.wantIn)
			}
		})
	}
}
