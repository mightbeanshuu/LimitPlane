package k8s

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Client is a REST client for one API server.
//
// It is deliberately UNTYPED at the transport layer: every method moves
// json.RawMessage around and lets the caller unmarshal into whatever struct it
// cares about. Kubernetes objects are open (a CRD gains fields when someone
// edits it) and a client that insists on a closed Go struct silently drops
// anything it does not know about — which, on a write path, is how controllers
// clobber fields written by other controllers.
type Client struct {
	cfg *Config
	// Two HTTP clients, and the split is not cosmetic.
	//
	// http.Client.Timeout bounds the WHOLE request, including reading the
	// response body. A watch is a response body that is meant to stay open for
	// minutes, so a timeout here kills every watch the moment it expires — with
	// the misleading error "context deadline exceeded (Client.Timeout ... while
	// reading body)". The controller then reconnects in a loop, never receives
	// a bookmark, and re-LISTs constantly. It looks like it is working, because
	// reconciliation still happens on every reconnect.
	//
	// So: `http` bounds ordinary requests, `longPoll` has no timeout at all and
	// is bounded by the caller's context and the server's own timeoutSeconds.
	http     *http.Client
	longPoll *http.Client
	ua       string
}

// GVR identifies a resource the way the API server's URL layout does.
//
//	Group ""            -> /api/v1/...                  (core: pods, secrets)
//	Group "apps"        -> /apis/apps/v1/...
//	Group "limitplane.dev" -> /apis/limitplane.dev/v1alpha1/...
type GVR struct {
	Group      string // "" for the core group
	Version    string // "v1", "v1alpha1"
	Resource   string // the PLURAL, lowercase name: "ratelimitpolicies"
	Namespaced bool
}

// New builds a client. The timeout applies to ordinary requests; watches use
// their own unbounded context because a watch that has not seen an event in
// thirty seconds is healthy, not hung.
func New(cfg *Config, version string) *Client {
	return &Client{
		cfg: cfg,
		http: &http.Client{
			Transport: cfg.Transport,
			Timeout:   30 * time.Second,
		},
		// No Timeout. Silence on a watch is a calm cluster, not a stalled
		// request; the watch is ended by ctx or by the server's timeoutSeconds.
		longPoll: &http.Client{Transport: cfg.Transport},
		ua:       "limitplane/" + version + " (+https://github.com/mightbeanshuu/LimitPlane)",
	}
}

// Namespace is the namespace this pod runs in.
func (c *Client) Namespace() string { return c.cfg.Namespace }

// Server is the API endpoint, useful for logs and the /healthz payload.
func (c *Client) Server() string { return c.cfg.Server }

// StatusError is a non-2xx response from the API server, carrying the fields
// controllers actually branch on.
type StatusError struct {
	Code    int    // HTTP status
	Reason  string // "NotFound", "Conflict", "Gone", "Forbidden", ...
	Message string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("k8s: %d %s: %s", e.Code, e.Reason, e.Message)
}

// The three outcomes a reconcile loop must tell apart, exposed as predicates so
// call sites read as intent rather than as status-code arithmetic.
func IsNotFound(err error) bool { return codeIs(err, http.StatusNotFound) }
func IsConflict(err error) bool { return codeIs(err, http.StatusConflict) }

// IsGone reports the single most important error in any watch-based controller:
// HTTP 410, "too old resource version". The API server only keeps a short
// window of history, so a client that reconnects after a pause may ask to
// resume from a point the server has already forgotten. The ONLY correct
// response is to throw away the cached resourceVersion and re-LIST — retrying
// the same watch just loops forever, and ignoring it means silently missing
// every change that happened while you were away.
func IsGone(err error) bool { return codeIs(err, http.StatusGone) }

// IsUnavailable covers the transient failures worth retrying quietly: the API
// server rolling, a proxy blipping, a 429 from the apiserver's own priority
// and fairness layer.
func IsUnavailable(err error) bool {
	return codeIs(err, http.StatusServiceUnavailable) ||
		codeIs(err, http.StatusTooManyRequests) ||
		codeIs(err, http.StatusInternalServerError) ||
		codeIs(err, http.StatusBadGateway) ||
		codeIs(err, http.StatusGatewayTimeout)
}

