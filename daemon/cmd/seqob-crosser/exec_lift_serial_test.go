package main

// exec_lift_serial_test.go — same-chain lifts spend from the keyed taker
// address, a single UTXO pool with no coin locking, so two lifts in flight at
// once select the same coins and the second broadcast dies
// bad-txns-inputs-missingorspent. The executor must run them one at a time while
// other leg kinds stay concurrent.

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeCLI writes a script that records its start and end to a log and sleeps.
func fakeCLI(t *testing.T, dir string) (bin, log string) {
	t.Helper()
	log = filepath.Join(dir, "calls.log")
	bin = filepath.Join(dir, "seqob-cli")
	script := "#!/bin/sh\necho \"start $1 $(date +%s%N)\" >> " + log + "\nsleep 0.3\necho \"end $1 $(date +%s%N)\" >> " + log + "\necho 'SWAP SETTLED'\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, log
}

func TestSameChainLiftsRunOneAtATime(t *testing.T) {
	dir := t.TempDir()
	bin, log := fakeCLI(t, dir)
	e := &Executor{
		Caps: Caps{SeqobCli: bin, StateDir: dir, Esplora: "http://e", TakerPriv: "aa", TakerBlinding: "bb", LegTimeout: 10 * time.Second},
		Logf: noLog,
	}
	n := norm(mkOffer(FamSameChain, SideAsk, tBase, tQuote, 100, 4000), tBase, tQuote)

	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if r := e.RunLeg(n, 10); !r.Settled {
				t.Errorf("lift not settled: %+v", r)
			}
		}()
	}
	wg.Wait()

	raw, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	// With the lifts serialised every "end" precedes the next "start".
	inFlight := 0
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		switch {
		case strings.HasPrefix(line, "start "):
			inFlight++
			if inFlight > 1 {
				t.Fatalf("two lifts in flight at once:\n%s", raw)
			}
		case strings.HasPrefix(line, "end "):
			inFlight--
		}
	}
	if strings.Count(string(raw), "start ") != 3 {
		t.Fatalf("expected 3 lifts, log:\n%s", raw)
	}
}
