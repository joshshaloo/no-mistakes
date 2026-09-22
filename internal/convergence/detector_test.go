package convergence

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// clearKeyEnv removes ambient operator keys so a test that means "no key
// configured" cannot accidentally pick one up from the developer's shell.
func clearKeyEnv(t *testing.T) {
	t.Helper()
	t.Setenv(openRouterKeyEnv, "")
	t.Setenv(typeSafeKeyEnv, "")
}

func twoRoundObservation() Observation {
	return Observation{
		Intent: "Reap child processes during worktree cleanup.",
		Step:   "review",
		Current: Round{
			Number:  3,
			Trigger: "auto_fix",
			Findings: []Finding{{
				ID:          "f7",
				Severity:    "error",
				File:        "internal/pipeline/steps/ci.go",
				Description: "The CI step still spawns without a process group, so grandchildren survive cancellation.",
			}},
		},
		Earlier: []Round{
			{Number: 1, Trigger: "initial", Findings: []Finding{{ID: "f1", File: "test.go", Description: "Test step leaks grandchildren."}}, SelectedForFix: []string{"f1"}},
			{Number: 2, Trigger: "auto_fix", FixSummary: "add process group to test step", Findings: []Finding{{ID: "f4", File: "lint.go", Description: "Lint step leaks grandchildren too."}}, SelectedForFix: []string{"f4"}},
		},
	}
}

func okDetector(t *testing.T, handler http.HandlerFunc) (*Detector, *httptest.Server) {
	t.Helper()
	clearKeyEnv(t)
	t.Setenv(openRouterKeyEnv, "test-key")
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	d := New(Settings{Enabled: true, BaseURL: srv.URL, Timeout: 2 * time.Second})
	if d == nil {
		t.Fatal("New returned nil with an enabled setting and a key present")
	}
	d.SetHTTPClient(srv.Client())
	return d, srv
}

func answerBody(t *testing.T, noul float64, model string) []byte {
	t.Helper()
	return []byte(`{"model":"` + model + `","answers":{"non_convergence":{"type":"noul","noul":` +
		strings.TrimRight(strings.TrimRight(formatFloat(noul), "0"), ".") +
		`},"causal_themes":{"type":"score","score":1.42,"confidence":0.8}},"usage":{"input_tokens":100,"output_tokens":20}}`)
}

func formatFloat(f float64) string {
	b, _ := json.Marshal(f)
	return string(b)
}

// --- disabled / unconfigured: the detector must not exist at all ---

func TestNewReturnsNilWhenNotEnabled(t *testing.T) {
	clearKeyEnv(t)
	t.Setenv(openRouterKeyEnv, "present")
	if d := New(Settings{Enabled: false}); d != nil {
		t.Fatal("detector must be nil when the feature is not enabled, even with a key present")
	}
}

func TestNewReturnsNilWhenNoKeyIsConfigured(t *testing.T) {
	clearKeyEnv(t)
	if d := New(Settings{Enabled: true, KeyFile: filepath.Join(t.TempDir(), "absent.env")}); d != nil {
		t.Fatal("detector must be nil when no key can be resolved")
	}
}

func TestNilDetectorObserveEmitsNothing(t *testing.T) {
	var d *Detector
	sig, err := d.Observe(context.Background(), twoRoundObservation())
	if sig != nil || err != nil {
		t.Fatalf("nil detector must emit nothing, got sig=%v err=%v", sig, err)
	}
}

// --- the single-round rule ---

func TestObserveEmitsNothingForASingleRoundRun(t *testing.T) {
	called := false
	d, _ := okDetector(t, func(w http.ResponseWriter, r *http.Request) {
		called = true
		_, _ = w.Write(answerBody(t, 0.9, "jev-test"))
	})
	obs := twoRoundObservation()
	obs.Current = Round{Number: 1, Trigger: "initial", Findings: obs.Current.Findings}
	obs.Earlier = nil

	sig, err := d.Observe(context.Background(), obs)
	if sig != nil || err != nil {
		t.Fatalf("a single-round run must emit nothing, got sig=%v err=%v", sig, err)
	}
	if called {
		t.Fatal("a single-round run must not reach the service at all")
	}
}

func TestObserveEmitsNothingWhenTheCurrentRoundHasNoFindings(t *testing.T) {
	called := false
	d, _ := okDetector(t, func(w http.ResponseWriter, r *http.Request) {
		called = true
		_, _ = w.Write(answerBody(t, 0.9, "jev-test"))
	})
	obs := twoRoundObservation()
	obs.Current.Findings = nil

	sig, err := d.Observe(context.Background(), obs)
	if sig != nil || err != nil {
		t.Fatalf("a round with no findings must emit nothing, got sig=%v err=%v", sig, err)
	}
	if called {
		t.Fatal("a round with no findings must not reach the service")
	}
}