func codeIs(err error, code int) bool {
	var se *StatusError
	return errors.As(err, &se) && se.Code == code
}

// path builds the REST path for a resource, with the namespace and name
// segments included only when they apply.
func (c *Client) path(gvr GVR, namespace, name, sub string) string {
	var b strings.Builder
	if gvr.Group == "" {
		b.WriteString("/api/" + gvr.Version)
	} else {
		b.WriteString("/apis/" + gvr.Group + "/" + gvr.Version)
	}
	if gvr.Namespaced && namespace != "" {
		b.WriteString("/namespaces/" + namespace)
	}
	b.WriteString("/" + gvr.Resource)
	if name != "" {
		b.WriteString("/" + name)
	}
	if sub != "" {
		b.WriteString("/" + sub)
	}
	return b.String()
}

// do performs one request and returns the raw body. contentType is empty for
// reads; writes set it (merge patch, JSON patch, or server-side apply).
func (c *Client) do(ctx context.Context, method, path string, q url.Values, contentType string, body []byte) ([]byte, error) {
	resp, err := c.stream(ctx, method, path, q, contentType, body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	// 8MB is far above any single Kubernetes object (the etcd limit is 1.5MB)
	// and stops a hostile or broken endpoint from ballooning the operator's
	// memory, which in a rate limiter would be an especially ironic outage.
	return io.ReadAll(io.LimitReader(resp.Body, 8<<20))
}

// stream performs a request and hands back the LIVE body, unread, using the
// timeout-bounded client. Used by do() for ordinary requests.
func (c *Client) stream(ctx context.Context, method, path string, q url.Values, contentType string, body []byte) (*http.Response, error) {
	return c.send(c.http, ctx, method, path, q, contentType, body)
}

// watchStream is stream() on the UNBOUNDED client. Only the reflector uses it.
func (c *Client) watchStream(ctx context.Context, path string, q url.Values) (*http.Response, error) {
	return c.send(c.longPoll, ctx, http.MethodGet, path, q, "", nil)
}

func (c *Client) send(hc *http.Client, ctx context.Context, method, path string, q url.Values, contentType string, body []byte) (*http.Response, error) {
	u := c.cfg.Server + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rdr)
	if err != nil {
		return nil, err
	}
	if tok := c.cfg.Token(); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.ua)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}

	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp, nil
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	return nil, parseStatus(resp.StatusCode, raw)
}

// parseStatus turns the API server's Status object into a StatusError. The
// body is what carries `reason`, and reason is more reliable than the status
// code for telling apart, say, an admission webhook rejection from a conflict.
func parseStatus(code int, raw []byte) error {
	var s struct {
		Reason  string `json:"reason"`
		Message string `json:"message"`
	}
	_ = json.Unmarshal(raw, &s)
	if s.Message == "" {
		s.Message = strings.TrimSpace(string(raw))
	}
	if s.Reason == "" {
		s.Reason = http.StatusText(code)
	}
	return &StatusError{Code: code, Reason: s.Reason, Message: s.Message}
}

// ---- reads -----------------------------------------------------------------

// List fetches every object of a kind and returns the raw items plus the
// resourceVersion of the LIST itself.
//
// That resourceVersion is the whole point: it is the exact position in the
// cluster's change stream that this snapshot represents, and starting a watch
// from it is what guarantees no event is missed and none is seen twice. Reading
// the objects and then watching "from now" is the subtle, common bug — anything
// that changed in between is lost forever.
func (c *Client) List(ctx context.Context, gvr GVR, namespace string, q url.Values) (items []json.RawMessage, resourceVersion string, err error) {
	raw, err := c.do(ctx, http.MethodGet, c.path(gvr, namespace, "", ""), q, "", nil)
	if err != nil {
		return nil, "", err
	}
	var list struct {
		Metadata struct {
			ResourceVersion string `json:"resourceVersion"`
		} `json:"metadata"`
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, "", fmt.Errorf("k8s: decoding list of %s: %w", gvr.Resource, err)
	}
	return list.Items, list.Metadata.ResourceVersion, nil
}

