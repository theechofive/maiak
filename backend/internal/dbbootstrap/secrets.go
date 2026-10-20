package dbbootstrap

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// SecretStore reads and creates Kubernetes Secrets. The production implementation talks to
// the in-cluster API server; tests use an in-memory fake.
type SecretStore interface {
	// Get returns the Secret's data, or found=false if it does not exist.
	Get(ctx context.Context, namespace, name string) (data map[string][]byte, found bool, err error)
	// Create creates an Opaque Secret. It returns ErrSecretExists if the name is taken.
	Create(ctx context.Context, namespace, name string, data map[string][]byte) error
	// Update sets the given keys of an existing Secret, leaving the other keys unchanged.
	Update(ctx context.Context, namespace, name string, data map[string][]byte) error
}

// ErrSecretExists is returned by SecretStore.Create when the Secret already exists.
var ErrSecretExists = errors.New("dbbootstrap: secret already exists")

// ServiceAccountDir is where Kubernetes mounts the pod's service account credentials.
const ServiceAccountDir = "/var/run/secrets/kubernetes.io/serviceaccount"

// InClusterNamespace returns the pod's namespace from the service account mount.
func InClusterNamespace() (string, error) {
	b, err := os.ReadFile(filepath.Join(ServiceAccountDir, "namespace"))
	if err != nil {
		return "", fmt.Errorf("dbbootstrap: read namespace (set MAIAK_BOOTSTRAP_NAMESPACE outside a pod): %w", err)
	}
	return strings.TrimSpace(string(b)), nil
}

// KubeSecrets is a minimal client for the Secrets API using the pod's ServiceAccount token.
// It deliberately avoids client-go: two calls do not justify its dependency tree.
type KubeSecrets struct {
	baseURL   string
	tokenPath string
	client    *http.Client
}

// NewInClusterSecrets builds a client from KUBERNETES_SERVICE_HOST/PORT and the service
// account CA bundle and token.
func NewInClusterSecrets() (*KubeSecrets, error) {
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return nil, errors.New("dbbootstrap: not running in a Kubernetes pod (KUBERNETES_SERVICE_HOST/PORT unset)")
	}
	ca, err := os.ReadFile(filepath.Join(ServiceAccountDir, "ca.crt"))
	if err != nil {
		return nil, fmt.Errorf("dbbootstrap: read service account CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, errors.New("dbbootstrap: service account CA bundle has no certificates")
	}
	client := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			Proxy:           nil,
			TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
		},
	}
	return newKubeSecrets("https://"+net.JoinHostPort(host, port), filepath.Join(ServiceAccountDir, "token"), client), nil
}

func newKubeSecrets(baseURL, tokenPath string, client *http.Client) *KubeSecrets {
	return &KubeSecrets{baseURL: strings.TrimSuffix(baseURL, "/"), tokenPath: tokenPath, client: client}
}

type kubeSecret struct {
	APIVersion string            `json:"apiVersion"`
	Kind       string            `json:"kind"`
	Metadata   kubeMeta          `json:"metadata"`
	Type       string            `json:"type,omitempty"`
	Data       map[string][]byte `json:"data,omitempty"`
}

type kubeMeta struct {
	Name      string            `json:"name"`
	Namespace string            `json:"namespace,omitempty"`
	Labels    map[string]string `json:"labels,omitempty"`
}

type kubeStatus struct {
	Message string `json:"message"`
	Reason  string `json:"reason"`
}

// Get implements SecretStore.
func (k *KubeSecrets) Get(ctx context.Context, namespace, name string) (map[string][]byte, bool, error) {
	resp, err := k.do(ctx, http.MethodGet, k.secretsPath(namespace)+"/"+url.PathEscape(name), nil)
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		var s kubeSecret
		if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&s); err != nil {
			return nil, false, fmt.Errorf("dbbootstrap: decode secret %s/%s: %w", namespace, name, err)
		}
		return s.Data, true, nil
	case http.StatusNotFound:
		return nil, false, nil
	default:
		return nil, false, statusError(resp, "get", namespace, name)
	}
}

// Create implements SecretStore.
func (k *KubeSecrets) Create(ctx context.Context, namespace, name string, data map[string][]byte) error {
	body, err := json.Marshal(kubeSecret{
		APIVersion: "v1",
		Kind:       "Secret",
		Metadata: kubeMeta{Name: name, Namespace: namespace, Labels: map[string]string{
			"app.kubernetes.io/managed-by": "maiakctl",
			"app.kubernetes.io/part-of":    "maiak",
		}},
		Type: "Opaque",
		Data: data,
	})
	if err != nil {
		return err
	}
	resp, err := k.do(ctx, http.MethodPost, k.secretsPath(namespace), body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusCreated, http.StatusOK:
		return nil
	case http.StatusConflict:
		return ErrSecretExists
	default:
		return statusError(resp, "create", namespace, name)
	}
}

// Update implements SecretStore with a JSON merge patch of the data keys.
func (k *KubeSecrets) Update(ctx context.Context, namespace, name string, data map[string][]byte) error {
	body, err := json.Marshal(map[string]any{"data": data})
	if err != nil {
		return err
	}
	resp, err := k.doContent(ctx, http.MethodPatch, k.secretsPath(namespace)+"/"+url.PathEscape(name), body, "application/merge-patch+json")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return statusError(resp, "update", namespace, name)
	}
	return nil
}

func (k *KubeSecrets) secretsPath(namespace string) string {
	return "/api/v1/namespaces/" + url.PathEscape(namespace) + "/secrets"
}

func (k *KubeSecrets) do(ctx context.Context, method, path string, body []byte) (*http.Response, error) {
	return k.doContent(ctx, method, path, body, "application/json")
}

func (k *KubeSecrets) doContent(ctx context.Context, method, path string, body []byte, contentType string) (*http.Response, error) {
	// Re-read the token every call: projected tokens are rotated by the kubelet.
	token, err := os.ReadFile(k.tokenPath)
	if err != nil {
		return nil, fmt.Errorf("dbbootstrap: read service account token: %w", err)
	}
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, k.baseURL+path, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := k.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("dbbootstrap: kubernetes api %s %s: %w", method, path, err)
	}
	return resp, nil
}

func statusError(resp *http.Response, op, namespace, name string) error {
	var st kubeStatus
	_ = json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&st)
	return fmt.Errorf("dbbootstrap: %s secret %s/%s: HTTP %d %s: %s", op, namespace, name, resp.StatusCode, st.Reason, st.Message)
}
