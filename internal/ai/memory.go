// memoryStore — RAG over the gateway's own history, built by hand.
//
// RAG = Retrieval Augmented Generation: before asking a model a question,
// RETRIEVE the documents that matter and paste them into the prompt, so the
// answer comes from facts instead of vibes. Frameworks hide the pipeline; this
// file builds every stage in the open, so it is clear there is no magic:
//
//	1. CHUNK     each notable gateway event becomes one small text document
//	2. EMBED     each document becomes a bag of weighted tokens (TF-IDF)
//	3. RETRIEVE  cosine similarity between the question and every document
//	4. GROUND    the top matches go into the chat prompt, with ids to cite
//
// About the "embedding": real systems use neural vectors that capture MEANING,
// so "cap exceeded" matches "quota exhausted". TF-IDF only matches shared
// words — but it is deterministic, dependency-free, debuggable by reading, and
// honestly good on log-like text that is mostly exact names and ids. House rule
// applies: deterministic first, neural swap-in later behind the same
// Remember/Search pair.
//
// TF-IDF in one breath: a token matters in a document if it appears often there
// (term frequency) AND rarely everywhere else (inverse document frequency).
// "the" is everywhere, so its weight is ~0. "visualise.vercel.app" is rare, so
// it is heavy — and heavy rare terms are exactly what makes a log search useful.
//
// Concurrency is the reason this looks different from the Node original. There,
// one thread owned the index. Here, request handlers call Remember while the
// chat endpoint calls Search, on different cores, so the whole index sits
// behind an RWMutex: many concurrent searches, one writer at a time. Nothing
// internal is ever handed out by reference.

package ai

import (
	"bufio"
	"encoding/json"
	"math"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	defaultMaxDocs = 5000    // roughly a month of notable events; then the oldest fall off
	defaultTopK    = 5       // what search() defaulted to in the JS
	maxLineBytes   = 1 << 20 // one JSONL record cannot exceed this on reload
)

// tokenPattern finds words, and keeps the dots, slashes, colons, underscores
// and hyphens that make hostnames, paths, ids and IPs stay whole:
// "visualise.vercel.app" must survive as ONE token so an exact search for it
// scores hugely.
//
// Regex note for the port: the JS class was [a-z0-9.\/:_-]. Go's regexp is RE2,
// where "/" needs no escaping inside a class and a trailing "-" is a literal
// rather than a range. So the escape comes out and the hyphen must stay LAST —
// written anywhere else it would silently mean a character range (":_-a" would
// be nonsense, "_-a" would be a range) and quietly change what tokenizes.
var tokenPattern = regexp.MustCompile(`[a-z0-9][a-z0-9./:_-]*`)

// splitPattern breaks a compound token into its word parts.
var splitPattern = regexp.MustCompile(`[./:_-]+`)

// compoundChars is the same class as splitPattern, as a plain string: asking
// strings.ContainsAny is cheaper than running a regex on every token.
const compoundChars = "./:_-"

// Doc is one retrieved memory. Score is only set by Search — a stored document
// has no score of its own, only a score relative to some question.
type Doc struct {
	ID    int            `json:"id"`
	At    int64          `json:"at"`
	Text  string         `json:"text"`
	Meta  map[string]any `json:"meta"`
	Score float64        `json:"score,omitempty"`
}

// memDoc is the indexed form: the public fields plus the term-frequency bag
// that the similarity maths runs on. tf is written once at index time and never
// mutated again, which is what lets concurrent searches read it under a shared
// read lock.
type memDoc struct {
	id   int
	at   int64
	text string
	meta map[string]any
	tf   map[string]int
}

// persisted is one JSONL line. The tf bag is deliberately NOT stored: it is a
// pure function of the text, so recomputing it on load keeps the file small and
// means a tokenizer improvement applies retroactively to old memories.
type persisted struct {
	ID   int            `json:"id"`
	At   int64          `json:"at"`
	Text string         `json:"text"`
	Meta map[string]any `json:"meta"`
}

// Memory is the incident index: an append-only log of short text documents with
// a TF-IDF retrieval layer over it. All exported methods are safe to call from
// any number of goroutines.
type Memory struct {
	file    string       // "" disables persistence entirely
	now     func() int64 // injectable clock, so tests are not at the mercy of wall time
	maxDocs int

	mu     sync.RWMutex
	docs   []*memDoc
	df     map[string]int // token -> how many documents contain it, for IDF
	nextID int
}

