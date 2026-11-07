package agenteval

import (
	"encoding/hex"
	"errors"
	"reflect"

	"github.com/ydtg1993/papa/v3/pkg/crawlagent"
)

func validHistoryRecord(h crawlagent.HistoryRecord) bool {
	if h.Stage == "" || h.Reference.ID == "" || h.Reference.Version == "" || h.State != "published" || h.ViewBytes <= 0 || h.ViewBytes > crawlagent.MaxHistoryViewBytes {
		return false
	}
	for _, hash := range []string{h.RuleSHA256, h.ViewSHA256} {
		if len(hash) != 64 {
			return false
		}
		if _, err := hex.DecodeString(hash); err != nil {
			return false
		}
	}
	return true
}

// ComparableHistoryIdentity allows only the explicit none -> published
// treatment. Everything else, including the host-authored history recipe,
// prompt, corpus, model and budgets, must remain identical before any calls.
func ComparableHistoryIdentity(control, treatment Identity) bool {
	if control.HistoryMode != "none" || treatment.HistoryMode != "published" || len(control.HistoryFixtureSHA256) != 64 {
		return false
	}
	treatment.HistoryMode = control.HistoryMode
	return reflect.DeepEqual(control, treatment)
}

// CompareHistory never relaxes Compare for ordinary baseline comparisons.
// Reports retain actual history references; the paired view is diagnostic and
// never a release proof or evidence that Replay measures real-model benefit.
func CompareHistory(control, treatment Report) (Comparison, error) {
	if err := control.ValidateComplete(); err != nil {
		return Comparison{}, err
	}
	if err := treatment.ValidateComplete(); err != nil {
		return Comparison{}, err
	}
	if !ComparableHistoryIdentity(control.Identity, treatment.Identity) {
		return Comparison{}, errors.New("agenteval: only history treatment may differ")
	}
	// Compare checks exact metric denominators using detached control-shaped
	// copies. Do not mutate the reports or strip history from the saved output.
	copyReport := treatment
	copyReport.Identity = control.Identity
	copyReport.Runs = append([]Run(nil), treatment.Runs...)
	for i := range copyReport.Runs {
		copyReport.Runs[i].Audit.History = nil
	}
	if _, err := Compare(control, copyReport); err != nil {
		return Comparison{}, err
	}
	before, _ := control.Summaries()
	after, _ := treatment.Summaries()
	return Comparison{Before: before, After: after}, nil
}
