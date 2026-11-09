package crawlagent

import (
	"bytes"
	"encoding/json"
	"io"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ydtg1993/papa/v4/pkg/webaccess"
)

type Agent struct{ config Config }

func New(cfg Config) (*Agent, error) {
	if cfg.Model == nil || cfg.Audit == nil || cfg.Repository == nil || cfg.Blobs == nil || cfg.SamplePolicy.RedactBody == nil || !validText(cfg.ModelName, 128) || !validText(cfg.Actor.Operator, 128) || !validText(cfg.Actor.Source, 256) {
		return nil, ErrInvalid
	}
	b := &cfg.Budgets
	if b.MaxDuration == 0 {
		b.MaxDuration = 180 * time.Second
	}
	if b.MaxDuration <= 0 || b.MaxDuration > 600*time.Second {
		return nil, ErrInvalid
	}
	for _, v := range []struct {
		p        *int
		def, max int
	}{{&b.MaxModelCalls, 12, 64}, {&b.MaxCandidatesPerStage, 3, 8}, {&b.MaxOutputTokens, 4096, 16384}, {&b.MaxInputBytes, 128 << 10, 256 << 10}, {&b.MaxTotalInputBytes, 1 << 20, 4 << 20}} {
		if *v.p == 0 {
			*v.p = v.def
		}
		if *v.p < 1 || *v.p > v.max {
			return nil, ErrInvalid
		}
	}
	p := b.Prices
	if p.InputPerMillionMicros < 0 || p.OutputPerMillionMicros < 0 || p.InputPerMillionMicros > 1_000_000_000 || p.OutputPerMillionMicros > 1_000_000_000 || p.MaxMicros < 0 || (p.MaxMicros > 0 && (p.InputPerMillionMicros == 0 || p.OutputPerMillionMicros == 0)) {
		return nil, ErrInvalid
	}
	cfg.SamplePolicy.Headers = append([]string(nil), cfg.SamplePolicy.Headers...)
	cfg.SamplePolicy.QueryKeys = append([]string(nil), cfg.SamplePolicy.QueryKeys...)
	cloneOrigins := func(in []webaccess.Origin) []webaccess.Origin {
		out := append([]webaccess.Origin(nil), in...)
		for i := range out {
			out[i].PathPrefixes = append([]string(nil), out[i].PathPrefixes...)
		}
		return out
	}
	cfg.Web.Policy.Origins = cloneOrigins(cfg.Web.Policy.Origins)
	cfg.Web.Policy.ResourceOrigins = cloneOrigins(cfg.Web.Policy.ResourceOrigins)
	if cfg.Web.Budgets.MaxDuration == 0 || cfg.Web.Budgets.MaxDuration > b.MaxDuration {
		cfg.Web.Budgets.MaxDuration = b.MaxDuration
	}
	if cfg.SourceFactory == nil {
		cfg.SourceFactory = func(c webaccess.Config) (Source, error) { return webaccess.New(c) }
	}
	// Validate host-only access policy before recording it or constructing any
	// model messages. New/Close does not start a browser or send network traffic.
	probe, err := webaccess.New(cfg.Web)
	if err != nil {
		return nil, ErrInvalid
	}
	cfg.Web.Budgets = probe.Budgets()
	if cfg.Web.Renderer.GlobalConcurrency == 0 {
		cfg.Web.Renderer.GlobalConcurrency = 1
	}
	if cfg.AllowRecordAPI && !cfg.Web.Renderer.Enabled {
		_ = probe.Close()
		return nil, ErrInvalid
	}
	_ = probe.Close()
	return &Agent{config: cfg}, nil
}

func validText(s string, max int) bool {
	return s != "" && len(s) <= max && strings.TrimSpace(s) == s && utf8.ValidString(s) && !strings.ContainsAny(s, "\x00\r\n")
}

