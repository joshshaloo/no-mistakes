package convergence

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Question ids. They are for this code only; the service never sends them to
// the model, so the full meaning lives in the instructions and criteria.
const (
	questionNonConvergence = "non_convergence"
	questionCausalThemes   = "causal_themes"
)

// nonConvergenceInstructions is the one narrow judgment this detector exists
// for. The wording is load-bearing and was tuned against real multi-round runs:
// the "repaired only the exact site" sentence is what separates a point patch
// whose cause is still reachable from an independent follow-up defect, and the
// explicit "merely share a file, package, subsystem" exclusion in the false
// criterion is what stops several independent gaps in one subsystem from
// reading as non-convergence.
const nonConvergenceInstructions = "Look at `earlier_rounds` and `current_round` for one run of an automated review-and-fix pipeline. " +
	"Have the fixes already applied on this run failed to settle the underlying cause behind the current finding, " +
	"so that fixing again would prop up a design that keeps producing defects rather than close an independent defect? " +
	"Judge convergence, not similarity. " +
	"A fix that repaired only the exact site it was reported at, while leaving the same cause reachable elsewhere, has NOT settled that cause."

const nonConvergenceTrue = "An earlier fix on this run was meant to settle the cause behind the current finding and did not. Any of: " +
	"an invariant an earlier fix was meant to establish is violated again; " +
	"the same class of defect reappears at a sibling site the same cause reaches, after an earlier round patched one site of it; " +
	"or the previous fix on that cause is what produced the current finding. The run is patching a design."

const nonConvergenceFalse = "The current finding is an independent gap that the earlier fixes were never about, and those earlier fixes held. " +
	"Findings that merely share a file, package, subsystem, or general topic with earlier rounds are FALSE " +
	"unless one of the causal links above is actually present."

const causalThemesInstructions = "How many distinct underlying causes has this run accumulated across all of its rounds so far? " +
	"Count causes, not findings: several findings that trace back to one cause count once."

var causalThemeLevels = []string{
	"One cause: every finding on this run traces back to a single underlying cause",
	"Two or three distinct causes",
	"Four or more distinct causes, or the causes are too scattered to name",
}

// State budget. The service allows far more, but a compact state is cheaper and
// judged more reliably, and this one only ever carries findings and one-line fix
// summaries. Long bodies are truncated down through descriptionLimits rather
// than dropping earlier rounds: the earlier rounds are the whole point of the
// question, so they are the last thing to go - and they never go.
const maxStateBytes = 48000

var descriptionLimits = []int{600, 400, 240, 120}

const maxIntentChars = 2000

// Detector asks one narrow question over a run's own round history. A nil
// *Detector is valid and silently answers nothing, which is how "not
// configured" is represented everywhere downstream.
type Detector struct {
	endpoint string
	key      string
	model    string
	timeout  time.Duration
	client   *http.Client
	now      func() time.Time
}

// New returns a Detector, or nil when the detector is not usable on this host:
// not enabled, no resolvable key, or no endpoint. Returning nil rather than an
// error is deliberate - "not configured" is the default state and must not read
// as a failure anywhere up the stack.
func New(s Settings) *Detector {
	if !s.Enabled {
		return nil
	}
	key := resolveKey(s.KeyFile)
	if key == "" {
		return nil
	}
	baseURL := strings.TrimSpace(s.BaseURL)
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	model := strings.TrimSpace(s.Model)
	if model == "" {
		model = DefaultModel
	}
	timeout := s.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	return &Detector{
		endpoint: strings.TrimRight(baseURL, "/") + "/v1/systemone",
		key:      key,
		model:    model,
		timeout:  timeout,
		client:   &http.Client{Timeout: timeout},
		now:      time.Now,
	}
}

// SetHTTPClient replaces the HTTP client. Tests use it to serve a local
// httptest server; production never calls it.
func (d *Detector) SetHTTPClient(c *http.Client) {
	if d != nil && c != nil {
		d.client = c
	}
}