// NewMemory opens (and, if it exists, replays) a memory file.
//
// An empty file path means memory-only: everything works, nothing survives a
// restart, which is the right default on an ephemeral host. A nil now means the
// wall clock in Unix milliseconds. maxDocs <= 0 means the house default.
//
// A missing or unreadable file is not an error — it is simply the first boot.
func NewMemory(file string, now func() int64, maxDocs int) *Memory {
	if now == nil {
		now = func() int64 { return time.Now().UnixMilli() }
	}
	if maxDocs <= 0 {
		maxDocs = defaultMaxDocs
	}
	m := &Memory{
		file:    file,
		now:     now,
		maxDocs: maxDocs,
		df:      map[string]int{},
		nextID:  1,
	}
	m.load()
	return m
}

// Remember indexes one document and returns its id.
//
// Stages 1 and 2 of the pipeline: the caller has already chunked the event into
// a sentence, and this embeds it as a weighted token bag. The returned id is
// stable and is what the chat UI cites.
//
// meta is copied on the way in, so a caller may reuse or mutate the map it
// passed without corrupting the index.
func (m *Memory) Remember(text string, meta map[string]any) int {
	doc := &memDoc{
		at:   m.now(),
		text: text,
		meta: copyMeta(meta),
		tf:   termCounts(text),
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	doc.id = m.nextID
	m.nextID++
	m.index(doc)

	// Persist inside the lock. It costs a syscall on a path that runs a handful
	// of times per incident, and it buys a file whose line order always matches
	// id order — which matters, because replay trusts that order.
	if m.file != "" {
		if line, err := json.Marshal(persisted{ID: doc.id, At: doc.at, Text: doc.text, Meta: doc.meta}); err == nil {
			m.append(line)
		}
	}
	return doc.id
}

// Search is stage 3: rank every document against the question by cosine
// similarity over TF-IDF weights, and return the best k.
//
// k <= 0 means the default. The result is always non-nil (an empty slice, not
// null, so the admin API renders []), every Doc is a copy, and Score is rounded
// to three decimals because further precision is noise a human should not be
// asked to read.
func (m *Memory) Search(query string, k int) []Doc {
	if k <= 0 {
		k = defaultTopK
	}
	qtf := termCounts(query)
	if len(qtf) == 0 {
		return []Doc{} // a question with no indexable words matches nothing
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	n := len(m.docs)
	if n == 0 {
		return []Doc{}
	}

	// The query vector: tf * idf per token, plus its length for the cosine.
	qw := make(map[string]float64, len(qtf))
	var qNorm float64
	for t, c := range qtf {
		w := float64(c) * m.idf(t, n)
		qw[t] = w
		qNorm += w * w
	}
	qNorm = math.Sqrt(qNorm)
	if qNorm == 0 {
		return []Doc{}
	}

	type hit struct {
		doc   *memDoc
		score float64
	}
	hits := make([]hit, 0, defaultTopK)
	for _, doc := range m.docs {
		var dot, dNorm float64
		for t, c := range doc.tf {
			w := float64(c) * m.idf(t, n)
			dNorm += w * w
			if q, shared := qw[t]; shared {
				dot += q * w // only tokens present in BOTH move the needle
			}
		}
		if dot > 0 && dNorm > 0 {
			hits = append(hits, hit{doc: doc, score: dot / (math.Sqrt(dNorm) * qNorm)})
		}
	}

	// Stable, so equally-scoring documents keep insertion order — oldest first,
	// which is what the JS produced and what makes repeated searches
	// reproducible rather than subtly shuffled.
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].score > hits[j].score })
	if k > len(hits) {
		k = len(hits)
	}

	out := make([]Doc, 0, k)
	for _, h := range hits[:k] {
		out = append(out, Doc{
			ID:    h.doc.id,
			At:    h.doc.at,
			Text:  h.doc.text,
			Meta:  copyMeta(h.doc.meta), // never hand out the indexed map itself
			Score: round3(h.score),
		})
	}
	return out
}

// Size reports how many documents are currently indexed.
func (m *Memory) Size() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.docs)
}

