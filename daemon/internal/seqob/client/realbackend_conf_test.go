package client

// realbackend_conf_test.go — the taker's confidentiality posture follows its
// coins. A confidential input can only balance against a blinded output, so a
// proposal that spends one must blind the taker's receive and change outputs
// even when the wallet was started explicit; an explicit coin stays explicit.

import (
	"bytes"
	"strings"
	"testing"

	"github.com/aejkcs50/seqdex/daemon/pkg/explorer"
	"github.com/aejkcs50/seqdex/daemon/pkg/explorer/esplora"
	"github.com/aejkcs50/seqdex/daemon/pkg/seqnet"
)

const (
	confPayAsset  = "c8eccacf0953e1931cd31e434d8319101cc36e6c38b0e2104d8687552fae3e40"
	confRecvAsset = "2a515539da5e6a60caa7766ecd65bac0c10d15717ddd2088844ba58f4d04b9de"
)

func confTestBackend(t *testing.T, utxos []explorer.Utxo) *RealBackend {
	t.Helper()
	net := seqnet.SequentiaTestnet
	b := NewRealBackend(&net, bytes.Repeat([]byte{7}, 32), bytes.Repeat([]byte{9}, 32))
	b.FetchUtxos = func(addr string, _ [][]byte) ([]explorer.Utxo, error) { return utxos, nil }
	return b
}

func takerScript(b *RealBackend) []byte {
	s, _ := b.taker.Script()
	return s
}

// A revealed confidential coin (commitments + the blinders the taker unblinded).
func confidentialCoin(script []byte, value uint64) explorer.Utxo {
	commit := "08" + strings.Repeat("ab", 32)
	return esplora.NewWitnessUtxo(strings.Repeat("11", 32), 0, value, confPayAsset,
		commit, "0a"+strings.Repeat("cd", 32),
		bytes.Repeat([]byte{1}, 32), bytes.Repeat([]byte{2}, 32),
		script, bytes.Repeat([]byte{3}, 33), []byte{4, 4}, []byte{5, 5}, true)
}

func TestProposalBlindsOutputsWhenSpendingConfidentialCoins(t *testing.T) {
	b := confTestBackend(t, nil)
	b.FetchUtxos = func(string, [][]byte) ([]explorer.Utxo, error) {
		return []explorer.Utxo{confidentialCoin(takerScript(b), 1_000_000)}, nil
	}
	req := ProposalReq{PayAsset: confPayAsset, PayAmount: 400_000, RecvAsset: confRecvAsset, RecvAmount: 10}
	out, err := b.ProposerBuildRequest(req, LegConfidentiality{}) // wallet started explicit
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if !psetHasConfidentialOutput(out.GetTransaction()) {
		t.Fatal("spending a confidential coin must blind the taker's outputs, or the tx can never balance")
	}
	if !requestIsConfidential(out) {
		t.Fatal("the request must carry the input blinders so the maker blinds its half")
	}
}

func TestProposalStaysExplicitOnExplicitCoins(t *testing.T) {
	b := confTestBackend(t, nil)
	b.FetchUtxos = func(string, [][]byte) ([]explorer.Utxo, error) {
		return []explorer.Utxo{esplora.NewUnconfidentialWitnessUtxo(strings.Repeat("22", 32), 1, 1_000_000, confPayAsset, takerScript(b))}, nil
	}
	req := ProposalReq{PayAsset: confPayAsset, PayAmount: 400_000, RecvAsset: confRecvAsset, RecvAmount: 10}
	out, err := b.ProposerBuildRequest(req, LegConfidentiality{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if psetHasConfidentialOutput(out.GetTransaction()) {
		t.Fatal("an explicit wallet spending explicit coins must not blind (transparent by default)")
	}
}
