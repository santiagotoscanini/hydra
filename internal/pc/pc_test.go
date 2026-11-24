package pc

import "testing"

func TestAProcessThatStoppedAnsweringIsNotStarting(t *testing.T) {
	start := "2026-11-24T01:37:17.33769-08:00"
	for _, tc := range []struct {
		name   string
		p      Process
		health Health
		says   string
	}{
		{"never ready yet", Process{Status: "Launched", IsReady: "Not Ready", StartTime: start, ReadyTime: ""},
			Starting, "starting"},
		{"zero ready time", Process{Status: "Running", IsReady: "Not Ready", StartTime: start, ReadyTime: "0001-01-01T00:00:00Z"},
			Starting, "starting"},
		{"ready before a restart", Process{Status: "Running", IsReady: "Not Ready", StartTime: start, ReadyTime: "2026-11-24T00:10:00-08:00"},
			Starting, "starting"},
		{"ready, then its check failed", Process{Status: "Launched", IsReady: "Not Ready", StartTime: start, ReadyTime: "2026-11-24T01:37:20.904759-08:00"},
			Failed, "stopped answering"},
		{"ready", Process{Status: "Launched", IsReady: "Ready", StartTime: start, ReadyTime: "2026-11-24T01:37:20.904759-08:00"},
			Up, "ready"},
	} {
		if got := tc.p.Health(); got != tc.health {
			t.Errorf("%s: health %v, want %v", tc.name, got, tc.health)
		}
		if got := tc.p.Describe(); got != tc.says {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.says)
		}
	}
}
