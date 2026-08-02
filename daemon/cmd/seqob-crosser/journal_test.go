package main

import (
	"path/filepath"
	"testing"
	"time"
)

func TestJournalCrashLeavesStrandedRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.json")

	j, stranded, err := OpenJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(stranded) != 0 {
		t.Fatalf("fresh journal has stranded records: %v", stranded)
	}
	rec := &ExecRecord{
		Pair: "a/b", StartedAt: time.Now(), Take: 10,
		First:  LegRecord{Family: "cross", Side: "bid", Status: "running", StateFile: "/tmp/x.json", Resume: "seqob-cli xrefund ..."},
		Second: LegRecord{Family: "pureln", Side: "ask", Status: "pending"},
	}
	if err := j.Begin("a/b", rec); err != nil {
		t.Fatal(err)
	}
	// Simulate a crash: reopen without End. The in-flight record must surface as
	// STRANDED (never assumed complete) and block the pair.
	j2, stranded, err := OpenJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(stranded) != 1 || stranded[0].First.StateFile != "/tmp/x.json" {
		t.Fatalf("stranded=%v", stranded)
	}
	if !j2.StrandedFor("a/b") || j2.StrandedFor("c/d") {
		t.Fatal("stranded pair blocking wrong")
	}
	if n := j2.ClearStranded(); n != 1 || j2.StrandedFor("a/b") {
		t.Fatalf("clear stranded: n=%d", n)
	}
}

func TestJournalDriftPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.json")
	j, _, err := OpenJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	rec := &ExecRecord{Pair: "a/b", StartedAt: time.Now()}
	if err := j.Begin("a/b", rec); err != nil {
		t.Fatal(err)
	}
	if err := j.End("a/b", map[string]int64{"aa": 100, "bb": -45}); err != nil {
		t.Fatal(err)
	}
	j2, stranded, err := OpenJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(stranded) != 0 {
		t.Fatalf("ended execution came back stranded: %v", stranded)
	}
	seed := j2.SeedDrift()
	if seed["aa"] != 100 || seed["bb"] != -45 {
		t.Fatalf("drift did not persist: %v", seed)
	}
}
