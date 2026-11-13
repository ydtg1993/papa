package breakerpolicy_test

import (
	"github.com/ydtg1993/papa/v4/config"
	"github.com/ydtg1993/papa/v4/internal/breaker"
	"github.com/ydtg1993/papa/v4/pkg/breakerpolicy"
	"testing"
)

var (
	_ config.BreakerPolicy = breakerpolicy.Policy{}
	_ breaker.Policy       = config.BreakerPolicy{}
	_ breakerpolicy.Policy = breaker.Policy{}
)

func TestPolicySharedDefaultsAndValidation(t *testing.T) {
	p := config.BreakerPolicy{}
	if breaker.EffectivePolicy(p) != breakerpolicy.EffectivePolicy(p) {
		t.Fatal("default policy changed")
	}
	p.Capacity = 1
	if breaker.ValidatePolicy(p) == nil || breakerpolicy.ValidatePolicy(p) == nil {
		t.Fatal("invalid capacity accepted")
	}
}
