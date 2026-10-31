package pipeline

import (
	"testing"

	"github.com/jackc/pglogrepl"
)

// A commit directly after a heartbeat has the same LSN as the heartbeat, since
// a message's LSN is its record's end and a commit's is its record's start.
// Regression: that transaction was treated as already applied and lost.
func TestCommitRightAfterMessageIsNotSkipped(t *testing.T) {
	message := pglogrepl.LSN(0x70C1090)
	commitRightAfter := uint64(message)
	applied := uint64(messagePosition(message))
	if alreadyApplied(commitRightAfter, applied) {
		t.Fatalf("a commit at the message's LSN %s was treated as already applied", message)
	}
}

func TestRedeliveredCommitIsSkipped(t *testing.T) {
	commit := uint64(0x70C1000)
	if !alreadyApplied(commit, commit) {
		t.Fatal("a redelivered commit at the applied position was not skipped")
	}
	if !alreadyApplied(commit-8, commit) {
		t.Fatal("a redelivered earlier commit was not skipped")
	}
}

func TestMessagePositionPassesEarlierCommits(t *testing.T) {
	earlier := uint64(0x70C1000)
	message := pglogrepl.LSN(earlier + 0x40)
	if !alreadyApplied(earlier, uint64(messagePosition(message))) {
		t.Fatal("a commit before the message must count as applied once the message is flushed")
	}
}
