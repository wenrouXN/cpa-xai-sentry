package state

import (
	"testing"
	"time"
)

func TestSnapshotLogsPageNewestFirst(t *testing.T) {
	s := New("")
	base := time.Date(2026, 7, 14, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	for i := 0; i < 5; i++ {
		s.Log(ActionLog{At: base.Add(time.Duration(i) * time.Minute), Action: "a", Auth: "x", Reason: string(rune('0' + i))})
	}
	page, total := s.SnapshotLogsPage(0, 2)
	if total != 5 {
		t.Fatalf("total=%d", total)
	}
	if len(page) != 2 {
		t.Fatalf("len=%d", len(page))
	}
	// newest first: reasons 4 then 3
	if page[0].Reason != "4" || page[1].Reason != "3" {
		t.Fatalf("page=%+v", page)
	}
	page2, _ := s.SnapshotLogsPage(2, 2)
	if page2[0].Reason != "2" || page2[1].Reason != "1" {
		t.Fatalf("page2=%+v", page2)
	}
	page3, _ := s.SnapshotLogsPage(4, 10)
	if len(page3) != 1 || page3[0].Reason != "0" {
		t.Fatalf("page3=%+v", page3)
	}
	empty, _ := s.SnapshotLogsPage(5, 10)
	if len(empty) != 0 {
		t.Fatalf("empty=%+v", empty)
	}
}
