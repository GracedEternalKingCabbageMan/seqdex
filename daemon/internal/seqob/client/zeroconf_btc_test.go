package client

import "testing"

// A RAIL-CROSSING TAKE MUST NOT COST A BITCOIN BLOCK.
//
// Rail-blind matching means the best-priced offer wins whatever rail it rests on, and
// the bridge absorbs the difference. It must not hand the block time back to the taker:
// paying over Lightning against a best-priced ON-CHAIN maker should still be instant.
//
// The only thing making it slow was MinBTCConf, which every driver coerced:
//
//	if p.MinBTCConf <= 0 { p.MinBTCConf = 1 }
//
// so "accept from the mempool" was inexpressible and every crossing take waited ~10
// minutes for a confirmation — while the taker's own Lightning payment was already held.
//
// btcLegConfirmedHeight is the gate; at minConf 0 it must return immediately for an
// unconfirmed tx rather than erroring.
type confOps struct {
	confs int
	tip   int64
}

func (o confOps) BtcConfirmations(string) (int, error) { return o.confs, nil }
func (o confOps) BtcTip() (int64, error)               { return o.tip, nil }

func TestZeroConfAcceptsAMempoolLeg(t *testing.T) {
	ops := confOps{confs: 0, tip: 146_400}
	h, confs, err := btcLegConfirmedHeight(ops, "tx", 0)
	if err != nil {
		t.Fatalf("0-conf must accept an unconfirmed leg, got %v", err)
	}
	if confs != 0 {
		t.Fatalf("confs = %d, want 0", confs)
	}
	// An unconfirmed leg will confirm no earlier than the next block.
	if h != ops.tip+1 {
		t.Fatalf("height = %d, want tip+1 = %d", h, ops.tip+1)
	}
}

func TestOneConfStillWaitsForAConfirmation(t *testing.T) {
	ops := confOps{confs: 0, tip: 146_400}
	if _, _, err := btcLegConfirmedHeight(ops, "tx", 1); err == nil {
		t.Fatal("minConf 1 must still refuse an unconfirmed leg")
	}
	confirmed := confOps{confs: 1, tip: 146_400}
	if _, _, err := btcLegConfirmedHeight(confirmed, "tx", 1); err != nil {
		t.Fatalf("a 1-conf leg must satisfy minConf 1: %v", err)
	}
}
