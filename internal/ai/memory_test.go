package ai

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// fixedClock is a deterministic stand-in for time.Now, so stored timestamps are
// assertable.
func fixedClock(at int64) func() int64 { return func() int64 { return at } }

func TestTermCounts(t *testing.T) {
	tests := []struct {
		name string
		text string
		want map[string]int
	}{
		{
			name: "plain words are lowercased",
			text: "Tenant ACME blocked",
			want: map[string]int{"tenant": 1, "acme": 1, "blocked": 1},
		},
		{
			name: "repeats are counted",
			text: "burst burst burst",
			want: map[string]int{"burst": 3},
		},
		{
			// The headline case: the compound survives whole AND is split, so
			// "visualise" alone can still find it.
			name: "hostname is kept whole and also split",
			text: "visualise.vercel.app",
			want: map[string]int{"visualise.vercel.app": 1, "visualise": 1, "vercel": 1, "app": 1},
		},
		{
			name: "paths keep their shape, the leading slash is not a token start",
			text: "GET /v1/demo/ping",
			want: map[string]int{"get": 1, "v1/demo/ping": 1, "v1": 1, "demo": 1, "ping": 1},
		},
		{
			name: "underscores and hyphens split too",
			text: "user_id-42",
			want: map[string]int{"user_id-42": 1, "user": 1, "id": 1, "42": 1},
		},
		{
			name: "ip addresses stay whole",
			text: "10.0.0.1",
			want: map[string]int{"10.0.0.1": 1, "10": 1},
		},
		{
			name: "colons in a limiter key",
			text: "acme:free:monthly",
			want: map[string]int{"acme:free:monthly": 1, "acme": 1, "free": 1, "monthly": 1},
		},
		{
			name: "single-character parts are dropped",
			text: "a.b",
			want: map[string]int{"a.b": 1},
		},
		{
			name: "trailing punctuation is not part of a token",
			text: "quota? (80%)",
			want: map[string]int{"quota": 1, "80": 1},
		},
		{
			name: "a compound repeated twice counts its parts twice",
			text: "visualise.app visualise.app",
			want: map[string]int{"visualise.app": 2, "visualise": 2, "app": 2},
		},
		{name: "empty text", text: "", want: map[string]int{}},
		{name: "punctuation only", text: "--- !!! ???", want: map[string]int{}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := termCounts(tc.text); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("termCounts(%q)\n got %v\nwant %v", tc.text, got, tc.want)
			}
		})
	}
}

func TestCompoundSplitFindsWholeToken(t *testing.T) {
	m := NewMemory("", fixedClock(1000), 0)
	m.Remember("deploy check on visualise.vercel.app looked fine", nil)
	m.Remember("acme hit their monthly cap on /v1/demo/ping", nil)

	// The docstring promise: a plain word finds the compound that contains it.
	hits := m.Search("visualise quota?", 5)
	if len(hits) != 1 {
		t.Fatalf("Search(\"visualise quota?\") returned %d hits, want 1", len(hits))
	}
	if !strings.Contains(hits[0].Text, "visualise.vercel.app") {
		t.Errorf("wrong document retrieved: %q", hits[0].Text)
	}

	// And the reverse: searching the path part finds the doc that wrote the path.
	if hits := m.Search("ping", 5); len(hits) != 1 || !strings.Contains(hits[0].Text, "/v1/demo/ping") {
		t.Errorf("Search(\"ping\") = %+v, want the route document", hits)
	}
}

func TestSearchRanksRareTermsAbove(t *testing.T) {
	m := NewMemory("", fixedClock(1), 0)
	for i := 0; i < 8; i++ {
		m.Remember("tenant acme blocked burst", nil)
	}
	rare := m.Remember("tenant acme blocked visualise.vercel.app", nil)

	// "acme" is in every document, so IDF flattens it; "visualise" is in one.
	hits := m.Search("acme visualise", 5)
	if len(hits) < 2 {
		t.Fatalf("Search returned %d hits, want the rare doc plus common ones", len(hits))
	}
	if hits[0].ID != rare {
		t.Errorf("top hit id = %d, want %d (the document with the rare term)", hits[0].ID, rare)
	}
	if !(hits[0].Score > hits[1].Score) {
		t.Errorf("rare-term doc scored %v, no better than a common one at %v", hits[0].Score, hits[1].Score)
	}

	// Scores are cosine similarities: bounded, positive, and rounded to 3dp.
	for _, h := range hits {
		if h.Score <= 0 || h.Score > 1.0001 {
			t.Errorf("score %v out of range for a cosine similarity", h.Score)
		}
		if r := round3(h.Score); r != h.Score {
			t.Errorf("score %v is not rounded to three decimals", h.Score)
		}
	}
}

