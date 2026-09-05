// Package metrics renders the Prometheus text exposition format.
//
// WHY HAND-WRITE THIS? Because the official client library is a dependency, and
// what it does for a gateway this size is not complicated: the exposition
// format is line-oriented UTF-8 text with three rules that matter (a HELP line,
// a TYPE line, then samples), and getting it exactly right is about a hundred
// lines. The parts people actually get wrong are escaping and duplicate
// families, so both are enforced here rather than left to care.
//
// The output is what makes LimitPlane legible to the rest of a Kubernetes
// cluster: Prometheus scrapes it, a HorizontalPodAutoscaler can scale on it via
// the Prometheus adapter or KEDA, Grafana draws it, and Alertmanager pages on
// it. A rate limiter nobody can graph is a rate limiter nobody trusts.
package metrics

import (
	"bytes"
	"fmt"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ContentType is what a Prometheus scraper expects. Version 0.0.4 is the
// classic text format every scraper still understands.
const ContentType = "text/plain; version=0.0.4; charset=utf-8"

// Registry accumulates metric families and renders them once.
type Registry struct {
	families []*Family
	seen     map[string]*Family
}

func New() *Registry {
	return &Registry{seen: map[string]*Family{}}
}

// Family is one metric name plus all of its labelled samples.
type Family struct {
	name    string
	help    string
	typ     string
	samples []sample
}

type sample struct {
	labels []string // flat key, value, key, value...
	value  float64
}

func (r *Registry) family(name, help, typ string) *Family {
	// Registering the same name twice with different types produces output
	// Prometheus rejects outright — the whole scrape is dropped, not just the
	// bad family. Returning the existing one makes that impossible to do by
	// accident from two different call sites.
	if f, ok := r.seen[name]; ok {
		return f
	}
	f := &Family{name: name, help: help, typ: typ}
	r.seen[name] = f
	r.families = append(r.families, f)
	return f
}

// Counter is a value that only ever goes up (and resets to zero on restart).
// Use it for totals: requests, errors, reconciles.
func (r *Registry) Counter(name, help string) *Family { return r.family(name, help, "counter") }

// Gauge is a value that moves in both directions: current tokens, replicas,
// bytes in use.
func (r *Registry) Gauge(name, help string) *Family { return r.family(name, help, "gauge") }

// Set records one sample. Labels are flat key/value pairs:
//
//	f.Set(12, "tenant", "acme", "decision", "blocked")
//
// An odd number of label arguments is a programming error and is dropped rather
// than emitted, because a malformed line poisons the entire scrape.
func (f *Family) Set(value float64, labels ...string) *Family {
	if len(labels)%2 != 0 {
		return f
	}
	f.samples = append(f.samples, sample{labels: labels, value: value})
	return f
}

// SetBool is the idiomatic Prometheus encoding of a boolean: 1 or 0. There is
// no boolean type in the format, and inventing one ("true") makes the series
// unusable in arithmetic — `sum(limitplane_operator_is_leader) != 1` is the
// alert that catches a split brain, and it needs a number.
func (f *Family) SetBool(b bool, labels ...string) *Family {
	v := 0.0
	if b {
		v = 1
	}
	return f.Set(v, labels...)
}

// Render writes the whole registry in exposition format.
func (r *Registry) Render() []byte {
	var b bytes.Buffer
	for _, f := range r.families {
		if len(f.samples) == 0 {
			continue // a family with no samples is noise, not information
		}
		fmt.Fprintf(&b, "# HELP %s %s\n", f.name, escapeHelp(f.help))
		fmt.Fprintf(&b, "# TYPE %s %s\n", f.name, f.typ)
		for _, s := range f.samples {
			b.WriteString(f.name)
			writeLabels(&b, s.labels)
			b.WriteByte(' ')
			b.WriteString(formatValue(s.value))
			b.WriteByte('\n')
		}
	}
	return b.Bytes()
}

func writeLabels(b *bytes.Buffer, labels []string) {
	if len(labels) == 0 {
		return
	}
	// Sort by label name so a series renders identically every scrape. Prometheus
	// does not require it, but stable output makes diffs and golden tests
	// meaningful, and costs nothing at this cardinality.
	type kv struct{ k, v string }
	pairs := make([]kv, 0, len(labels)/2)
	for i := 0; i+1 < len(labels); i += 2 {
		if labels[i] == "" {
			continue
		}
		pairs = append(pairs, kv{sanitizeName(labels[i]), labels[i+1]})
	}
	if len(pairs) == 0 {
		return
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].k < pairs[j].k })

	b.WriteByte('{')
	for i, p := range pairs {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(p.k)
		b.WriteString(`="`)
		b.WriteString(escapeLabelValue(p.v))
		b.WriteByte('"')
	}
	b.WriteByte('}')
}

