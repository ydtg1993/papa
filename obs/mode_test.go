package obs

import "testing"

func TestResolveMode(t *testing.T) {
	cases := []struct {
		name       string
		configured string
		trigger    Trigger
		want       Mode
		wantErr    bool
	}{
		{"auto+periodic", "auto", TriggerPeriodic, ModeSnapshot, false},
		{"auto+exit", "auto", TriggerExit, ModeSnapshot, false},
		{"auto+cli", "auto", TriggerCLI, ModeSnapshot, false},
		{"empty+periodic", "", TriggerPeriodic, ModeSnapshot, false},
		{"empty+exit", "", TriggerExit, ModeSnapshot, false},
		{"empty+cli", "", TriggerCLI, ModeSnapshot, false},
		{"force snapshot cli", "snapshot", TriggerCLI, ModeSnapshot, false},
		{"force acceptance periodic", "acceptance", TriggerPeriodic, ModeAcceptance, false},
		{"force acceptance exit", "acceptance", TriggerExit, ModeAcceptance, false},
		{"force acceptance cli", "acceptance", TriggerCLI, ModeAcceptance, false},
		{"trim space", "  snapshot  ", TriggerExit, ModeSnapshot, false},
		{"bogus value", "weekly", TriggerCLI, "", true},
		{"unknown trigger", "auto", Trigger("cron"), "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolveMode(tc.configured, tc.trigger)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ResolveMode(%q,%q) 应报错，实得 %q", tc.configured, tc.trigger, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveMode(%q,%q) 报错: %v", tc.configured, tc.trigger, err)
			}
			if got != tc.want {
				t.Fatalf("ResolveMode(%q,%q) = %q, want %q", tc.configured, tc.trigger, got, tc.want)
			}
		})
	}
}
