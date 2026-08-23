package main

// journal.go — the crash journal. One JSON file, written atomically (tmp +
// rename), holding: the settled drift ledger, every in-flight execution, and any
// STRANDED execution found at startup. The journal does NOT try to be the
// drivers' recovery state — each shelled driver owns its own per-session state
// file (xlift-/xsell-/xsubas-…-session.json) and refund command; the journal
// records WHICH driver state files belong to which crossing so a crash leaves an
// operator (or a restart) one honest list of what to resume/refund.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type LegRecord struct {
	Family    string `json:"family"`
	Side      string `json:"side"` // bid|ask
	OfferID   string `json:"offer_id"`
	Maker     string `json:"maker_pubkey"`
	Relay     string `json:"relay"`
	StateFile string `json:"state_file,omitempty"` // the DRIVER's own recovery file
	Resume    string `json:"resume_hint,omitempty"`
	Status    string `json:"status"` // pending | running | settled | failed
}

type ExecRecord struct {
	Pair      string    `json:"pair"`
	StartedAt time.Time `json:"started_at"`
	Take      uint64    `json:"take_base_atoms"`
	Cost      uint64    `json:"cost_quote_atoms"`
	Revenue   uint64    `json:"revenue_quote_atoms"`
	First     LegRecord `json:"first"`
	Second    LegRecord `json:"second"`
	Note      string    `json:"note,omitempty"`
}

type journalState struct {
	Drift    map[string]int64       `json:"drift,omitempty"`
	InFlight map[string]*ExecRecord `json:"in_flight,omitempty"` // keyed by pair
	Stranded []*ExecRecord          `json:"stranded,omitempty"`  // in-flight found at startup
}

type Journal struct {
	mu   sync.Mutex
	path string
	st   journalState
}

// OpenJournal loads (or initializes) the journal. Any in-flight record found on
// disk predates this process: it is moved to Stranded — the crosser NEVER
// assumes a crashed execution completed — and the caller logs resume hints.
func OpenJournal(path string) (*Journal, []*ExecRecord, error) {
	j := &Journal{path: path, st: journalState{
		Drift:    map[string]int64{},
		InFlight: map[string]*ExecRecord{},
	}}
	b, err := os.ReadFile(path)
	if err == nil {
		if err := json.Unmarshal(b, &j.st); err != nil {
			return nil, nil, fmt.Errorf("journal %s is corrupt (%v); refusing to start over it — move it aside after recovering any listed state files", path, err)
		}
	} else if !os.IsNotExist(err) {
		return nil, nil, err
	}
	if j.st.Drift == nil {
		j.st.Drift = map[string]int64{}
	}
	var newlyStranded []*ExecRecord
	for _, rec := range j.st.InFlight {
		newlyStranded = append(newlyStranded, rec)
		j.st.Stranded = append(j.st.Stranded, rec)
	}
	j.st.InFlight = map[string]*ExecRecord{}
	if err := j.flushLocked(); err != nil {
		return nil, nil, err
	}
	return j, newlyStranded, nil
}

func (j *Journal) SeedDrift() map[string]int64 {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := map[string]int64{}
	for k, v := range j.st.Drift {
		out[k] = v
	}
	return out
}

// StrandedFor reports whether a pair has stranded state (execution stays blocked
// for that pair until the operator clears it — see -clear-stranded).
func (j *Journal) StrandedFor(pair string) bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	for _, r := range j.st.Stranded {
		if r.Pair == pair {
			return true
		}
	}
	return false
}

func (j *Journal) ClearStranded() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	n := len(j.st.Stranded)
	j.st.Stranded = nil
	_ = j.flushLocked()
	return n
}

func (j *Journal) Begin(pair string, rec *ExecRecord) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.st.InFlight[pair] = rec
	return j.flushLocked()
}

// Update rewrites the record (leg statuses) for a pair.
func (j *Journal) Update(pair string) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.flushLocked()
}

// SetLeg records one leg's outcome under the journal's lock and flushes. Records
// live inside the journal's map, so mutating them from a pair's goroutine while
// another pair's End marshals the map was a data race (a torn string in the file).
func (j *Journal) SetLeg(pair string, second bool, status, stateFile, resume string) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	rec := j.st.InFlight[pair]
	if rec == nil {
		return nil
	}
	leg := &rec.First
	if second {
		leg = &rec.Second
	}
	leg.Status = status
	if stateFile != "" {
		leg.StateFile = stateFile
	}
	if resume != "" {
		leg.Resume = resume
	}
	return j.flushLocked()
}

// End closes a pair's in-flight record, folding `drift` (nil for a clean
// outcome) into the settled ledger.
func (j *Journal) End(pair string, drift map[string]int64) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	delete(j.st.InFlight, pair)
	for k, v := range drift {
		j.st.Drift[k] += v
	}
	return j.flushLocked()
}

func (j *Journal) flushLocked() error {
	b, err := json.MarshalIndent(&j.st, "", "  ")
	if err != nil {
		return err
	}
	tmp := j.path + ".tmp"
	if err := os.MkdirAll(filepath.Dir(j.path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, j.path)
}
