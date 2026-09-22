package convergence

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/intent"
)

// stateDoc is the exact shape the question is asked over. Field names are part
// of the question: the instructions reference `earlier_rounds` and
// `current_round` by name.
type stateDoc struct {
	AcceptedIntent string  `json:"accepted_intent,omitempty"`
	Step           string  `json:"step,omitempty"`
	CurrentRound   Round   `json:"current_round"`
	EarlierRounds  []Round `json:"earlier_rounds"`
}

// buildState renders the observation as JSON within the request budget.
//
// Truncation policy: long finding descriptions shrink, earlier rounds never
// drop. A run's earlier rounds ARE the question - a state that kept full prose
// but lost round two could not answer whether round three repeats its cause -
// so every round survives at the shortest description length before anything
// else is considered.
func redactObservation(obs Observation, exactSecret string) Observation {
	redact := func(text string) string {
		if exactSecret != "" {
			text = strings.ReplaceAll(text, exactSecret, "[REDACTED]")
		}
		return intent.RedactSecrets(text)
	}
	redactFinding := func(f Finding) Finding {
		f.ID = redact(f.ID)
		f.Severity = redact(f.Severity)
		f.File = redact(f.File)
		f.Action = redact(f.Action)
		f.Description = redact(f.Description)
		return f
	}
	redactRound := func(r Round) Round {
		r.Trigger = redact(r.Trigger)
		r.FixSummary = redact(r.FixSummary)
		r.SelectedForFix = append([]string(nil), r.SelectedForFix...)
		for i := range r.SelectedForFix {
			r.SelectedForFix[i] = redact(r.SelectedForFix[i])
		}
		r.Findings = append([]Finding(nil), r.Findings...)
		for i := range r.Findings {
			r.Findings[i] = redactFinding(r.Findings[i])
		}
		return r
	}

	obs.Intent = redact(obs.Intent)
	obs.Step = redact(obs.Step)
	obs.Current = redactRound(obs.Current)
	obs.Earlier = append([]Round(nil), obs.Earlier...)
	for i := range obs.Earlier {
		obs.Earlier[i] = redactRound(obs.Earlier[i])
	}
	return obs
}

func buildState(obs Observation) ([]byte, error) {
	for _, limit := range descriptionLimits {
		encoded, err := json.Marshal(renderState(obs, limit))
		if err != nil {
			return nil, fmt.Errorf("encode state: %w", err)
		}
		if len(encoded) <= maxStateBytes {
			return encoded, nil
		}
	}
	// Still over budget at the shortest description length. Keep every round
	// and let the shortest rendering stand: the service enforces its own limit
	// and a rejected request is simply one more way the detector stays silent,
	// which is strictly better than answering the question over a history with
	// rounds missing from it.
	encoded, err := json.Marshal(renderState(obs, descriptionLimits[len(descriptionLimits)-1]))
	if err != nil {
		return nil, fmt.Errorf("encode state: %w", err)
	}
	return encoded, nil
}

func renderState(obs Observation, descriptionLimit int) stateDoc {
	doc := stateDoc{
		AcceptedIntent: truncate(clean(obs.Intent), maxIntentChars),
		Step:           clean(obs.Step),
		CurrentRound:   renderRound(obs.Current, descriptionLimit),
		EarlierRounds:  make([]Round, 0, len(obs.Earlier)),
	}
	for _, r := range obs.Earlier {
		doc.EarlierRounds = append(doc.EarlierRounds, renderRound(r, descriptionLimit))
	}
	return doc
}

func renderRound(r Round, descriptionLimit int) Round {
	out := Round{
		Number:         r.Number,
		Trigger:        clean(r.Trigger),
		FixSummary:     truncate(clean(r.FixSummary), descriptionLimit),
		SelectedForFix: make([]string, 0, len(r.SelectedForFix)),
	}
	for _, id := range r.SelectedForFix {
		if id = clean(id); id != "" {
			out.SelectedForFix = append(out.SelectedForFix, id)
		}
	}
	if len(out.SelectedForFix) == 0 {
		out.SelectedForFix = nil
	}
	for _, f := range r.Findings {
		out.Findings = append(out.Findings, Finding{
			ID:          clean(f.ID),
			Severity:    clean(f.Severity),
			File:        clean(f.File),
			Action:      clean(f.Action),
			Description: truncate(clean(f.Description), descriptionLimit),
		})
	}
	return out
}

// clean collapses whitespace so a multi-line finding body costs one line of
// budget. It is not a security boundary: the state is a question's input, not
// an instruction channel, and the service treats it as data.
func clean(text string) string {
	return strings.Join(strings.Fields(text), " ")
}

func truncate(text string, limit int) string {
	if limit <= 0 || len(text) <= limit {
		return text
	}
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return strings.TrimSpace(string(runes[:limit])) + "..."
}