// --- the success path ---

func TestObserveRecordsProbabilityAndRespondingModelVersion(t *testing.T) {
	d, _ := okDetector(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(answerBody(t, 0.83, "typesafe/jev-1.13-20260917"))
	})

	sig, err := d.Observe(context.Background(), twoRoundObservation())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sig == nil {
		t.Fatal("expected a signal")
	}
	if sig.Probability != 0.83 {
		t.Fatalf("probability = %v, want 0.83", sig.Probability)
	}
	if sig.Model != "typesafe/jev-1.13-20260917" {
		t.Fatalf("model = %q, want the responding model version", sig.Model)
	}
	if sig.Step != "review" || sig.Round != 3 {
		t.Fatalf("signal must locate the round it judged, got step=%q round=%d", sig.Step, sig.Round)
	}
	if sig.ObservedAt == 0 {
		t.Fatal("signal must carry an observation time")
	}
	if sig.CausalThemes == nil || *sig.CausalThemes != 1.42 {
		t.Fatalf("causal themes = %v, want 1.42", sig.CausalThemes)
	}
	if sig.CausalThemeConfidence == nil || *sig.CausalThemeConfidence != 0.8 {
		t.Fatalf("causal theme confidence = %v, want 0.8", sig.CausalThemeConfidence)
	}
}

func TestObserveRedactsSecretsAcrossTheCompleteRequestState(t *testing.T) {
	const key = "resolved-key-exact-match"
	clearKeyEnv(t)
	t.Setenv(openRouterKeyEnv, key)
	var requestBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var err error
		requestBody, err = io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request: %v", err)
		}
		_, _ = w.Write(answerBody(t, 0.5, "jev-test"))
	}))
	t.Cleanup(srv.Close)
	d := New(Settings{Enabled: true, BaseURL: srv.URL, Timeout: time.Second})
	if d == nil {
		t.Fatal("expected a detector")
	}
	d.SetHTTPClient(srv.Client())

	obs := twoRoundObservation()
	obs.Intent = "intent contains " + key
	obs.Earlier[1].FixSummary = "fixed using " + key
	obs.Current.Findings[0].Description = "finding exposes " + key
	if _, err := d.Observe(context.Background(), obs); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(string(requestBody), key) {
		t.Fatalf("serialized request leaked resolved key: %s", requestBody)
	}
	if !bytes.Contains(requestBody, []byte("[REDACTED]")) {
		t.Fatalf("serialized request did not carry redaction markers: %s", requestBody)
	}
}

func TestObserveSendsIntentCurrentAndEveryEarlierRound(t *testing.T) {
	var got struct {
		Model     string `json:"model"`
		State     stateDoc
		Questions map[string]struct {
			Type         string          `json:"type"`
			Instructions string          `json:"instructions"`
			Criteria     json.RawMessage `json:"criteria"`
		} `json:"questions"`
	}
	d, _ := okDetector(t, func(w http.ResponseWriter, r *http.Request) {
		var raw struct {
			Model     string          `json:"model"`
			State     json.RawMessage `json:"state"`
			Questions json.RawMessage `json:"questions"`
		}
		if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
			t.Errorf("request body did not decode: %v", err)
		}
		if err := json.Unmarshal(raw.State, &got.State); err != nil {
			t.Errorf("state did not decode: %v", err)
		}
		if err := json.Unmarshal(raw.Questions, &got.Questions); err != nil {
			t.Errorf("questions did not decode: %v", err)
		}
		got.Model = raw.Model
		if auth := r.Header.Get("Authorization"); auth != "Bearer test-key" {
			t.Errorf("Authorization header = %q, want the bearer key", auth)
		}
		_, _ = w.Write(answerBody(t, 0.5, "jev-test"))
	})

	if _, err := d.Observe(context.Background(), twoRoundObservation()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.State.AcceptedIntent == "" {
		t.Error("state must carry the accepted intent")
	}
	if got.State.CurrentRound.Number != 3 || len(got.State.CurrentRound.Findings) != 1 {
		t.Errorf("state must carry the current round's finding, got %+v", got.State.CurrentRound)
	}
	if len(got.State.EarlierRounds) != 2 {
		t.Fatalf("state must carry every earlier round, got %d", len(got.State.EarlierRounds))
	}
	if got.State.EarlierRounds[1].FixSummary == "" {
		t.Error("state must carry earlier rounds' applied-fix summaries")
	}
	// Score criteria is an ordered array; an object returns 400 invalid_type.
	themes := got.Questions[questionCausalThemes]
	if themes.Type != "score" {
		t.Fatalf("causal themes question type = %q", themes.Type)
	}
	var levels []string
	if err := json.Unmarshal(themes.Criteria, &levels); err != nil {
		t.Fatalf("score criteria must be an ordered array, got %s", themes.Criteria)
	}
	if len(levels) < 2 {
		t.Fatalf("score criteria needs at least two levels, got %d", len(levels))
	}
	if got.Questions[questionNonConvergence].Type != "noul" {
		t.Errorf("non-convergence question must be a noul")
	}
}