// escapeLabelValue escapes the three characters the format reserves.
//
// This is not pedantry — label values here include tenant IDs, which are
// user-supplied. A tenant that named itself with a quote character would
// otherwise emit a broken line and break the ENTIRE scrape for every metric,
// turning a cosmetic input into a monitoring outage. Untrusted input reaching
// a text format is the same class of bug as SQL injection, and gets the same
// treatment: escape at the boundary.
func escapeLabelValue(s string) string {
	if !strings.ContainsAny(s, `\"`+"\n") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 8)
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// escapeHelp escapes the two characters that are special in a HELP line.
func escapeHelp(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	return strings.ReplaceAll(s, "\n", `\n`)
}

// sanitizeName forces a string into a valid metric or label name. Anything
// outside [a-zA-Z0-9_:] becomes an underscore, and a leading digit gets a
// prefix, because a name that starts with a digit is not parseable.
func sanitizeName(s string) string {
	if s == "" {
		return "_"
	}
	var b strings.Builder
	b.Grow(len(s))
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '_', r == ':':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			if i == 0 {
				b.WriteByte('_')
			}
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// formatValue renders a float the way Prometheus expects. The three special
// values have literal spellings and would otherwise come out as Go's "NaN" /
// "+Inf" spelled differently enough to be rejected.
func formatValue(v float64) string {
	switch {
	case v != v:
		return "NaN"
	case v > 1.7976931348623157e308:
		return "+Inf"
	case v < -1.7976931348623157e308:
		return "-Inf"
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// Runtime adds the standard Go process metrics.
//
// These are the ones a Kubernetes operator is actually asked about during an
// incident: goroutine count (a leak shows as a straight climbing line),
// heap size, and process start time (which is how every dashboard computes
// uptime and how you spot a CrashLoopBackOff that restarts too fast to notice).
func (r *Registry) Runtime(started time.Time, version, commit string) {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)

	r.Gauge("go_goroutines", "Number of goroutines that currently exist.").
		Set(float64(runtime.NumGoroutine()))
	r.Gauge("go_memstats_alloc_bytes", "Number of bytes allocated in heap objects.").
		Set(float64(m.Alloc))
	r.Gauge("go_memstats_sys_bytes", "Total bytes of memory obtained from the OS.").
		Set(float64(m.Sys))
	r.Gauge("go_memstats_heap_objects", "Number of allocated objects.").
		Set(float64(m.HeapObjects))
	r.Counter("go_memstats_gc_cycles_total", "Number of completed GC cycles.").
		Set(float64(m.NumGC))
	r.Gauge("go_max_procs", "Maximum number of CPUs that can execute simultaneously.").
		Set(float64(runtime.GOMAXPROCS(0)))
	r.Gauge("process_start_time_seconds", "Start time of the process since the Unix epoch, in seconds.").
		Set(float64(started.Unix()))
	// go_info and build_info are the conventional way to expose strings:
	// a constant 1 carrying the interesting data in labels, so a query can join
	// on version without version ever being a value.
	r.Gauge("go_info", "Information about the Go environment.").
		Set(1, "version", runtime.Version())
	r.Gauge("limitplane_build_info", "Build information; the value is always 1.").
		Set(1, "version", version, "commit", commit, "goversion", runtime.Version())
}
