package breaker

import "github.com/ydtg1993/papa/v4/pkg/breakerpolicy"

type Policy = breakerpolicy.Policy

func EffectivePolicy(p Policy) Policy { return breakerpolicy.EffectivePolicy(p) }
func ValidatePolicy(p Policy) error   { return breakerpolicy.ValidatePolicy(p) }