// --- every failure mode stays silent ---

func TestObserveEmitsNothingOnHTTPError(t *testing.T) {
	d, _ := okDetector(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "upstream exploded", http.StatusInternalServerError)
	})
	sig, err := d.Observe(context.Background(), twoRoundObservation())
	if sig != nil {
		t.Fatalf("an HTTP error must emit nothing, got %+v", sig)
	}
	if err == nil {
		t.Fatal("an HTTP error must be reported to the caller as an error, never as a value")
	}
}

func TestObserveEmitsNothingOnMalformedBody(t *testing.T) {
	d, _ := okDetector(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("{not json"))
	})
	sig, err := d.Observe(context.Background(), twoRoundObservation())
	if sig != nil || err == nil {
		t.Fatalf("a malformed body must emit nothing, got sig=%v err=%v", sig, err)
	}
}

func TestObserveEmitsNothingWhenTheExpectedAnswerIsMissing(t *testing.T) {
	d, _ := okDetector(t, func(w http.ResponseWriter, r *http.Request) {
		// Well-formed, but the non-convergence answer is absent. An absent
		// answer must never become "converging".
		_, _ = w.Write([]byte(`{"model":"jev-test","answers":{"causal_themes":{"type":"score","score":1.0}}}`))
	})
	sig, err := d.Observe(context.Background(), twoRoundObservation())
	if sig != nil || err == nil {
		t.Fatalf("a missing answer must emit nothing, got sig=%v err=%v", sig, err)
	}
}

func TestObserveValidatesEveryPrimaryDecisionFieldBeforeEmitting(t *testing.T) {
	valid := `{"model":"jev-test","answers":{"non_convergence":{"type":"noul","noul":0.7,"confidence":"not-a-noul-field"},"causal_themes":{"type":"score","score":1.4,"confidence":0.8}}}`
	tests := []struct {
		name string
		body string
		want bool
	}{
		{name: "fully valid", body: valid, want: true},
		{name: "model missing", body: `{"answers":{"non_convergence":{"type":"noul","noul":0.7}}}`},
		{name: "model wrong type", body: `{"model":7,"answers":{"non_convergence":{"type":"noul","noul":0.7}}}`},
		{name: "primary discriminator", body: `{"model":"jev-test","answers":{"non_convergence":{"type":"score","noul":0.7}}}`},
		{name: "primary value missing", body: `{"model":"jev-test","answers":{"non_convergence":{"type":"noul"}}}`},
		{name: "primary value wrong type", body: `{"model":"jev-test","answers":{"non_convergence":{"type":"noul","noul":"high"}}}`},
		{name: "primary value out of range", body: `{"model":"jev-test","answers":{"non_convergence":{"type":"noul","noul":7}}}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, _ := okDetector(t, func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(tt.body))
			})
			sig, err := d.Observe(context.Background(), twoRoundObservation())
			if tt.want {
				if err != nil || sig == nil {
					t.Fatalf("valid response emitted sig=%v err=%v", sig, err)
				}
				return
			}
			if sig != nil || err == nil {
				t.Fatalf("malformed response must emit nothing, got sig=%v err=%v", sig, err)
			}
		})
	}
}

func TestObserveDropsMalformedSecondaryScoreWithoutSuppressingNoul(t *testing.T) {
	tests := []struct {
		name      string
		secondary string
	}{
		{name: "wrong discriminator", secondary: `{"type":"noul","score":1.4,"confidence":0.8}`},
		{name: "missing value", secondary: `{"type":"score","confidence":0.8}`},
		{name: "wrong value type", secondary: `{"type":"score","score":"many","confidence":0.8}`},
		{name: "value below range", secondary: `{"type":"score","score":-0.1,"confidence":0.8}`},
		{name: "value above criteria", secondary: `{"type":"score","score":3,"confidence":0.8}`},
		{name: "missing confidence", secondary: `{"type":"score","score":1.4}`},
		{name: "wrong confidence type", secondary: `{"type":"score","score":1.4,"confidence":"high"}`},
		{name: "confidence out of range", secondary: `{"type":"score","score":1.4,"confidence":2}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := `{"model":"jev-test","answers":{"non_convergence":{"type":"noul","noul":0.7},"causal_themes":` + tt.secondary + `}}`
			d, _ := okDetector(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) })
			sig, err := d.Observe(context.Background(), twoRoundObservation())
			if err != nil || sig == nil || sig.Probability != 0.7 {
				t.Fatalf("malformed Score suppressed valid Noul: sig=%+v err=%v", sig, err)
			}
			if sig.CausalThemes != nil || sig.CausalThemeConfidence != nil {
				t.Fatalf("malformed Score must be absent, got %+v", sig)
			}
		})
	}
}

