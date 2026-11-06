package papa

import "github.com/ydtg1993/papa/v3/engine"

type RuleFetcher = engine.RuleFetcher
type RuleFetcherConfig = engine.RuleFetcherConfig
type RuleRef = engine.RuleRef
type RuleRequest = engine.RuleRequest
type RuleLink = engine.RuleLink
type RulePage = engine.RulePage

var NewRuleFetcher = engine.NewRuleFetcher

type RuleSchedule = engine.RuleSchedule
type RuleScheduleConfig = engine.RuleScheduleConfig
type RuleScheduleStatus = engine.RuleScheduleStatus

var NewRuleSchedule = engine.NewRuleSchedule
var ErrRuleScheduleInvalid = engine.ErrRuleScheduleInvalid
var ErrRuleScheduleCorrupt = engine.ErrRuleScheduleCorrupt
var ErrRuleScheduleConflict = engine.ErrRuleScheduleConflict
var ErrRuleScheduleUnconfirmed = engine.ErrRuleScheduleUnconfirmed
