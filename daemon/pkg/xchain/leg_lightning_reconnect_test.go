package xchain

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// fakeCLN is a minimal unix-socket JSON-RPC 2.0 server that answers exactly the
// methods ReconnectPeers drives (listpeers, connect). lnRPC.call dials a fresh
// connection per request, so it serves one request per accepted connection.
type fakeCLN struct {
	path       string
	ln         net.Listener
	peers      []fakePeer
	connectErr map[string]bool // peer id -> reply with a JSON-RPC error

	mu        sync.Mutex
	connectRC map[string]int // peer id -> times `connect` was invoked
}

type fakePeer struct {
	id        string
	connected bool
}

func startFakeCLN(t *testing.T, peers []fakePeer, connectErr map[string]bool) *fakeCLN {
	t.Helper()
	path := filepath.Join(t.TempDir(), "lightning-rpc")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	f := &fakeCLN{path: path, ln: ln, peers: peers, connectErr: connectErr, connectRC: map[string]int{}}
	go f.serve()
	t.Cleanup(func() { _ = ln.Close(); _ = os.Remove(path) })
	return f
}

func (f *fakeCLN) serve() {
	for {
		conn, err := f.ln.Accept()
		if err != nil {
			return
		}
		go f.handle(conn)
	}
}

func (f *fakeCLN) handle(conn net.Conn) {
	defer conn.Close()
	var req struct {
		ID     json.RawMessage        `json:"id"`
		Method string                 `json:"method"`
		Params map[string]interface{} `json:"params"`
	}
	if err := json.NewDecoder(conn).Decode(&req); err != nil {
		return
	}
	resp := map[string]interface{}{"jsonrpc": "2.0", "id": req.ID}
	switch req.Method {
	case "listpeers":
		f.mu.Lock()
		ps := make([]map[string]interface{}, 0, len(f.peers))
		for _, p := range f.peers {
			ps = append(ps, map[string]interface{}{"id": p.id, "connected": p.connected})
		}
		f.mu.Unlock()
		resp["result"] = map[string]interface{}{"peers": ps}
	case "connect":
		id, _ := req.Params["id"].(string)
		f.mu.Lock()
		f.connectRC[id]++
		wantErr := f.connectErr[id]
		f.mu.Unlock()
		if wantErr {
			resp["error"] = map[string]interface{}{"code": 402, "message": "could not connect"}
		} else {
			resp["result"] = map[string]interface{}{"id": id}
		}
	default:
		resp["result"] = map[string]interface{}{}
	}
	_ = json.NewEncoder(conn).Encode(resp)
}

func (f *fakeCLN) connectCount(id string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.connectRC[id]
}

// Only disconnected peers are reconnected, each exactly once; connected peers are
// left alone.
func TestReconnectPeers_OnlyDisconnected(t *testing.T) {
	f := startFakeCLN(t, []fakePeer{
		{id: "aa", connected: true},
		{id: "bb", connected: false},
		{id: "cc", connected: false},
	}, nil)

	n, err := NewCLNLNLeg(f.path).ReconnectPeers()
	if err != nil {
		t.Fatalf("ReconnectPeers: %v", err)
	}
	if n != 2 {
		t.Fatalf("reconnected = %d, want 2", n)
	}
	if c := f.connectCount("aa"); c != 0 {
		t.Errorf("already-connected peer aa reconnected %d times, want 0", c)
	}
	if c := f.connectCount("bb"); c != 1 {
		t.Errorf("bb connect count = %d, want 1", c)
	}
	if c := f.connectCount("cc"); c != 1 {
		t.Errorf("cc connect count = %d, want 1", c)
	}
}

// A peer with no known address (connect fails) does not stop the others from
// reconnecting, and the failure is surfaced (best-effort semantics).
func TestReconnectPeers_BestEffort(t *testing.T) {
	f := startFakeCLN(t, []fakePeer{
		{id: "bb", connected: false},
		{id: "cc", connected: false},
	}, map[string]bool{"bb": true})

	n, err := NewCLNLNLeg(f.path).ReconnectPeers()
	if n != 1 {
		t.Fatalf("reconnected = %d, want 1 (cc succeeds even though bb fails)", n)
	}
	if err == nil {
		t.Fatalf("want a surfaced error for the failed bb reconnect, got nil")
	}
}

// All peers already connected => nothing to do, no error (idempotent before every
// re-quote).
func TestReconnectPeers_AllConnected(t *testing.T) {
	f := startFakeCLN(t, []fakePeer{
		{id: "aa", connected: true},
		{id: "bb", connected: true},
	}, nil)

	n, err := NewCLNLNLeg(f.path).ReconnectPeers()
	if err != nil {
		t.Fatalf("ReconnectPeers: %v", err)
	}
	if n != 0 {
		t.Fatalf("reconnected = %d, want 0", n)
	}
}