func TestRecordedJevResponseFixturesMatchValidators(t *testing.T) {
	for _, name := range []string{"noul", "score"} {
		raw, err := os.ReadFile(filepath.Join("testdata", "jev-"+name+"-answer.json"))
		if err != nil {
			t.Fatal(err)
		}
		var got answer
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("decode recorded %s answer: %v", name, err)
		}
		if name == "noul" {
			if err := validateNoul(got); err != nil {
				t.Fatalf("recorded Noul rejected unchanged: %v", err)
			}
		} else if !validScore(got, len(causalThemeLevels)) {
			t.Fatal("recorded Score rejected unchanged")
		}
	}

	raw, err := os.ReadFile(filepath.Join("testdata", "jev-combined-response.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got response
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if _, themes, _, err := validateResponse(got); err != nil || themes == nil {
		t.Fatalf("recorded combined response rejected unchanged: themes=%+v err=%v", themes, err)
	}
}

func TestObserveIgnoresUsageTelemetry(t *testing.T) {
	tests := []struct {
		name  string
		usage string
	}{
		{name: "absent"},
		{name: "garbage", usage: `,"usage":{"input_tokens":"many","bogus":-1}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, _ := okDetector(t, func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(`{"model":"jev-test","answers":{"non_convergence":{"type":"noul","noul":0.6}}` + tt.usage + `}`))
			})
			sig, err := d.Observe(context.Background(), twoRoundObservation())
			if err != nil || sig == nil || sig.Probability != 0.6 {
				t.Fatalf("usage telemetry affected decision: sig=%+v err=%v", sig, err)
			}
		})
	}
}

func TestObserveKeepsTheNoulWhenTheScoreIsAbsent(t *testing.T) {
	d, _ := okDetector(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"model":"jev-test","answers":{"non_convergence":{"type":"noul","noul":0.6}},"usage":{"input_tokens":100,"output_tokens":20}}`))
	})
	sig, err := d.Observe(context.Background(), twoRoundObservation())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sig == nil || sig.Probability != 0.6 {
		t.Fatalf("the Noul is the deliverable and must survive a missing Score, got %+v", sig)
	}
	if sig.CausalThemes != nil {
		t.Fatalf("an absent Score must stay absent, got %v", *sig.CausalThemes)
	}
}

// --- the call is bounded ---

func TestObserveIsBoundedBySettingsTimeout(t *testing.T) {
	clearKeyEnv(t)
	t.Setenv(openRouterKeyEnv, "test-key")
	// A service that never answers within the bound. The handler returns on its
	// own so the test server can shut down once the client has given up.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(time.Second):
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)

	d := New(Settings{Enabled: true, BaseURL: srv.URL, Timeout: 150 * time.Millisecond})
	if d == nil {
		t.Fatal("expected a detector")
	}
	d.SetHTTPClient(srv.Client())

	start := time.Now()
	sig, err := d.Observe(context.Background(), twoRoundObservation())
	elapsed := time.Since(start)

	if sig != nil || err == nil {
		t.Fatalf("a slow service must emit nothing, got sig=%v err=%v", sig, err)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("a slow service extended the call by %v; it must stay bounded by the configured timeout", elapsed)
	}
}