// Observe asks the non-convergence question over obs and returns the signal.
//
// It returns (nil, nil) for every reason the question should not be asked at
// all - a nil detector, a run with no earlier rounds, or a current round with
// no findings - and (nil, err) when the call was attempted and did not produce
// a usable answer. Callers record a non-nil signal and otherwise do nothing:
// there is no default value and no fallback retry.
func (d *Detector) Observe(ctx context.Context, obs Observation) (*Signal, error) {
	if d == nil {
		return nil, nil
	}
	// Beyond the first round only: with nothing applied yet there is no
	// "fixes already applied" to judge, and a one-round run must emit nothing.
	if len(obs.Earlier) == 0 || obs.Current.Number <= 1 {
		return nil, nil
	}
	// Nothing to ask about the cause of.
	if len(obs.Current.Findings) == 0 {
		return nil, nil
	}

	obs = redactObservation(obs, d.key)
	state, err := buildState(obs)
	if err != nil {
		return nil, err
	}

	// This sends only an operator's own code-review text about their own repositories,
	// is off unless they configure a key, and must never be used for tenant data,
	// which may not leave the tenant account.
	// One request carries both questions: they are independent judgments over
	// the same state, so the Score costs only its own tokens and cannot delay
	// the Noul. An absent Score never suppresses the Noul.
	payload, err := json.Marshal(request{
		Model: d.model,
		State: json.RawMessage(state),
		Questions: map[string]question{
			questionNonConvergence: {
				Type:         "noul",
				Instructions: nonConvergenceInstructions,
				Criteria: noulCriteria{
					True:  nonConvergenceTrue,
					False: nonConvergenceFalse,
				},
			},
			questionCausalThemes: {
				Type:         "score",
				Instructions: causalThemesInstructions,
				// Score criteria is an ORDERED ARRAY, low end first. Passing an
				// object here returns 400 invalid_type: expected array.
				Criteria: causalThemeLevels,
			},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("encode request: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	// The key travels only in this header. It is never placed in a URL, an
	// argument, an error, or a log line.
	req.Header.Set("Authorization", "Bearer "+d.key)
	req.Header.Set("Content-Type", "application/json")

	resp, err := d.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("systemone request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		// The body can echo request content; only the status is reported.
		return nil, fmt.Errorf("systemone responded %d", resp.StatusCode)
	}

	var decoded response
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	answer, themes, model, err := validateResponse(decoded)
	if err != nil {
		return nil, err
	}

	sig := &Signal{
		Probability: *answer.Noul,
		Model:       model,
		Step:        obs.Step,
		Round:       obs.Current.Number,
		ObservedAt:  d.now().Unix(),
	}
	if themes != nil {
		score := *themes.Score
		confidence := *themes.Confidence
		sig.CausalThemes = &score
		sig.CausalThemeConfidence = &confidence
	}
	return sig, nil
}

func validateResponse(decoded response) (answer, *answer, string, error) {
	model := strings.TrimSpace(decoded.Model)
	if model == "" {
		return answer{}, nil, "", fmt.Errorf("response carried no responding model version")
	}
	primaryRaw, ok := decoded.Answers[questionNonConvergence]
	if !ok {
		return answer{}, nil, "", fmt.Errorf("response carried no %s answer", questionNonConvergence)
	}
	var primaryWire noulAnswer
	if err := json.Unmarshal(primaryRaw, &primaryWire); err != nil {
		return answer{}, nil, "", fmt.Errorf("response carried malformed %s answer: %w", questionNonConvergence, err)
	}
	primary := answer{Type: primaryWire.Type, Noul: primaryWire.Noul}
	if err := validateNoul(primary); err != nil {
		return answer{}, nil, "", fmt.Errorf("response carried invalid %s answer: %w", questionNonConvergence, err)
	}

	var themes *answer
	if secondaryRaw, ok := decoded.Answers[questionCausalThemes]; ok {
		var secondary answer
		if err := json.Unmarshal(secondaryRaw, &secondary); err == nil && validScore(secondary, len(causalThemeLevels)) {
			themes = &secondary
		}
	}
	return primary, themes, model, nil
}

func validateNoul(a answer) error {
	if a.Type != "noul" {
		return fmt.Errorf("type is %q, want noul", a.Type)
	}
	if a.Noul == nil || !unitInterval(*a.Noul) {
		return fmt.Errorf("noul is missing or outside 0..1")
	}
	return nil
}

func validScore(a answer, levels int) bool {
	return a.Type == "score" &&
		a.Score != nil && *a.Score >= 0 && *a.Score <= float64(levels-1) &&
		a.Confidence != nil && unitInterval(*a.Confidence)
}

func unitInterval(value float64) bool {
	return value >= 0 && value <= 1
}

// --- wire types ---

type request struct {
	Model     string              `json:"model"`
	State     json.RawMessage     `json:"state"`
	Questions map[string]question `json:"questions"`
}

type question struct {
	Type         string `json:"type"`
	Instructions string `json:"instructions"`
	Criteria     any    `json:"criteria,omitempty"`
}

type noulCriteria struct {
	True  string `json:"true"`
	False string `json:"false"`
}

// Response-field classification is part of the validation boundary:
//
//   - DECISION-BEARING: model; answers keys; each answer's type discriminator;
//     Noul's noul value; Score's score value and confidence. Model must be
//     present for traceability. Noul is bounded to 0..1. Score is bounded to
//     0..(criteria levels-1), and only Score has confidence.
//   - TELEMETRY: usage and every unrecognized response field. They are not
//     decoded because telemetry must never decide whether a signal emits.
//
// A malformed primary Noul rejects the response. A malformed secondary Score
// is omitted without suppressing the valid Noul, because Noul is the deliverable.
// Keeping answers raw lets those two policies remain independent even when the
// secondary answer has the wrong JSON field types.
type response struct {
	Model   string                     `json:"model"`
	Answers map[string]json.RawMessage `json:"answers"`
}

type noulAnswer struct {
	Type string   `json:"type"`
	Noul *float64 `json:"noul"`
}

type answer struct {
	Type       string   `json:"type"`
	Noul       *float64 `json:"noul"`
	Score      *float64 `json:"score"`
	Confidence *float64 `json:"confidence"`
}
