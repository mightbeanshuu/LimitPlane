package metrics

import (
	"math"
	"strings"
	"testing"
	"time"
)

func TestRenderShapeIsValidExposition(t *testing.T) {
	r := New()
	r.Counter("limitplane_checks_total", "Admission decisions.").
		Set(7, "decision", "allowed").
		Set(3, "decision", "blocked")

	got := string(r.Render())
	want := "# HELP limitplane_checks_total Admission decisions.\n" +
		"# TYPE limitplane_checks_total counter\n" +
		`limitplane_checks_total{decision="allowed"} 7` + "\n" +
		`limitplane_checks_total{decision="blocked"} 3` + "\n"
	if got != want {
		t.Fatalf("exposition output mismatch\n got: %q\nwant: %q", got, want)
	}
}

// A tenant ID is user-supplied, so it can contain the characters the format
// reserves. Emitting them raw produces a line Prometheus cannot parse, and a
// single bad line makes the scraper drop the WHOLE scrape — every metric, not
// just this one. That turns a cosmetic input into a monitoring outage, which is
// why this test exists.
func TestLabelValuesAreEscaped(t *testing.T) {
	r := New()
	r.Gauge("limitplane_tenant_lane", "lane").
		Set(1, "tenant", `evil" injected="yes`+"\n"+`back\slash`)

	got := string(r.Render())
	if strings.Contains(got, `injected="yes`) && !strings.Contains(got, `\"`) {
		t.Fatalf("quote was not escaped, the line is injectable:\n%s", got)
	}
	for _, forbidden := range []string{"\n\\slash", `evil" `} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("unescaped %q survived into the output:\n%s", forbidden, got)
		}
	}
	if want := `\"`; !strings.Contains(got, want) {
		t.Fatalf("expected an escaped quote in output:\n%s", got)
	}
	if want := `\\`; !strings.Contains(got, want) {
		t.Fatalf("expected an escaped backslash in output:\n%s", got)
	}
	// Exactly one sample line means the newline did not split the record.
	lines := 0
	for _, l := range strings.Split(strings.TrimSpace(got), "\n") {
		if !strings.HasPrefix(l, "#") {
			lines++
		}
	}
	if lines != 1 {
		t.Fatalf("a newline in a label value split the record into %d lines:\n%s", lines, got)
	}
}

// Registering one name as two types produces output a scraper rejects outright.
// The registry returns the existing family instead, so it cannot happen.
func TestDuplicateFamilyNameKeepsOneType(t *testing.T) {
	r := New()
	r.Counter("limitplane_thing", "first").Set(1)
	r.Gauge("limitplane_thing", "second").Set(2)

	got := string(r.Render())
	if strings.Count(got, "# TYPE limitplane_thing") != 1 {
		t.Fatalf("expected exactly one TYPE line:\n%s", got)
	}
	if strings.Contains(got, "gauge") {
		t.Fatalf("the second registration changed the type:\n%s", got)
	}
	if !strings.Contains(got, "limitplane_thing 1\n") || !strings.Contains(got, "limitplane_thing 2\n") {
		t.Fatalf("both samples should still be present:\n%s", got)
	}
}

func TestEmptyFamilyIsOmitted(t *testing.T) {
	r := New()
	r.Gauge("limitplane_never_set", "nothing here")
	if got := string(r.Render()); got != "" {
		t.Fatalf("a family with no samples should render nothing, got:\n%s", got)
	}
}

func TestOddLabelCountIsDropped(t *testing.T) {
	r := New()
	// A malformed line poisons the whole scrape, so a caller mistake must not
	// reach the output at all.
	r.Gauge("limitplane_thing", "help").Set(1, "tenant")
	if got := string(r.Render()); got != "" {
		t.Fatalf("an odd label list should be dropped, got:\n%s", got)
	}
}

func TestLabelsAreSortedForStableOutput(t *testing.T) {
	r := New()
	r.Gauge("g", "h").Set(1, "zebra", "z", "alpha", "a", "middle", "m")
	got := string(r.Render())
	if !strings.Contains(got, `g{alpha="a",middle="m",zebra="z"} 1`) {
		t.Fatalf("labels were not sorted:\n%s", got)
	}
}

func TestSetBoolUsesNumbers(t *testing.T) {
	r := New()
	r.Gauge("limitplane_operator_is_leader", "leader").SetBool(true)
	r.Gauge("other", "x").SetBool(false)
	got := string(r.Render())
	if !strings.Contains(got, "limitplane_operator_is_leader 1\n") {
		t.Fatalf("true should render as 1:\n%s", got)
	}
	if !strings.Contains(got, "other 0\n") {
		t.Fatalf("false should render as 0:\n%s", got)
	}
}

func TestSanitizeName(t *testing.T) {
	cases := map[string]string{
		"tenant":      "tenant",
		"cost-class":  "cost_class",
		"9lives":      "_9lives",
		"a.b/c":       "a_b_c",
		"":            "_",
		"already_ok:": "already_ok:",
	}
	for in, want := range cases {
		if got := sanitizeName(in); got != want {
			t.Errorf("sanitizeName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFormatValueSpecials(t *testing.T) {
	if got := formatValue(math.NaN()); got != "NaN" {
		t.Errorf("NaN rendered as %q", got)
	}
	if got := formatValue(math.Inf(1)); got != "+Inf" {
		t.Errorf("+Inf rendered as %q", got)
	}
	if got := formatValue(math.Inf(-1)); got != "-Inf" {
		t.Errorf("-Inf rendered as %q", got)
	}
	if got := formatValue(0.5); got != "0.5" {
		t.Errorf("0.5 rendered as %q", got)
	}
}

func TestRuntimeEmitsBuildInfo(t *testing.T) {
	r := New()
	r.Runtime(time.Unix(1700000000, 0), "v1.2.3", "abc1234")
	got := string(r.Render())
	for _, want := range []string{
		`limitplane_build_info{`,
		`commit="abc1234"`,
		`version="v1.2.3"`,
		"go_goroutines",
		"process_start_time_seconds 1.7e+09",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in runtime metrics:\n%s", want, got)
		}
	}
}
