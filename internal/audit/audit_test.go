package audit

import (
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestEventsPerKindAreCappedAndSummarised(t *testing.T) {
	l := New(nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	at := time.Unix(1000, 0)
	for i := 0; i < 1000; i++ {
		l.Event(Event{At: at, Kind: "blocked", Severity: "P1", Detail: "x"})
	}
	l.Event(Event{At: at, Kind: "other", Severity: "P1", Detail: "y"})
	if len(l.events) != eventsPerSecond+1 {
		t.Fatalf("stored %d, want %d", len(l.events), eventsPerSecond+1)
	}
	// The next second reports how many the first one dropped.
	l.Event(Event{At: at.Add(time.Second), Kind: "other", Severity: "P1", Detail: "z"})
	found := false
	for _, e := range l.events {
		if e.Kind == "blocked" && strings.Contains(e.Detail, "995 more") {
			found = true
		}
	}
	if !found {
		t.Error("the dropped count was lost")
	}
}