func TestSearchK(t *testing.T) {
	m := NewMemory("", fixedClock(1), 0)
	for i := 0; i < 12; i++ {
		m.Remember(fmt.Sprintf("event %d tenant acme blocked", i), nil)
	}

	tests := []struct {
		name string
		k    int
		want int
	}{
		{name: "explicit k", k: 3, want: 3},
		{name: "zero means the default", k: 0, want: defaultTopK},
		{name: "negative means the default", k: -1, want: defaultTopK},
		{name: "k larger than the corpus", k: 50, want: 12},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := len(m.Search("acme", tc.k)); got != tc.want {
				t.Errorf("Search(k=%d) returned %d hits, want %d", tc.k, got, tc.want)
			}
		})
	}
}

func TestSearchEmptyCases(t *testing.T) {
	empty := NewMemory("", fixedClock(1), 0)
	if got := empty.Search("anything", 5); got == nil || len(got) != 0 {
		t.Errorf("Search on an empty index = %v, want an empty non-nil slice", got)
	}

	m := NewMemory("", fixedClock(1), 0)
	m.Remember("tenant acme blocked", nil)

	tests := []struct{ name, query string }{
		{name: "blank query", query: ""},
		{name: "punctuation-only query", query: "???"},
		{name: "no shared tokens", query: "kangaroo"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := m.Search(tc.query, 5)
			if got == nil {
				t.Fatal("Search returned nil; it must return an empty slice so the API renders []")
			}
			if len(got) != 0 {
				t.Errorf("Search(%q) = %v, want no hits", tc.query, got)
			}
		})
	}
}

func TestEvictionDecrementsDocumentFrequency(t *testing.T) {
	m := NewMemory("", fixedClock(1), 2) // room for two documents

	m.Remember("shared alpha", nil)
	m.Remember("shared beta", nil)
	if got := m.Size(); got != 2 {
		t.Fatalf("Size() = %d, want 2", got)
	}
	if m.df["shared"] != 2 {
		t.Fatalf("df[shared] = %d, want 2", m.df["shared"])
	}

	m.Remember("shared gamma", nil) // evicts "shared alpha"

	if got := m.Size(); got != 2 {
		t.Errorf("Size() = %d, want the index capped at 2", got)
	}
	if got := m.df["shared"]; got != 2 {
		// 3 documents contained it, one was evicted: honest IDF says 2.
		t.Errorf("df[shared] = %d, want 2 — eviction must un-count the evicted doc", got)
	}
	if _, present := m.df["alpha"]; present {
		t.Errorf("df still holds %q from the evicted document; a zero must be deleted, not left behind", "alpha")
	}
	if m.df["beta"] != 1 || m.df["gamma"] != 1 {
		t.Errorf("df[beta]=%d df[gamma]=%d, want 1 each", m.df["beta"], m.df["gamma"])
	}

	// The evicted document must also be unreachable.
	if hits := m.Search("alpha", 5); len(hits) != 0 {
		t.Errorf("Search(\"alpha\") = %+v, want nothing after eviction", hits)
	}
	if hits := m.Search("gamma", 5); len(hits) != 1 {
		t.Errorf("Search(\"gamma\") = %+v, want the newest document", hits)
	}
}

func TestEvictionDropsCompoundParts(t *testing.T) {
	m := NewMemory("", fixedClock(1), 1)
	m.Remember("visualise.vercel.app", nil)
	m.Remember("plain text", nil) // evicts the compound

	for _, token := range []string{"visualise.vercel.app", "visualise", "vercel", "app"} {
		if _, present := m.df[token]; present {
			t.Errorf("df kept %q after its only document was evicted", token)
		}
	}
}