func TestObserveDoesNotRetryAfterContextCancellation(t *testing.T) {
	calls := 0
	d, _ := okDetector(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = w.Write(answerBody(t, 0.9, "jev-test"))
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	sig, err := d.Observe(ctx, twoRoundObservation())
	if sig != nil || err == nil {
		t.Fatalf("a cancelled context must emit nothing, got sig=%v err=%v", sig, err)
	}
	if calls != 0 {
		t.Fatalf("a cancelled context must not reach the service, got %d calls", calls)
	}
}

// --- key resolution ---

func TestResolveKeyReadsTheOperatorsExistingEnvFile(t *testing.T) {
	clearKeyEnv(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "openrouter.env")
	contents := "# operator file\nexport OPENROUTER_API_KEY=\"file-key\"\nTYPESAFE_BASE_URL=https://openrouter.ai/api\n"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := resolveKey(path); got != "file-key" {
		t.Fatalf("resolveKey = %q, want the key from the operator's file", got)
	}
}

func TestResolveKeyPrefersTheEnvironmentOverTheFile(t *testing.T) {
	clearKeyEnv(t)
	t.Setenv(openRouterKeyEnv, "env-key")
	path := filepath.Join(t.TempDir(), "openrouter.env")
	if err := os.WriteFile(path, []byte("OPENROUTER_API_KEY=file-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := resolveKey(path); got != "env-key" {
		t.Fatalf("resolveKey = %q, want the environment value", got)
	}
}

func TestResolveKeyIsEmptyWhenNothingIsConfigured(t *testing.T) {
	clearKeyEnv(t)
	if got := resolveKey(""); got != "" {
		t.Fatalf("resolveKey = %q, want empty", got)
	}
	if got := resolveKey(filepath.Join(t.TempDir(), "missing.env")); got != "" {
		t.Fatalf("resolveKey on a missing file = %q, want empty", got)
	}
}

// --- state budget ---

func TestBuildStateTruncatesBodiesRatherThanDroppingEarlierRounds(t *testing.T) {
	obs := Observation{Intent: "do the thing", Step: "review"}
	huge := strings.Repeat("x", 4000)
	for i := 1; i <= 40; i++ {
		obs.Earlier = append(obs.Earlier, Round{
			Number:     i,
			Trigger:    "auto_fix",
			FixSummary: huge,
			Findings:   []Finding{{ID: "f", Description: huge}, {ID: "g", Description: huge}},
		})
	}
	obs.Current = Round{Number: 41, Findings: []Finding{{ID: "cur", Description: huge}}}

	encoded, err := buildState(obs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(encoded) > maxStateBytes {
		t.Fatalf("state is %d bytes, over the %d budget", len(encoded), maxStateBytes)
	}
	var doc stateDoc
	if err := json.Unmarshal(encoded, &doc); err != nil {
		t.Fatalf("state did not decode: %v", err)
	}
	if len(doc.EarlierRounds) != 40 {
		t.Fatalf("earlier rounds = %d, want all 40 kept: the earlier rounds are the question", len(doc.EarlierRounds))
	}
	if len(doc.EarlierRounds[0].Findings[0].Description) >= len(huge) {
		t.Fatal("long bodies must be truncated to fit the budget")
	}
}

func TestBuildStateKeepsEveryRoundEvenWhenNoLimitFits(t *testing.T) {
	obs := Observation{Step: "review", Current: Round{Number: 2000, Findings: []Finding{{ID: "cur", Description: "x"}}}}
	for i := 1; i < 2000; i++ {
		obs.Earlier = append(obs.Earlier, Round{Number: i, Findings: []Finding{{ID: "f", Description: strings.Repeat("y", 200)}}})
	}
	encoded, err := buildState(obs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var doc stateDoc
	if err := json.Unmarshal(encoded, &doc); err != nil {
		t.Fatalf("state did not decode: %v", err)
	}
	if len(doc.EarlierRounds) != 1999 {
		t.Fatalf("earlier rounds = %d, want every round kept rather than dropped for budget", len(doc.EarlierRounds))
	}
}

// The key is never a command argument and never reaches a log. The only place
// an error could leak it is a response body echoed into the error text, so the
// detector reports the status code and nothing else.
func TestObserveErrorsNeverCarryTheKey(t *testing.T) {
	const key = "sk-super-secret-value"
	clearKeyEnv(t)
	t.Setenv(openRouterKeyEnv, key)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A hostile or careless service echoes the credential back.
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"bad Authorization: Bearer ` + key + `"}`))
	}))
	t.Cleanup(srv.Close)

	d := New(Settings{Enabled: true, BaseURL: srv.URL, Timeout: time.Second})
	if d == nil {
		t.Fatal("expected a detector")
	}
	d.SetHTTPClient(srv.Client())

	sig, err := d.Observe(context.Background(), twoRoundObservation())
	if sig != nil {
		t.Fatal("a 400 must emit nothing")
	}
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), key) {
		t.Fatalf("the error leaked the key: %q", err.Error())
	}
}

func TestBuiltStateNeverCarriesTheKey(t *testing.T) {
	const key = "sk-super-secret-value"
	clearKeyEnv(t)
	t.Setenv(openRouterKeyEnv, key)
	encoded, err := buildState(twoRoundObservation())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), key) {
		t.Fatal("the state must never carry the key")
	}
}
