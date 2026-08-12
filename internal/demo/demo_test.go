package demo

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

func TestClassifyText(t *testing.T) {
	tests := []struct {
		name           string
		text           string
		wantLabel      string
		wantConfidence float64
	}{
		{
			name:           "empty text",
			text:           "",
			wantLabel:      "safe",
			wantConfidence: 0.02,
		},
		{
			name:           "no flagged terms",
			text:           "a perfectly ordinary sentence about rate limiting",
			wantLabel:      "safe",
			wantConfidence: 0.02,
		},
		{
			// 0.02 + 0.4 = 0.42, still under the 0.5 threshold: one flagged
			// word is suspicion, not a verdict.
			name:           "one hit is suspicious but still safe",
			text:           "this page is nsfw",
			wantLabel:      "safe",
			wantConfidence: 0.42,
		},
		{
			// 0.02 + 0.8 = 0.82: the second hit is what crosses 0.5.
			name:           "two hits cross the threshold",
			text:           "nsfw and explicit material",
			wantLabel:      "nsfw",
			wantConfidence: 0.82,
		},
		{
			name:           "three hits reach the cap",
			text:           "nsfw explicit adult-only",
			wantLabel:      "nsfw",
			wantConfidence: 0.98,
		},
		{
			name:           "all four hits stay capped at 0.98",
			text:           "nsfw explicit adult-only xxx",
			wantLabel:      "nsfw",
			wantConfidence: 0.98,
		},
		{
			name:           "matching is case insensitive",
			text:           "NSFW",
			wantLabel:      "safe",
			wantConfidence: 0.42,
		},
		{
			name:           "case insensitive across two terms",
			text:           "NSFW and EXPLICIT",
			wantLabel:      "nsfw",
			wantConfidence: 0.82,
		},
		{
			name:           "mixed case counts every distinct term",
			text:           "XXX and Explicit and Adult-Only",
			wantLabel:      "nsfw",
			wantConfidence: 0.98,
		},
		{
			name:           "repeats of one term still count once",
			text:           "xxx xxx xxx xxx xxx",
			wantLabel:      "safe",
			wantConfidence: 0.42,
		},
		{
			name:           "substring matches inside a word",
			text:           "https://example.com/xxx-archive",
			wantLabel:      "safe",
			wantConfidence: 0.42,
		},
		{
			name:           "hyphenated term must match in full",
			text:           "adult content, nothing else",
			wantLabel:      "safe",
			wantConfidence: 0.02,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyText(tc.text)

			if got.Label != tc.wantLabel {
				t.Errorf("Label = %q, want %q", got.Label, tc.wantLabel)
			}
			if got.Confidence != tc.wantConfidence {
				t.Errorf("Confidence = %v, want %v", got.Confidence, tc.wantConfidence)
			}
			if got.Model != "stub-keyword-v0" {
				t.Errorf("Model = %q, want %q", got.Model, "stub-keyword-v0")
			}
		})
	}
}

// The label follows from the confidence, and the cap is never exceeded no
// matter how much is thrown at it.
func TestClassifyTextInvariants(t *testing.T) {
	inputs := []string{
		"",
		"clean",
		"nsfw",
		"nsfw explicit",
		"nsfw explicit adult-only xxx " + strings.Repeat("xxx ", 100),
	}
	for _, in := range inputs {
		v := ClassifyText(in)
		if v.Confidence < 0.02 || v.Confidence > 0.98 {
			t.Errorf("ClassifyText(%q).Confidence = %v, outside [0.02, 0.98]", in, v.Confidence)
		}
		wantLabel := "safe"
		if v.Confidence >= 0.5 {
			wantLabel = "nsfw"
		}
		if v.Label != wantLabel {
			t.Errorf("ClassifyText(%q) = %q at confidence %v", in, v.Label, v.Confidence)
		}
	}
}

// Float arithmetic leaves 0.02+0.4 as 0.42000000000000004; the JS original
// used Number(x.toFixed(2)) and this must land on the same two decimals.
func TestClassifyTextRoundsToTwoDecimals(t *testing.T) {
	for _, text := range []string{"", "nsfw", "nsfw explicit", "nsfw explicit xxx"} {
		c := ClassifyText(text).Confidence
		if rounded := math.Round(c*100) / 100; c != rounded {
			t.Errorf("ClassifyText(%q).Confidence = %v, not rounded to 2dp", text, c)
		}
	}
}

// The verdict is an API response before it is anything else, so the JSON keys
// matter as much as the values.
func TestVerdictJSONShape(t *testing.T) {
	b, err := json.Marshal(ClassifyText("this is nsfw and explicit"))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	const want = `{"label":"nsfw","confidence":0.82,"model":"stub-keyword-v0"}`
	if string(b) != want {
		t.Fatalf("JSON = %s, want %s", b, want)
	}
}

// Deterministic by construction: the same input must never produce a
// different verdict, which is what makes this usable as a test fixture.
func TestClassifyTextIsDeterministic(t *testing.T) {
	first := ClassifyText("some explicit text")
	for i := 0; i < 100; i++ {
		if got := ClassifyText("some explicit text"); got != first {
			t.Fatalf("run %d = %+v, first = %+v", i, got, first)
		}
	}
}