func TestIDFRewardsRarity(t *testing.T) {
	m := NewMemory("", fixedClock(1), 0)
	for i := 0; i < 9; i++ {
		m.Remember("common", nil)
	}
	m.Remember("rare", nil)

	n := m.Size()
	common, rare, unseen := m.idf("common", n), m.idf("rare", n), m.idf("nowhere", n)
	if !(common < rare && rare < unseen) {
		t.Errorf("idf ordering broken: common=%v rare=%v unseen=%v", common, rare, unseen)
	}
	if common <= 0 {
		t.Errorf("idf must stay positive even for ubiquitous tokens, got %v", common)
	}
}

func TestRememberReturnsSequentialIDs(t *testing.T) {
	m := NewMemory("", fixedClock(1), 0)
	for want := 1; want <= 5; want++ {
		if got := m.Remember("event", nil); got != want {
			t.Errorf("Remember() = %d, want %d", got, want)
		}
	}
}

func TestPersistenceRoundTrip(t *testing.T) {
	file := filepath.Join(t.TempDir(), "memory.jsonl")

	first := NewMemory(file, fixedClock(1700000000000), 0)
	idA := first.Remember("acme hit their monthly cap on /v1/demo/ping", map[string]any{"kind": "block", "type": "monthly_quota"})
	idB := first.Remember("deploy check on visualise.vercel.app looked fine", map[string]any{"kind": "admin"})
	if idA != 1 || idB != 2 {
		t.Fatalf("ids = %d, %d; want 1, 2", idA, idB)
	}

	// A second process opens the same file.
	second := NewMemory(file, fixedClock(1700000009999), 0)
	if got := second.Size(); got != 2 {
		t.Fatalf("Size() after reload = %d, want 2", got)
	}

	hits := second.Search("visualise", 5)
	if len(hits) != 1 {
		t.Fatalf("Search after reload returned %d hits, want 1", len(hits))
	}
	got := hits[0]
	if got.ID != idB {
		t.Errorf("id = %d, want %d", got.ID, idB)
	}
	if got.At != 1700000000000 {
		t.Errorf("at = %d, want the ORIGINAL timestamp, not the reload clock", got.At)
	}
	if got.Text != "deploy check on visualise.vercel.app looked fine" {
		t.Errorf("text = %q", got.Text)
	}
	if !reflect.DeepEqual(got.Meta, map[string]any{"kind": "admin"}) {
		t.Errorf("meta = %v, want it preserved through the file", got.Meta)
	}

	// Ids continue where the file left off rather than colliding.
	if id := second.Remember("a new incident", nil); id != 3 {
		t.Errorf("Remember() after reload = %d, want 3", id)
	}

	// And the append landed in the same file, in order.
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read memory file: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 3 {
		t.Fatalf("file has %d lines, want 3 (one JSON object per line)", len(lines))
	}
	for i, line := range lines {
		if !strings.HasPrefix(line, fmt.Sprintf(`{"id":%d,`, i+1)) {
			t.Errorf("line %d is out of id order: %s", i+1, line)
		}
	}
}