// Get fetches one object.
func (c *Client) Get(ctx context.Context, gvr GVR, namespace, name string) (json.RawMessage, error) {
	return c.do(ctx, http.MethodGet, c.path(gvr, namespace, name, ""), nil, "", nil)
}

// ---- writes ----------------------------------------------------------------

// The three content types the API server accepts for a PATCH. Which one you
// pick decides how your write interacts with everyone else's.
const (
	// MergePatch replaces the fields you name and leaves the rest. Simple, and
	// correct for a controller writing its own status subresource.
	MergePatch = "application/merge-patch+json"
	// JSONPatch is the surgical form: a list of test/replace/remove ops. The
	// `test` op gives optimistic concurrency without a resourceVersion, which
	// is how a finalizer is removed safely.
	JSONPatch = "application/json-patch+json"
	// ApplyPatch is server-side apply, where the API server tracks WHICH manager
	// owns WHICH field and rejects conflicting writes. The content type says
	// YAML, but every JSON document is also valid YAML — so server-side apply
	// works from this client with no YAML encoder at all.
	ApplyPatch = "application/apply-patch+yaml"
)

// Patch applies a patch to an object, optionally to a subresource ("status").
func (c *Client) Patch(ctx context.Context, gvr GVR, namespace, name, sub, contentType string, patch []byte) (json.RawMessage, error) {
	return c.do(ctx, http.MethodPatch, c.path(gvr, namespace, name, sub), nil, contentType, patch)
}

// PatchStatus is the single write a well-behaved controller makes most often.
//
// It targets the /status subresource on purpose. Writing status through the
// main endpoint would mean sending the whole object back, which races the user
// editing spec at the same moment and can silently revert their change. The
// subresource split means a controller physically cannot clobber spec.
func (c *Client) PatchStatus(ctx context.Context, gvr GVR, namespace, name string, status any) error {
	body, err := json.Marshal(map[string]any{"status": status})
	if err != nil {
		return err
	}
	_, err = c.Patch(ctx, gvr, namespace, name, "status", MergePatch, body)
	return err
}

// Apply performs a server-side apply as the named field manager. Used for
// objects LimitPlane OWNS (its Lease, its generated config), never for objects
// a human wrote.
func (c *Client) Apply(ctx context.Context, gvr GVR, namespace, name string, obj any, fieldManager string, force bool) (json.RawMessage, error) {
	body, err := json.Marshal(obj)
	if err != nil {
		return nil, err
	}
	q := url.Values{"fieldManager": {fieldManager}}
	if force {
		q.Set("force", "true")
	}
	return c.do(ctx, http.MethodPatch, c.path(gvr, namespace, name, ""), q, ApplyPatch, body)
}

// Create POSTs a new object.
func (c *Client) Create(ctx context.Context, gvr GVR, namespace string, obj any) (json.RawMessage, error) {
	body, err := json.Marshal(obj)
	if err != nil {
		return nil, err
	}
	return c.do(ctx, http.MethodPost, c.path(gvr, namespace, "", ""), nil, "application/json", body)
}

// Update PUTs a whole object. The object must carry the resourceVersion it was
// read at; the API server rejects the write with 409 if anything changed in
// between. That conflict is not a failure — it is the concurrency control
// working, and the caller is expected to re-read and retry.
func (c *Client) Update(ctx context.Context, gvr GVR, namespace, name string, obj any) (json.RawMessage, error) {
	body, err := json.Marshal(obj)
	if err != nil {
		return nil, err
	}
	return c.do(ctx, http.MethodPut, c.path(gvr, namespace, name, ""), nil, "application/json", body)
}

// ---- object metadata -------------------------------------------------------