// ---- indexing internals -----------------------------------------------------

// termCounts turns text into a term-frequency bag.
//
// Each token is counted twice over: once in its exact compound form, so
// "visualise.vercel.app" is a precise, very rare, very heavy hit; and once per
// word part, so the question "visualise quota?" still finds a document that
// only ever wrote the compound. Single-character parts are dropped — they carry
// no signal and would only inflate the norms.
func termCounts(text string) map[string]int {
	tf := map[string]int{}
	for _, t := range tokenPattern.FindAllString(strings.ToLower(text), -1) {
		tf[t]++
		if strings.ContainsAny(t, compoundChars) {
			for _, part := range splitPattern.Split(t, -1) {
				if len(part) > 1 {
					tf[part]++
				}
			}
		}
	}
	return tf
}

// index adds a document and evicts the oldest if the index is now over budget.
//
// The caller must hold m.mu for writing, or own m outright (as NewMemory does
// during replay, before the value has escaped).
func (m *Memory) index(doc *memDoc) {
	m.docs = append(m.docs, doc)
	for token := range doc.tf {
		m.df[token]++
	}
	if len(m.docs) <= m.maxDocs {
		return
	}

	// Forget the oldest — and UN-COUNT its tokens. This is the step that is easy
	// to miss: leave df alone and every evicted document keeps voting on how
	// rare its words are forever, so IDF drifts and rare terms slowly stop
	// looking rare. The index would still return results; they would just get
	// quietly worse.
	old := m.docs[0]
	copy(m.docs, m.docs[1:]) // shift down so the backing array cannot grow forever
	m.docs[len(m.docs)-1] = nil
	m.docs = m.docs[:len(m.docs)-1]

	for token := range old.tf {
		if n := m.df[token] - 1; n > 0 {
			m.df[token] = n
		} else {
			delete(m.df, token) // gone entirely; do not leave a zero behind
		}
	}
}

// idf weighs a token by how rare it is: ln(1 + N/(1+df)). Unseen tokens score
// high, ubiquitous ones tend to zero, and the +1s keep it defined for a token
// no document contains. n is passed in because the caller already holds the
// lock and knows the corpus size.
func (m *Memory) idf(token string, n int) float64 {
	return math.Log(1 + float64(n)/float64(1+m.df[token]))
}

// ---- persistence: the humblest possible database ----------------------------

// append writes one JSONL record. Persistence is best-effort by design: the
// in-memory index is the source of truth for a running process, and a full disk
// must degrade the gateway's memory across restarts, not fail the request that
// was being served. The caller holds m.mu.
func (m *Memory) append(line []byte) {
	f, err := os.OpenFile(m.file, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(line, '\n'))
}

// load replays yesterday's memory on boot. It runs inside NewMemory before the
// Memory value escapes, so it needs no locking.
//
// Deliberate deviation from the JS: there, one corrupt line threw and silently
// discarded every record after it. Here a bad line is skipped and replay
// continues, because a half-written final line (the classic result of a crash
// mid-append) should cost one memory, not all of them.
func (m *Memory) load() {
	if m.file == "" {
		return
	}
	f, err := os.Open(m.file)
	if err != nil {
		return // no memory file yet — first boot
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), maxLineBytes)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var p persisted
		if err := json.Unmarshal([]byte(line), &p); err != nil {
			continue
		}
		m.index(&memDoc{
			id:   p.ID,
			at:   p.At,
			text: p.Text,
			meta: copyMeta(p.Meta),
			tf:   termCounts(p.Text),
		})
		if p.ID >= m.nextID {
			m.nextID = p.ID + 1 // ids continue where the file left off
		}
	}
}

// ---- small helpers ----------------------------------------------------------

// copyMeta shallow-copies a metadata map, and turns nil into an empty map so
// stored records and API responses read {} rather than null — the shape the
// Node build produced. Values are treated as immutable by convention; the
// gateway only ever stores strings and numbers in here.
func copyMeta(meta map[string]any) map[string]any {
	out := make(map[string]any, len(meta))
	for k, v := range meta {
		out[k] = v
	}
	return out
}

// round3 trims a similarity score to three decimals.
func round3(v float64) float64 { return math.Round(v*1000) / 1000 }
