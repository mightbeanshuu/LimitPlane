// Package k8s talks to the Kubernetes API server using nothing but net/http
// and encoding/json.
//
// WHY NOT client-go? Because LimitPlane's whole premise is that it has no
// third-party dependencies, and client-go is the single largest dependency
// tree in the Go ecosystem — pulling it in would add roughly 40 modules and
// tens of megabytes to a project whose binary is currently 6.8MB. The
// apiserver, though, is *just a REST API over HTTPS*: LIST is a GET, WATCH is
// a GET that never closes, and an update is a PATCH. Everything the operator
// needs is reachable with the standard library, and writing it out makes the
// machinery legible instead of magic.
//
// What that costs, stated honestly: no informer cache, no typed clientsets, no
// kubeconfig YAML parsing (parsing YAML would itself need a dependency). What
// it buys: you can read the entire Kubernetes integration end to end in an
// afternoon, and the zero-dependency CI gate still passes.
package k8s

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// The paths kubelet mounts into every pod that has a service account. These are
// the operator's entire credential set — there is no kubeconfig in a pod.
const (
	saDir     = "/var/run/secrets/kubernetes.io/serviceaccount"
	tokenPath = saDir + "/token"
	caPath    = saDir + "/ca.crt"
	nsPath    = saDir + "/namespace"
)

// ErrNotInCluster means the process is not running as a pod, so there is no
// service account to authenticate with. Callers treat this as "Kubernetes
// integration is off", never as a fatal error: LimitPlane must still boot on a
// laptop, on Render, and in a plain Docker container.
var ErrNotInCluster = errors.New("k8s: not running inside a cluster")

// Config is everything needed to reach one API server.
type Config struct {
	// Server is the scheme+host+port, e.g. https://10.96.0.1:443.
	Server string
	// Namespace is the pod's own namespace — where Leases and Events are written.
	Namespace string
	// Token returns a bearer token. It is a func, not a string, because
	// projected service account tokens EXPIRE (one hour by default since 1.21)
	// and kubelet rewrites the file in place. A long-lived operator that read
	// the token once at boot works perfectly for an hour and then starts
	// failing with 401s — a genuinely nasty bug to find, so the whole design
	// avoids ever holding the value.
	Token     func() string
	Transport http.RoundTripper
	InCluster bool
}

// InClusterConfig builds a Config from the pod's own service account.
func InClusterConfig() (*Config, error) {
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return nil, ErrNotInCluster
	}
	if _, err := os.Stat(tokenPath); err != nil {
		return nil, fmt.Errorf("%w: no service account token at %s", ErrNotInCluster, tokenPath)
	}

	caPEM, err := os.ReadFile(caPath)
	if err != nil {
		return nil, fmt.Errorf("k8s: reading cluster CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("k8s: cluster CA bundle contained no usable certificate")
	}

	ns := "default"
	if b, err := os.ReadFile(nsPath); err == nil && len(b) > 0 {
		ns = strings.TrimSpace(string(b))
	}
	if override := os.Getenv("POD_NAMESPACE"); override != "" {
		ns = override
	}

	// The API server is reached by its in-cluster IP, so the certificate is
	// verified against the cluster CA — never InsecureSkipVerify. An operator
	// that skips verification is one DNS hijack away from handing its token to
	// an attacker, and it holds enough RBAC to read every Secret it is granted.
	return &Config{
		Server:    "https://" + net.JoinHostPort(host, port),
		Namespace: ns,
		Token:     newTokenSource(tokenPath, 30*time.Second).get,
		InCluster: true,
		Transport: &http.Transport{
			TLSClientConfig:     &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
			MaxIdleConnsPerHost: 8,
			// Watches are long-lived HTTP GETs that can go quiet for minutes at
			// a time. Keepalives are what tell us the difference between "the
			// cluster is calm" and "the connection died silently behind a NAT",
			// which is the classic way a watch-based controller wedges forever.
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
			ForceAttemptHTTP2:     true,
		},
	}, nil
}

// ConfigFromEnv is the out-of-cluster escape hatch, used for local development
// and by the e2e tests.
//
// It deliberately does NOT parse ~/.kube/config: that file is YAML, and a YAML
// parser is a dependency. Instead you point it at an API endpoint you already
// have — most easily `kubectl proxy`, which authenticates on your behalf and
// serves the API unauthenticated on localhost:8001.
//
//	kubectl proxy &
//	KUBE_API_SERVER=http://127.0.0.1:8001 go run ./cmd/gateway
func ConfigFromEnv() (*Config, error) {
	server := os.Getenv("KUBE_API_SERVER")
	if server == "" {
		return nil, ErrNotInCluster
	}
	cfg := &Config{
		Server:    strings.TrimRight(server, "/"),
		Namespace: envOr("POD_NAMESPACE", "default"),
		Token:     func() string { return os.Getenv("KUBE_API_TOKEN") },
	}
	if os.Getenv("KUBE_API_INSECURE") == "true" {
		// Only ever reachable by explicitly opting in via an env var, and only
		// meaningful for a throwaway kind cluster.
		cfg.Transport = &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // opt-in, dev only
		}
	}
	return cfg, nil
}

// AutoConfig prefers the in-cluster identity and falls back to the environment.
// It returns ErrNotInCluster when neither is available, which callers use to
// decide whether to start the operator at all.
func AutoConfig() (*Config, error) {
	if cfg, err := InClusterConfig(); err == nil {
		return cfg, nil
	} else if !errors.Is(err, ErrNotInCluster) {
		return nil, err // a real problem: the token exists but the CA is broken
	}
	return ConfigFromEnv()
}

// tokenSource re-reads the projected token from disk, caching it briefly so a
// busy watch loop is not doing a syscall per request.
type tokenSource struct {
	path string
	ttl  time.Duration

	mu     sync.Mutex
	value  string
	readAt time.Time
}

func newTokenSource(path string, ttl time.Duration) *tokenSource {
	return &tokenSource{path: path, ttl: ttl}
}

func (t *tokenSource) get() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.value != "" && time.Since(t.readAt) < t.ttl {
		return t.value
	}
	b, err := os.ReadFile(t.path)
	if err != nil {
		// Keep serving the previous token: a transient read failure should not
		// turn every in-flight API call into an anonymous request.
		return t.value
	}
	t.value = strings.TrimSpace(string(b))
	t.readAt = time.Now()
	return t.value
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
