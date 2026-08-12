// Package demo is a STAND-IN for the expensive AI model the gateway protects.
//
// LimitPlane's job is guarding costly AI endpoints, not being one. So the demo
// needs something that LOOKS like an inference endpoint — a JSON label plus a
// confidence, exactly the shape the NSFW Website Detection problem statement
// asks for — without shipping a model, a GPU, or a per-call bill. This is a
// deterministic keyword scorer: same input, same output, every time, which is
// also what makes it usable in tests where a real model never would be.
//
// The point is the SEAM, not the classifier. Swapping in a real model later
// means replacing one function; the gateway never learns the difference,
// because all it knows about this route is that policy marks it "heavy" and a
// heavy call spends five tokens from the caller's bucket instead of one.
package demo

import (
	"math"
	"strings"
)

// flaggedTerms are the demo trigger words. Each DISTINCT term found in the
// text counts once, however many times it appears — the stub scores breadth of
// evidence, not repetition.
var flaggedTerms = []string{"nsfw", "explicit", "adult-only", "xxx"}

// modelName is reported on every verdict so a response is always honest about
// what produced it. A real model would put its own version here.
const modelName = "stub-keyword-v0"

// Verdict is one classification result: the call, the certainty, and the
// provenance.
type Verdict struct {
	// Label is "nsfw" when confidence reaches 0.5, otherwise "safe".
	Label string `json:"label"`
	// Confidence is 0..0.98, rounded to two decimal places.
	Confidence float64 `json:"confidence"`
	// Model names whatever produced this verdict.
	Model string `json:"model"`
}

// ClassifyText scores text for flagged terms and returns the verdict.
//
// Scoring: no hits scores 0.02 (basically clean), each distinct flagged term
// adds 0.4, and the total is capped at 0.98 — a real model is never fully
// certain, and the stub mimics that honesty rather than claiming 1.0. The
// label is decided from that score before it is rounded for display.
//
// Matching is case-insensitive and substring-based, so "XXX" and "the xxx
// site" both count, which is deliberately crude: this is a demo payload, not
// a moderation system.
func ClassifyText(text string) Verdict {
	lower := strings.ToLower(text)

	hits := 0
	for _, term := range flaggedTerms {
		if strings.Contains(lower, term) {
			hits++
		}
	}

	confidence := math.Min(0.02+float64(hits)*0.4, 0.98)

	label := "safe"
	if confidence >= 0.5 {
		label = "nsfw"
	}

	return Verdict{
		Label: label,
		// Round for the wire the way the JS Number(x.toFixed(2)) did: float
		// arithmetic leaves values like 0.42000000000000004, and nobody wants
		// that in an API response.
		Confidence: math.Round(confidence*100) / 100,
		Model:      modelName,
	}
}
