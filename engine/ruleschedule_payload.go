package engine

import (
	"context"
	"encoding/json"
	"maps"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ydtg1993/papa/v3/models"
)

const scheduleMeta = "_rule_schedule"
const scheduleMaxBytes = 2 << 20

type scheduleSpec struct {
	SchemaVersion int               `json:"schema_version"`
	Request       RuleRequest       `json:"request"`
	Account       string            `json:"account,omitempty"`
	Meta          map[string]string `json:"meta,omitempty"`
}

type scheduleRoot struct {
	Site  string            `json:"site"`
	Stage string            `json:"stage"`
	URL   string            `json:"url"`
	Meta  map[string]string `json:"meta"`
}

func (r scheduleRoot) task() *Task {
	return &Task{Site: r.Site, Stage: r.Stage, URL: r.URL, Meta: maps.Clone(r.Meta)}
}

type schedulePayload struct {
	SchemaVersion int          `json:"schema_version"`
	SpecSHA256    string       `json:"spec_sha256"`
	Sequence      uint64       `json:"sequence"`
	BatchID       string       `json:"batch_id"`
	Tick          string       `json:"tick"`
	Root          scheduleRoot `json:"root"`
}

type scheduleControl struct {
	models.CrawlerTask
	spec     scheduleSpec
	specTask *Task
}

func (c scheduleControl) processing() bool { return c.Status == models.TaskStatusProcessing }

type scheduleState struct {
	row     models.RuleSchedule
	payload schedulePayload
}

func scheduleText(s string, max int) bool {
	return s != "" && strings.TrimSpace(s) == s && len(s) <= max && utf8.ValidString(s) && !strings.ContainsAny(s, "\x00\r\n")
}
func scheduleTick(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}
func scheduleBatchID(c scheduleControl, sequence uint64) string {
	return ruleDigest([]byte(ruleJSON(struct {
		TaskID           uint
		Site, Stage, URL string
		Sequence         uint64
	}{c.ID, c.Site, c.Stage, c.URL, sequence})))
}

func (s *RuleSchedule) specTask(spec scheduleSpec) (*Task, error) {
	if spec.SchemaVersion != 1 || len(spec.Request.State) > 16<<10 || !utf8.ValidString(spec.Request.State) || len(spec.Account) > 128 || !utf8.ValidString(spec.Account) {
		return nil, ErrRuleScheduleInvalid
	}
	canonical, err := s.config.Entry.allowedURL(spec.Request.URL)
	if err != nil {
		return nil, ErrRuleScheduleInvalid
	}
	spec.Request.URL = canonical
	spec.Meta = maps.Clone(spec.Meta)
	for k, v := range spec.Meta {
		if k == "" || ruleReserved(k) || !utf8.ValidString(k) || !utf8.ValidString(v) {
			return nil, ErrRuleScheduleInvalid
		}
	}
	encoded := ruleJSON(spec)
	if len(ruleJSON(map[string]string{scheduleMeta: encoded})) > scheduleMaxBytes || !utf8.ValidString(encoded) {
		return nil, ErrRuleScheduleInvalid
	}
	url := canonical + "#papa-rule-schedule=" + ruleDigest([]byte(ruleJSON(struct{ Site, Stage, Entry, Spec string }{s.config.Entry.config.Site, s.config.Stage, s.config.Entry.config.Stage, encoded})))
	if len(url) > 65535 {
		return nil, ErrRuleScheduleInvalid
	}
	return &Task{Site: s.config.Entry.config.Site, Stage: s.config.Stage, URL: url, Repeatable: true, RepeatInterval: s.config.Interval, Meta: map[string]string{scheduleMeta: encoded}}, nil
}

func (s *RuleSchedule) decodeControl(row models.CrawlerTask) (scheduleControl, error) {
	c := scheduleControl{CrawlerTask: row}
	var meta map[string]string
	if row.ID == 0 || row.Site != s.config.Entry.config.Site || row.Stage != s.config.Stage || row.IdempotencyKey != "" || len(row.Meta) > scheduleMaxBytes || !utf8.Valid(row.Meta) || json.Unmarshal(row.Meta, &meta) != nil || len(meta) != 1 || ruleDecode(meta[scheduleMeta], &c.spec) != nil {
		return c, ErrRuleScheduleCorrupt
	}
	expected, err := s.specTask(c.spec)
	if err != nil || expected.URL != row.URL || expected.Meta[scheduleMeta] != meta[scheduleMeta] {
		return c, ErrRuleScheduleCorrupt
	}
	c.specTask = expected
	return c, nil
}

