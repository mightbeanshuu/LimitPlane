package k8s

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPathLayoutMatchesTheAPIServer(t *testing.T) {
	c := &Client{}
	cases := []struct {
		name         string
		gvr          GVR
		ns, obj, sub string
		want         string
	}{
		{
			name: "core group has no /apis segment",
			gvr:  GVR{Group: "", Version: "v1", Resource: "secrets", Namespaced: true},
			ns:   "lp", obj: "keys",
			want: "/api/v1/namespaces/lp/secrets/keys",
		},
		{
			name: "custom resource",
			gvr:  GVR{Group: "limitplane.dev", Version: "v1alpha1", Resource: "tenants", Namespaced: true},
			ns:   "lp", obj: "acme",
			want: "/apis/limitplane.dev/v1alpha1/namespaces/lp/tenants/acme",
		},
		{
			name: "status subresource",
			gvr:  GVR{Group: "limitplane.dev", Version: "v1alpha1", Resource: "tenants", Namespaced: true},
			ns:   "lp", obj: "acme", sub: "status",
			want: "/apis/limitplane.dev/v1alpha1/namespaces/lp/tenants/acme/status",
		},
		{
			name: "empty namespace means all namespaces",
			gvr:  GVR{Group: "limitplane.dev", Version: "v1alpha1", Resource: "tenants", Namespaced: true},
			want: "/apis/limitplane.dev/v1alpha1/tenants",
		},
		{
			name: "cluster scoped resource never gets a namespace",
			gvr:  GVR{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"},
			ns:   "ignored", obj: "tenants.limitplane.dev",
			want: "/apis/apiextensions.k8s.io/v1/customresourcedefinitions/tenants.limitplane.dev",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := c.path(tc.gvr, tc.ns, tc.obj, tc.sub); got != tc.want {
				t.Errorf("path = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestStatusErrorsAreClassified(t *testing.T) {
	body := `{"kind":"Status","code":410,"reason":"Expired","message":"too old resource version"}`
	err := parseStatus(410, []byte(body))
	if !IsGone(err) {
		t.Fatalf("410 should be classified as Gone: %v", err)
	}
	if IsNotFound(err) || IsConflict(err) {
		t.Fatalf("410 misclassified: %v", err)
	}
	if got := err.Error(); got != "k8s: 410 Expired: too old resource version" {
		t.Fatalf("error text = %q", got)
	}

	if !IsNotFound(parseStatus(404, []byte(`{"code":404,"reason":"NotFound"}`))) {
		t.Error("404 should be NotFound")
	}
	if !IsConflict(parseStatus(409, []byte(`{"code":409,"reason":"Conflict"}`))) {
		t.Error("409 should be Conflict")
	}
	for _, code := range []int{500, 502, 503, 504, 429} {
		if !IsUnavailable(parseStatus(code, nil)) {
			t.Errorf("%d should be retryable", code)
		}
	}
	// A body that is not a Status object must still produce a usable error
	// rather than an empty message.
	if got := parseStatus(403, []byte("forbidden by webhook")).Error(); got == "" {
		t.Error("a non-JSON body should still yield a message")
	}
}

// A merge patch on a list REPLACES the whole list, so removing our finalizer
// that way would silently delete any finalizer another controller added
// between our read and our write — and the object would vanish before that
// controller had cleaned up. The JSON patch `test` op makes the API server
// verify the list is unchanged and reject the write otherwise.
func TestRemoveFinalizerUsesAGuardedJSONPatch(t *testing.T) {
	var gotType string
	var gotBody []byte
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotType = r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer ts.Close()

	c := New(&Config{Server: ts.URL, Namespace: "lp", Token: func() string { return "" }}, "test")
	obj := PartialObject{Metadata: ObjectMeta{
		Name: "acme", Namespace: "lp", ResourceVersion: "7",
		Finalizers: []string{"someone-else/cleanup", "limitplane.dev/revoke-api-key"},
	}}
	gvr := GVR{Group: "limitplane.dev", Version: "v1alpha1", Resource: "tenants", Namespaced: true}

	if err := c.RemoveFinalizer(context.Background(), gvr, obj, "limitplane.dev/revoke-api-key"); err != nil {
		t.Fatalf("RemoveFinalizer: %v", err)
	}
	if gotType != JSONPatch {
		t.Fatalf("content type = %q, want %q", gotType, JSONPatch)
	}
	var ops []map[string]any
	if err := json.Unmarshal(gotBody, &ops); err != nil {
		t.Fatalf("patch body is not a JSON patch: %v (%s)", err, gotBody)
	}
	if len(ops) != 2 || ops[0]["op"] != "test" || ops[1]["op"] != "remove" {
		t.Fatalf("expected a test+remove pair, got %s", gotBody)
	}
	// Index 1, not 0: the other controller's finalizer must be left alone.
	if ops[0]["path"] != "/metadata/finalizers/1" {
		t.Fatalf("patch targets the wrong index: %s", gotBody)
	}
}

func TestRemoveFinalizerIsANoOpWhenAbsent(t *testing.T) {
	called := false
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		_, _ = w.Write([]byte(`{}`))
	}))
	defer ts.Close()
	c := New(&Config{Server: ts.URL, Namespace: "lp", Token: func() string { return "" }}, "test")
	obj := PartialObject{Metadata: ObjectMeta{Name: "acme", Namespace: "lp"}}
	if err := c.RemoveFinalizer(context.Background(), EventGVR, obj, "nope"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if called {
		t.Fatal("removing an absent finalizer should not touch the API server")
	}
}

func TestAddFinalizerPreservesExistingOnes(t *testing.T) {
	var body []byte
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer ts.Close()
	c := New(&Config{Server: ts.URL, Namespace: "lp", Token: func() string { return "" }}, "test")
	obj := PartialObject{Metadata: ObjectMeta{
		Name: "acme", Namespace: "lp", ResourceVersion: "9",
		Finalizers: []string{"someone-else/cleanup"},
	}}
	if err := c.AddFinalizer(context.Background(), TenantTestGVR, obj, "limitplane.dev/revoke-api-key"); err != nil {
		t.Fatalf("AddFinalizer: %v", err)
	}
	var patch struct {
		Metadata struct {
			ResourceVersion string   `json:"resourceVersion"`
			Finalizers      []string `json:"finalizers"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(body, &patch); err != nil {
		t.Fatalf("bad patch: %v (%s)", err, body)
	}
	if len(patch.Metadata.Finalizers) != 2 || patch.Metadata.Finalizers[0] != "someone-else/cleanup" {
		t.Fatalf("existing finalizer was dropped: %s", body)
	}
	// Sending resourceVersion turns the merge patch into an optimistic write,
	// so a concurrent edit fails loudly instead of being overwritten.
	if patch.Metadata.ResourceVersion != "9" {
		t.Fatalf("patch omitted resourceVersion, making the write unguarded: %s", body)
	}
}

func TestTerminatingDetectsAPendingDeletion(t *testing.T) {
	stamp := "2026-09-04T10:00:00Z"
	empty := ""
	if (PartialObject{}).Terminating() {
		t.Error("an object with no deletionTimestamp is not terminating")
	}
	if (PartialObject{Metadata: ObjectMeta{DeletionTimestamp: &empty}}).Terminating() {
		t.Error("an empty deletionTimestamp is not a deletion")
	}
	if !(PartialObject{Metadata: ObjectMeta{DeletionTimestamp: &stamp}}).Terminating() {
		t.Error("a set deletionTimestamp means the object is being deleted")
	}
}

func TestPatchStatusTargetsTheSubresource(t *testing.T) {
	var gotPath, gotType string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotType = r.URL.Path, r.Header.Get("Content-Type")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer ts.Close()
	c := New(&Config{Server: ts.URL, Namespace: "lp", Token: func() string { return "" }}, "test")
	if err := c.PatchStatus(context.Background(), TenantTestGVR, "lp", "acme",
		map[string]any{"tier": "pro"}); err != nil {
		t.Fatalf("PatchStatus: %v", err)
	}
	// Writing through the main endpoint instead would race a user editing spec
	// and could silently revert their change.
	if gotPath != "/apis/limitplane.dev/v1alpha1/namespaces/lp/tenants/acme/status" {
		t.Fatalf("status patch went to %q", gotPath)
	}
	if gotType != MergePatch {
		t.Fatalf("content type = %q, want %q", gotType, MergePatch)
	}
}

// TenantTestGVR mirrors the operator's Tenant resource without importing it,
// which would be an import cycle.
var TenantTestGVR = GVR{Group: "limitplane.dev", Version: "v1alpha1", Resource: "tenants", Namespaced: true}

// Regression test for a bug that reached a real cluster.
//
// http.Client.Timeout bounds the ENTIRE request, including reading the response
// body. A watch IS a response body held open for minutes, so a timeout on the
// client that serves it kills every watch the moment it expires — reported as
// "context deadline exceeded (Client.Timeout ... while reading body)". The
// controller then reconnects forever, never receives a bookmark, and re-LISTs
// on every cycle. It looks healthy from the outside because reconciliation
// still happens on each reconnect.
//
// The existing reflector tests could not catch this: they all finish in well
// under the 30-second timeout. So the assertion is structural rather than
// behavioural — a test that waited out the timeout would add 30 seconds to
// every CI run to observe something a field comparison proves instantly.
func TestWatchClientHasNoTimeout(t *testing.T) {
	c := New(&Config{Server: "https://example", Namespace: "lp", Token: func() string { return "" }}, "test")

	if c.longPoll.Timeout != 0 {
		t.Fatalf("the watch client has a %v timeout; every watch will die after that and reconnect forever",
			c.longPoll.Timeout)
	}
	// The ordinary client must still be bounded: a hung GET on a Secret with no
	// deadline would wedge a reconcile indefinitely.
	if c.http.Timeout == 0 {
		t.Fatal("the request client has no timeout; a hung read would wedge a reconcile forever")
	}
}