// ObjectMeta is the slice of every Kubernetes object's metadata that a
// controller reads. Generation and ResourceVersion are the two that matter:
//
//	Generation      increments ONLY when spec changes. Comparing it against
//	                status.observedGeneration is how a controller knows whether
//	                it has caught up — and how it avoids reconciling in an
//	                infinite loop when its own status write comes back as a
//	                MODIFIED event.
//	ResourceVersion is an opaque cluster-wide cursor. Never parse it, never
//	                compare it numerically; only ever hand it back verbatim.
type ObjectMeta struct {
	Name              string            `json:"name"`
	Namespace         string            `json:"namespace,omitempty"`
	UID               string            `json:"uid,omitempty"`
	ResourceVersion   string            `json:"resourceVersion,omitempty"`
	Generation        int64             `json:"generation,omitempty"`
	CreationTimestamp string            `json:"creationTimestamp,omitempty"`
	DeletionTimestamp *string           `json:"deletionTimestamp,omitempty"`
	Labels            map[string]string `json:"labels,omitempty"`
	Annotations       map[string]string `json:"annotations,omitempty"`
	Finalizers        []string          `json:"finalizers,omitempty"`
}

// PartialObject is enough of any object to route it: what kind it is, what it
// is called, and whether it is being deleted.
type PartialObject struct {
	APIVersion string     `json:"apiVersion"`
	Kind       string     `json:"kind"`
	Metadata   ObjectMeta `json:"metadata"`
}

// Terminating reports whether the object has been marked for deletion.
//
// An object with finalizers does not vanish when someone runs `kubectl delete`
// — the API server sets deletionTimestamp and then WAITS. That pause is the
// controller's only chance to clean up external state (here: dropping the
// tenant out of the live policy so its API key stops working) before the object
// disappears. Miss it and the tenant keeps being served after being deleted.
func (o PartialObject) Terminating() bool {
	return o.Metadata.DeletionTimestamp != nil && *o.Metadata.DeletionTimestamp != ""
}

// HasFinalizer reports whether the given finalizer is present.
func (o PartialObject) HasFinalizer(name string) bool {
	for _, f := range o.Metadata.Finalizers {
		if f == name {
			return true
		}
	}
	return false
}

// AddFinalizer registers a finalizer via a merge patch that preserves any
// finalizers other controllers have added.
func (c *Client) AddFinalizer(ctx context.Context, gvr GVR, o PartialObject, name string) error {
	if o.HasFinalizer(name) {
		return nil
	}
	next := append(append([]string{}, o.Metadata.Finalizers...), name)
	body, err := json.Marshal(map[string]any{
		"metadata": map[string]any{
			// Sending resourceVersion with a merge patch turns it into an
			// optimistic write: if someone else edited the object first, this
			// fails with 409 instead of overwriting their finalizer list.
			"resourceVersion": o.Metadata.ResourceVersion,
			"finalizers":      next,
		},
	})
	if err != nil {
		return err
	}
	_, err = c.Patch(ctx, gvr, o.Metadata.Namespace, o.Metadata.Name, "", MergePatch, body)
	return err
}

// RemoveFinalizer releases the object so Kubernetes can finish deleting it.
//
// This uses a JSON patch with a `test` op rather than a merge patch, because
// merge-patching a list means sending the whole list — and if another
// controller added its own finalizer between our read and our write, we would
// delete theirs and let the object vanish before they had cleaned up. The
// `test` op makes the API server verify the list is still exactly what we saw,
// and reject the write with 409 if it is not.
func (c *Client) RemoveFinalizer(ctx context.Context, gvr GVR, o PartialObject, name string) error {
	idx := -1
	for i, f := range o.Metadata.Finalizers {
		if f == name {
			idx = i
			break
		}
	}
	if idx < 0 {
		return nil
	}
	patch, err := json.Marshal([]map[string]any{
		{"op": "test", "path": "/metadata/finalizers/" + strconv.Itoa(idx), "value": name},
		{"op": "remove", "path": "/metadata/finalizers/" + strconv.Itoa(idx)},
	})
	if err != nil {
		return err
	}
	_, err = c.Patch(ctx, gvr, o.Metadata.Namespace, o.Metadata.Name, "", JSONPatch, patch)
	return err
}