func (s *RuleSchedule) prepareBatch(ctx context.Context, c scheduleControl, seq uint64) (schedulePayload, error) {
	root, err := s.config.Entry.NewTaskContext(ctx, c.spec.Request, c.spec.Meta)
	if err != nil {
		return schedulePayload{}, err
	}
	request, bindings, cp, err := s.config.Entry.taskState(root)
	if err != nil {
		return schedulePayload{}, err
	}
	if _, exists := bindings[s.config.Stage]; exists {
		return schedulePayload{}, ErrRuleScheduleInvalid
	}
	batch := scheduleBatchID(c, seq)
	root.Meta[ruleBatchMeta] = batch
	root, err = s.config.Entry.makeTask(s.config.Entry.config.Stage, request, root.Meta, bindings, c.spec.Account, cp)
	if err != nil {
		return schedulePayload{}, err
	}
	p := schedulePayload{SchemaVersion: 1, SpecSHA256: ruleDigest([]byte(c.specTask.Meta[scheduleMeta])), Sequence: seq, BatchID: batch, Tick: scheduleTick(c.LastRepeatAt), Root: scheduleRoot{root.Site, root.Stage, root.URL, root.Meta}}
	_, err = s.encodePayload(c, p)
	return p, err
}

func (s *RuleSchedule) encodePayload(c scheduleControl, p schedulePayload) ([]byte, error) {
	if p.SchemaVersion != 1 || p.Sequence == 0 || p.SpecSHA256 != ruleDigest([]byte(c.specTask.Meta[scheduleMeta])) || p.BatchID != scheduleBatchID(c, p.Sequence) || !validRuleBatch(p.BatchID) || p.Root.Meta[ruleBatchMeta] != p.BatchID || p.Root.Meta[ruleAccountMeta] != c.spec.Account {
		return nil, ErrRuleScheduleCorrupt
	}
	if p.Tick != "" {
		tick, err := time.Parse(time.RFC3339Nano, p.Tick)
		if err != nil || scheduleTick(&tick) != p.Tick {
			return nil, ErrRuleScheduleCorrupt
		}
	}
	if !utf8.ValidString(p.Root.URL) {
		return nil, ErrRuleScheduleCorrupt
	}
	for k, v := range p.Root.Meta {
		if !utf8.ValidString(k) || !utf8.ValidString(v) {
			return nil, ErrRuleScheduleCorrupt
		}
	}
	req, bindings, cp, err := s.config.Entry.taskState(p.Root.task())
	if err != nil || req != c.spec.Request || len(cp.Requests) != 0 || len(cp.Bodies) != 0 || len(cp.Items) != 0 {
		return nil, ErrRuleScheduleCorrupt
	}
	if _, exists := bindings[s.config.Stage]; exists {
		return nil, ErrRuleScheduleCorrupt
	}
	business := map[string]string{}
	for k, v := range p.Root.Meta {
		if !ruleReserved(k) {
			business[k] = v
		}
	}
	if len(business) != len(c.spec.Meta) {
		return nil, ErrRuleScheduleCorrupt
	}
	for k, v := range business {
		if expected, ok := c.spec.Meta[k]; !ok || expected != v {
			return nil, ErrRuleScheduleCorrupt
		}
	}
	b, err := json.Marshal(p)
	if err != nil || len(b) > scheduleMaxBytes {
		return nil, ErrRuleScheduleCorrupt
	}
	return b, nil
}

func (s *RuleSchedule) decodeState(c scheduleControl, row models.RuleSchedule) (*scheduleState, error) {
	if row.TaskID != c.ID || row.Revision == 0 || len(row.PayloadJSON) > scheduleMaxBytes || !utf8.ValidString(row.PayloadJSON) || ruleDigest([]byte(row.PayloadJSON)) != row.PayloadSHA256 {
		return nil, ErrRuleScheduleCorrupt
	}
	var p schedulePayload
	if ruleDecode(row.PayloadJSON, &p) != nil {
		return nil, ErrRuleScheduleCorrupt
	}
	b, err := s.encodePayload(c, p)
	if err != nil || string(b) != row.PayloadJSON {
		return nil, ErrRuleScheduleCorrupt
	}
	return &scheduleState{row, p}, nil
}
