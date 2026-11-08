package crawlagent

import "github.com/ydtg1993/papa/v4/pkg/rulestore"

// Each completed step is independently useful after a crash before Finish.
// References reveal neither page contents/URLs nor hidden fixture values.
func (e *execution) auditReferences() any {
	type sampleRef struct {
		Origin string `json:"origin,omitempty"`
		ID     string `json:"id"`
		Digest string `json:"digest"`
	}
	type candidateRef struct {
		Stage            string              `json:"stage"`
		Reference        rulestore.Reference `json:"reference"`
		TrainingReportID string              `json:"training_report_id,omitempty"`
		HoldoutReportID  string              `json:"holdout_report_id,omitempty"`
	}
	out := struct {
		Evidence   []sampleRef    `json:"evidence"`
		Candidates []candidateRef `json:"candidates"`
	}{}
	for _, item := range e.evidence {
		out.Evidence = append(out.Evidence, sampleRef{item.Origin, item.ID, item.BodySHA256})
	}
	for _, s := range e.goal.Stages {
		if c, ok := e.candidates[s.Stage]; ok {
			out.Candidates = append(out.Candidates, candidateRef{c.Stage, c.Reference, c.TrainingReportID, c.HoldoutReportID})
		}
	}
	return out
}