func TestLoadSkipsCorruptLines(t *testing.T) {
	file := filepath.Join(t.TempDir(), "memory.jsonl")
	content := strings.Join([]string{
		`{"id":1,"at":10,"text":"first incident acme","meta":{}}`,
		`{"id":2,"at":20,"text":"truncated`, // a crash mid-append
		``,                                  // a blank line
		`not json at all`,
		`{"id":3,"at":30,"text":"third incident globex","meta":{"kind":"block"}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(file, []byte(content), 0o600); err != nil {
		t.Fatalf("seed memory file: %v", err)
	}

	m := NewMemory(file, fixedClock(99), 0)
	if got := m.Size(); got != 2 {
		t.Fatalf("Size() = %d, want 2 — a bad line must cost one memory, not the rest of the file", got)
	}
	// Crucially, the document AFTER the corrupt line survived.
	if hits := m.Search("globex", 5); len(hits) != 1 || hits[0].ID != 3 {
		t.Errorf("Search(\"globex\") = %+v, want the record that followed the corrupt line", hits)
	}
	if id := m.Remember("next", nil); id != 4 {
		t.Errorf("Remember() = %d, want 4 (ids continue past the highest loaded id)", id)
	}
}

func TestMissingFileIsFirstBoot(t *testing.T) {
	m := NewMemory(filepath.Join(t.TempDir(), "does-not-exist-yet.jsonl"), fixedClock(1), 0)
	if got := m.Size(); got != 0 {
		t.Errorf("Size() = %d, want 0", got)
	}
	if id := m.Remember("first ever incident", nil); id != 1 {
		t.Errorf("Remember() = %d, want 1", id)
	}
}

func TestNoFileMeansMemoryOnly(t *testing.T) {
	m := NewMemory("", fixedClock(1), 0)
	m.Remember("nothing to persist", nil)
	if got := m.Size(); got != 1 {
		t.Errorf("Size() = %d, want 1", got)
	}
}

func TestMetaIsCopiedInAndOut(t *testing.T) {
	m := NewMemory("", fixedClock(1), 0)

	meta := map[string]any{"kind": "block"}
	m.Remember("acme blocked on burst", meta)
	meta["kind"] = "mutated-by-caller" // the caller reuses its map

	hits := m.Search("acme", 5)
	if len(hits) != 1 {
		t.Fatalf("Search returned %d hits, want 1", len(hits))
	}
	if hits[0].Meta["kind"] != "block" {
		t.Errorf("meta = %v; Remember must copy the caller's map", hits[0].Meta)
	}

	hits[0].Meta["kind"] = "mutated-by-reader" // the reader scribbles on its copy
	again := m.Search("acme", 5)
	if again[0].Meta["kind"] != "block" {
		t.Errorf("meta = %v; Search must hand out a copy, not the indexed map", again[0].Meta)
	}

	// A nil meta reads back as {} rather than null, so the API shape is stable.
	m.Remember("globex blocked on burst", nil)
	for _, h := range m.Search("globex", 5) {
		if h.Meta == nil {
			t.Error("meta = nil; a missing meta must serialise as {}")
		}
	}
}

func TestNewMemoryDefaults(t *testing.T) {
	m := NewMemory("", nil, 0)
	if m.maxDocs != defaultMaxDocs {
		t.Errorf("maxDocs = %d, want %d", m.maxDocs, defaultMaxDocs)
	}
	if m.now == nil {
		t.Fatal("now = nil; a nil clock must be replaced with the wall clock")
	}
	if m.now() <= 0 {
		t.Errorf("the default clock returned %d, want Unix milliseconds", m.now())
	}
	if neg := NewMemory("", nil, -3); neg.maxDocs != defaultMaxDocs {
		t.Errorf("maxDocs = %d for a negative cap, want %d", neg.maxDocs, defaultMaxDocs)
	}
}

// TestMemoryConcurrentRememberAndSearch is the point of the port: Node served
// this from one thread, Go serves it from many. Run under -race.
func TestMemoryConcurrentRememberAndSearch(t *testing.T) {
	const (
		writers      = 8
		readers      = 4
		perWriter    = 100
		indexCeiling = 200
	)

	m := NewMemory(filepath.Join(t.TempDir(), "memory.jsonl"), nil, indexCeiling)

	var (
		idMu sync.Mutex
		ids  = make(map[int]bool, writers*perWriter)
		wg   sync.WaitGroup
	)

	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				id := m.Remember(
					fmt.Sprintf("worker %d event %d tenant acme blocked burst on /v1/demo/ping", w, i),
					map[string]any{"kind": "test", "worker": w},
				)
				idMu.Lock()
				if ids[id] {
					t.Errorf("Remember handed out id %d twice", id)
				}
				ids[id] = true
				idMu.Unlock()
			}
		}(w)
	}

	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				for _, hit := range m.Search("tenant acme blocked ping", 5) {
					if hit.Text == "" {
						t.Error("Search returned a document with no text")
					}
					// Scribbling on the result proves it is not the live map:
					// under -race, sharing it would be a data race against the
					// other readers doing exactly the same thing.
					hit.Meta["kind"] = "mutated"
				}
				_ = m.Size()
			}
		}()
	}

	wg.Wait()

	if got := len(ids); got != writers*perWriter {
		t.Errorf("distinct ids = %d, want %d", got, writers*perWriter)
	}
	if got := m.Size(); got != indexCeiling {
		t.Errorf("Size() = %d, want the index capped at %d", got, indexCeiling)
	}

	// df must never exceed the number of live documents, which is the invariant
	// eviction is there to protect.
	m.mu.RLock()
	defer m.mu.RUnlock()
	for token, count := range m.df {
		if count > len(m.docs) {
			t.Fatalf("df[%q] = %d with only %d documents indexed", token, count, len(m.docs))
		}
		if count <= 0 {
			t.Fatalf("df[%q] = %d; zero counts must be deleted", token, count)
		}
	}
}
