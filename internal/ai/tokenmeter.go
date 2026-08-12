// tokenMeter — the piece that makes "AI-aware" literal.
//
// Elsewhere in the gateway "heavy = 5 units" is a static price tag: a guess,
// written down once, that never learns. The real cost driver of an AI endpoint
// is TOKENS, and tokens are only knowable AFTER the model has answered. This
// proxies a real inference call, reads the true usage back off the response,
// and charges the tenant's monthly meter in real units and real dollars — so a
// plan can be a hard monthly DOLLAR cap rather than a request count.
//
//	client -> LimitPlane /v1/ai/proxy -> Groq -> usage.total_tokens
//	                                          -> units = ceil(tokens / UnitTokens)
//	                                          -> $     = in*inRate + out*outRate
//
// The lesson lives in that ordering. Cost is unknown at admission time, so the
// gateway pre-authorises a small hold, makes the call, then reconciles the
// meter against the true token count — the same trick a fuel pump plays on your
// card. Without an API key the meter still runs, on a deterministic estimate,
// so the demo and the tests never need a credential.

package ai

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"time"
)

const (
	// UnitTokens is the exchange rate: one LimitPlane unit buys 100 tokens.
	// Billing in units rather than raw tokens keeps a single scale across
	// endpoints that meter requests and endpoints that meter inference.
	UnitTokens = 100

	// Groq llama-3.3-70b public pricing, USD per 1M tokens (Jul 2026). Output
	// costs more than input, which is why the two are metered separately
	// instead of being folded into one total.
	pricePerMInput  = 0.59
	pricePerMOutput = 0.79

	defaultMaxTokens = 256  // what the JS defaulted to
	maxPromptChars   = 4000 // truncation guard: nobody proxies a novel through a demo
	simulatedOutput  = 80   // the offline estimate's stand-in completion length

	// completeTimeout is generous because this one sits ON the request path and
	// a real completion can genuinely take a while — but it is still finite,
	// which is the whole point.
	completeTimeout = 60 * time.Second

	simulatedText = "(token meter demo — set GROQ_API_KEY for real inference)"
)

// Usage is the true token accounting for one completion, as the provider
// reported it.
type Usage struct {
	InTokens    int `json:"inTokens"`
	OutTokens   int `json:"outTokens"`
	TotalTokens int `json:"totalTokens"`
}

// Completion is one model answer plus what it cost. Units is what the monthly
// meter is charged; USD is what it cost us, rounded to the sixth decimal
// because a single cheap call lands in the tens of microdollars and printing
// float noise past that helps nobody.
type Completion struct {
	Text      string  `json:"text"`
	Usage     Usage   `json:"usage"`
	Units     float64 `json:"units"`
	USD       float64 `json:"usd"`
	Simulated bool    `json:"simulated,omitempty"`
}

// EstimateUnits converts a token count into billable units, rounding up and
// never charging zero. A one-token request still costs something, because the
// call itself is what the gateway is rationing.
func EstimateUnits(tokens int) float64 {
	return math.Max(1, math.Ceil(float64(tokens)/UnitTokens))
}

// DollarsFor prices a completion at list rates. Input and output tokens are
// charged separately; the result is exact dollars, unrounded, so callers can
// sum many calls before rounding once.
func DollarsFor(inTokens, outTokens int) float64 {
	return (float64(inTokens)*pricePerMInput + float64(outTokens)*pricePerMOutput) / 1e6
}

// TokenMeter proxies inference and reports what it cost.
//
// Immutable after construction, so it is safe to share across goroutines; the
// HTTP client it holds is concurrency-safe by contract.
type TokenMeter struct {
	apiKey string // empty = offline estimate mode
	model  string
	hc     *http.Client
}

// NewTokenMeter builds a meter. An empty apiKey selects the deterministic
// offline estimate rather than an error, so the billing path can be demoed and
// tested with no credential at all. An empty model means the house default; a
// nil client gets one with a timeout.
func NewTokenMeter(apiKey, model string, hc *http.Client) *TokenMeter {
	if model == "" {
		model = defaultModel
	}
	if hc == nil {
		hc = &http.Client{Timeout: completeTimeout}
	}
	return &TokenMeter{apiKey: apiKey, model: model, hc: hc}
}

// Live reports whether completions are real inference or the offline estimate.
func (t *TokenMeter) Live() bool { return t.apiKey != "" }

// Complete asks the model and returns the answer together with its true cost.
//
// maxTokens <= 0 means the default ceiling. Offline it returns a deterministic
// estimate with Simulated set, and no error — the meter still moves, which is
// what a demo needs. Live, unlike Explain, failures are REPORTED: a caller that
// is about to bill somebody must never be handed a made-up number, so a network
// error, a non-200 or an undecodable body all come back as an error and the
// meter is left untouched.
func (t *TokenMeter) Complete(prompt string, maxTokens int) (Completion, error) {
	if maxTokens <= 0 {
		maxTokens = defaultMaxTokens
	}
	if !t.Live() {
		return simulate(prompt, maxTokens), nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), completeTimeout)
	defer cancel()

	resp, err := postChat(ctx, t.hc, t.apiKey, completionRequest{
		Model:     t.model,
		MaxTokens: maxTokens,
		Messages:  []chatMessage{{Role: "user", Content: truncateChars(prompt, maxPromptChars)}},
	})
	if err != nil {
		return Completion{}, fmt.Errorf("token meter: %w", err)
	}

	// A provider that answers without a usage block gets billed as zero rather
	// than crashing the request; that is strictly better for the customer and
	// visible in the response either way.
	var in, out int
	total := 0
	if resp.Usage != nil {
		in, out = resp.Usage.PromptTokens, resp.Usage.CompletionTokens
		if resp.Usage.TotalTokens != nil {
			total = *resp.Usage.TotalTokens
		} else {
			total = in + out
		}
	}

	return Completion{
		Text:  firstContent(resp),
		Usage: Usage{InTokens: in, OutTokens: out, TotalTokens: total},
		Units: EstimateUnits(total),
		USD:   round6(DollarsFor(in, out)),
	}, nil
}

// simulate is the no-key estimate: four characters to a token is the usual
// rule of thumb for English, and a fixed short completion stands in for the
// answer. It is deterministic on purpose — the same prompt always costs the
// same, so the billing UI can be demoed and asserted on.
func simulate(prompt string, maxTokens int) Completion {
	in := int(math.Ceil(float64(len(prompt)) / 4))
	out := maxTokens
	if out > simulatedOutput {
		out = simulatedOutput
	}
	total := in + out
	return Completion{
		Text:      simulatedText,
		Usage:     Usage{InTokens: in, OutTokens: out, TotalTokens: total},
		Units:     EstimateUnits(total),
		USD:       round6(DollarsFor(in, out)),
		Simulated: true,
	}
}

type completionRequest struct {
	Model     string        `json:"model"`
	MaxTokens int           `json:"max_tokens"`
	Messages  []chatMessage `json:"messages"`
}

// round6 snaps a dollar figure to the sixth decimal — one microdollar, which is
// finer than the cheapest possible call.
func round6(v float64) float64 { return math.Round(v*1e6) / 1e6 }

// truncateChars caps a string at max CHARACTERS, not bytes. Cutting on a byte
// boundary could split a multi-byte rune and ship invalid UTF-8 to the
// provider; the fast path avoids decoding at all for the common ASCII case.
func truncateChars(s string, max int) string {
	if len(s) <= max { // bytes <= max implies runes <= max
		return s
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}