func decodeStrict(raw []byte, out any, max int) error {
	if len(raw) == 0 || len(raw) > max || !unambiguousJSON(raw) {
		return ErrAction
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	d.DisallowUnknownFields()
	if d.Decode(out) != nil || d.Decode(new(any)) != io.EOF {
		return ErrAction
	}
	return nil
}

// JSON decoders otherwise accept duplicate keys with last-writer-wins
// semantics. Reject them before interpreting a model command or rule.
func unambiguousJSON(raw []byte) bool {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var value func(int) bool
	value = func(depth int) bool {
		if depth > 64 {
			return false
		}
		token, err := d.Token()
		if err != nil {
			return false
		}
		delim, ok := token.(json.Delim)
		if !ok {
			return true
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				key, err := d.Token()
				name, ok := key.(string)
				if err != nil || !ok || seen[name] {
					return false
				}
				seen[name] = true
				if !value(depth + 1) {
					return false
				}
			}
			end, err := d.Token()
			return err == nil && end == json.Delim('}')
		case '[':
			for d.More() {
				if !value(depth + 1) {
					return false
				}
			}
			end, err := d.Token()
			return err == nil && end == json.Delim(']')
		default:
			return false
		}
	}
	if !value(0) {
		return false
	}
	_, err := d.Token()
	return err == io.EOF
}

func cloneGoal(goal Goal) (Goal, error) {
	data, err := json.Marshal(goal)
	if err != nil || len(data) > 64<<10 {
		return Goal{}, ErrInvalid
	}
	var out Goal
	if decodeStrict(data, &out, 64<<10) != nil || !validText(out.Site, 128) || len(out.Seeds) == 0 || len(out.Seeds) > 32 || len(out.Stages) == 0 || len(out.Stages) > 8 {
		return Goal{}, ErrInvalid
	}
	if len(out.EvidenceSamples) > 64 || len(out.Context) > 4096 || !utf8.ValidString(out.Context) {
		return Goal{}, ErrInvalid
	}
	for _, id := range out.EvidenceSamples {
		if !validText(id, 128) {
			return Goal{}, ErrInvalid
		}
	}
	for _, s := range out.Seeds {
		if !validText(s, 16<<10) {
			return Goal{}, ErrInvalid
		}
	}
	stages := map[string]bool{}
	ruleIDs := map[string]bool{}
	for i := range out.Stages {
		s := &out.Stages[i]
		if s.Transport == "" {
			s.Transport = "http"
		}
		if !validText(s.Stage, 128) || !validText(s.RuleID, 128) || stages[s.Stage] || ruleIDs[s.RuleID] || (s.Transport != "http" && s.Transport != "browser") || len(s.Fields) == 0 || len(s.Fields) > 32 || len(s.Links) > 8 {
			return Goal{}, ErrInvalid
		}
		if s.Browser != nil && (s.Transport != "browser" || s.Browser.Validate() != nil) {
			return Goal{}, ErrInvalid
		}
		stages[s.Stage] = true
		ruleIDs[s.RuleID] = true
		fields := map[string]bool{}
		for _, f := range s.Fields {
			if !validText(f.Name, 128) || len(f.Description) > 2048 || !utf8.ValidString(f.Description) || fields[f.Name] {
				return Goal{}, ErrInvalid
			}
			fields[f.Name] = true
			switch f.Kind {
			case "any", "string", "number", "integer", "bool", "object", "array", "http-url":
			default:
				return Goal{}, ErrInvalid
			}
		}
		if s.ItemKey != "" && !fields[s.ItemKey] {
			return Goal{}, ErrInvalid
		}
		for _, link := range s.Links {
			if !fields[link.Field] || len(link.Meta) > 32 {
				return Goal{}, ErrInvalid
			}
			for key, value := range link.Meta {
				if !validText(key, 128) || !fields[value] {
					return Goal{}, ErrInvalid
				}
			}
		}
		for _, id := range []string{s.TrainingSet, s.HoldoutSet} {
			if id != "" && !validText(id, 128) {
				return Goal{}, ErrInvalid
			}
		}
	}
	for _, s := range out.Stages {
		for _, link := range s.Links {
			if !stages[link.Stage] {
				return Goal{}, ErrInvalid
			}
		}
	}
	return out, nil
}

func cost(tokens, rate int64) (int64, error) {
	if tokens < 0 || rate < 0 || (rate > 0 && tokens > (math.MaxInt64-999999)/rate) {
		return 0, ErrBudget
	}
	return (tokens*rate + 999999) / 1_000_000, nil
}

func encode(v any) string { b, _ := json.Marshal(v); return string(b) }
