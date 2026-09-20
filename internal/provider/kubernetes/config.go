package kubernetes

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"os"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/evatt-labs/kraai/internal/kerrors"
)

// Config is everything needed to reach one cluster's API server: where it is,
// how to trust it, and how to authenticate to it.
type Config struct {
	// Server is the API server's base URL, e.g. "https://10.0.1.5:6443".
	Server string
	// TLS carries the cluster's CA and, for certificate auth, the client
	// keypair.
	TLS *tls.Config
	// BearerToken authenticates a service account. Empty when the kubeconfig
	// authenticates with a client certificate instead.
	BearerToken string
}

// kubeconfig is the subset of a kubeconfig file this package reads.
//
// Deliberately partial. A kubeconfig can carry many clusters, users and
// contexts, plus proxy settings, impersonation and exec credential plugins;
// what kraai needs is the one context in use and the credential it names. An
// unrecognised field is ignored rather than rejected, because a file kraai
// did not write is not kraai's to validate.
type kubeconfig struct {
	CurrentContext string `yaml:"current-context"`
	Clusters       []struct {
		Name    string `yaml:"name"`
		Cluster struct {
			Server                   string `yaml:"server"`
			CertificateAuthorityData string `yaml:"certificate-authority-data"`
			CertificateAuthority     string `yaml:"certificate-authority"`
			InsecureSkipTLSVerify    bool   `yaml:"insecure-skip-tls-verify"`
		} `yaml:"cluster"`
	} `yaml:"clusters"`
	Users []struct {
		Name string `yaml:"name"`
		User struct {
			ClientCertificateData string `yaml:"client-certificate-data"`
			ClientCertificate     string `yaml:"client-certificate"`
			ClientKeyData         string `yaml:"client-key-data"`
			ClientKey             string `yaml:"client-key"`
			Token                 string `yaml:"token"`
			// Exec names an external command that mints a credential —
			// how EKS, GKE and AKS authenticate. Detected so ParseConfig
			// can refuse clearly instead of failing later with an
			// unauthenticated request. See ParseConfig.
			Exec *struct {
				Command string `yaml:"command"`
			} `yaml:"exec"`
		} `yaml:"user"`
	} `yaml:"users"`
	Contexts []struct {
		Name    string `yaml:"name"`
		Context struct {
			Cluster   string `yaml:"cluster"`
			User      string `yaml:"user"`
			Namespace string `yaml:"namespace"`
		} `yaml:"context"`
	} `yaml:"contexts"`
}

// ParseConfig builds a Config from the bytes of a kubeconfig file, using its
// current-context.
//
// Exec credential plugins are refused by name rather than ignored. A
// kubeconfig for a managed cluster authenticates by running a binary
// (`aws eks get-token` and its equivalents), and a request sent without that
// credential fails as a 401 far from the cause. Refusing here names the
// command the file expects and says plainly that it is unsupported.
func ParseConfig(data []byte) (*Config, error) {
	var file kubeconfig
	if err := yaml.Unmarshal(data, &file); err != nil {
		return nil, kerrors.Wrap(err, kerrors.CodeValidation, "parsing kubeconfig")
	}
	if file.CurrentContext == "" {
		return nil, kerrors.Validation("kubeconfig names no current-context")
	}

	clusterName, userName := "", ""
	for _, c := range file.Contexts {
		if c.Name == file.CurrentContext {
			clusterName, userName = c.Context.Cluster, c.Context.User
			break
		}
	}
	if clusterName == "" {
		return nil, kerrors.Validation(
			"kubeconfig's current-context %q names no context in the file", file.CurrentContext)
	}

	cfg := &Config{TLS: &tls.Config{MinVersion: tls.VersionTLS12}}

	found := false
	for _, c := range file.Clusters {
		if c.Name != clusterName {
			continue
		}
		found = true
		cfg.Server = strings.TrimSuffix(c.Cluster.Server, "/")
		cfg.TLS.InsecureSkipVerify = c.Cluster.InsecureSkipTLSVerify // G402: only when the kubeconfig explicitly asks for it
		ca, err := readMaybeFile(c.Cluster.CertificateAuthorityData, c.Cluster.CertificateAuthority)
		if err != nil {
			return nil, kerrors.Wrap(err, kerrors.CodeValidation, "cluster %q certificate authority", clusterName)
		}
		if len(ca) > 0 {
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(ca) {
				return nil, kerrors.Validation("cluster %q certificate authority is not valid PEM", clusterName)
			}
			cfg.TLS.RootCAs = pool
		}
	}
	if !found {
		return nil, kerrors.Validation("kubeconfig has no cluster named %q", clusterName)
	}
	if cfg.Server == "" {
		return nil, kerrors.Validation("cluster %q declares no server address", clusterName)
	}

	for _, u := range file.Users {
		if u.Name != userName {
			continue
		}
		if u.User.Exec != nil {
			return nil, kerrors.Validation(
				"kubeconfig user %q authenticates by running %q, which kraai does not support — "+
					"exec credential plugins are how managed clusters (EKS, GKE, AKS) mint tokens; "+
					"use a kubeconfig carrying a service account token or a client certificate instead",
				userName, u.User.Exec.Command)
		}
		cfg.BearerToken = u.User.Token

		cert, err := readMaybeFile(u.User.ClientCertificateData, u.User.ClientCertificate)
		if err != nil {
			return nil, kerrors.Wrap(err, kerrors.CodeValidation, "user %q client certificate", userName)
		}
		keyPEM, err := readMaybeFile(u.User.ClientKeyData, u.User.ClientKey)
		if err != nil {
			return nil, kerrors.Wrap(err, kerrors.CodeValidation, "user %q client key", userName)
		}
		if len(cert) > 0 && len(keyPEM) > 0 {
			pair, err := tls.X509KeyPair(cert, keyPEM)
			if err != nil {
				return nil, kerrors.Wrap(err, kerrors.CodeValidation, "user %q client keypair", userName)
			}
			cfg.TLS.Certificates = []tls.Certificate{pair}
		}
	}

	if cfg.BearerToken == "" && len(cfg.TLS.Certificates) == 0 {
		return nil, kerrors.Validation(
			"kubeconfig user %q carries neither a token nor a client certificate", userName)
	}
	return cfg, nil
}

// readMaybeFile returns inline base64 data when present, otherwise the
// contents of path. Both empty is not an error: a kubeconfig may legitimately
// omit a CA (a publicly-trusted server) or a client certificate (token auth).
func readMaybeFile(inline, path string) ([]byte, error) {
	if inline != "" {
		decoded, err := base64.StdEncoding.DecodeString(inline)
		if err != nil {
			return nil, fmt.Errorf("decoding inline data: %w", err)
		}
		return decoded, nil
	}
	if path == "" {
		return nil, nil
	}
	// The path comes from the kubeconfig the caller supplied, which is the
	// same file already trusted to say which server to talk to and which
	// credential to send. A caller able to choose it can read the file
	// anyway.
	return os.ReadFile(path) //nolint:gosec // G304: path is from the operator's own kubeconfig
}
