package state

import (
	"testing"
	"time"
)

func TestTrimLogsPreferCritical(t *testing.T) {
	base := time.Now().Add(-time.Hour)
	logs := make([]ActionLog, 0, 30)
	// oldest noise
	for i := 0; i < 10; i++ {
		logs = append(logs, ActionLog{At: base.Add(time.Duration(i) * time.Second), Action: "heal_summary", Auth: ""})
	}
	// critical cool
	for i := 0; i < 10; i++ {
		logs = append(logs, ActionLog{At: base.Add(time.Duration(10+i) * time.Second), Action: "cooldown", Auth: "a"})
	}
	// more noise
	for i := 0; i < 10; i++ {
		logs = append(logs, ActionLog{At: base.Add(time.Duration(20+i) * time.Second), Action: "heal_summary"})
	}
	out := trimLogsPreferCritical(logs, 15)
	if len(out) != 15 {
		t.Fatalf("want 15, got %d", len(out))
	}
	cool := 0
	for _, e := range out {
		if e.Action == "cooldown" {
			cool++
		}
	}
	if cool < 8 {
		t.Fatalf("expected most cooldown retained, got cool=%d out=%v", cool, actionsOf(out))
	}
}

func actionsOf(logs []ActionLog) []string {
	out := make([]string, len(logs))
	for i, e := range logs {
		out[i] = e.Action
	}
	return out
}
