// Package watcher is the SeqOB chain-watcher: the safety layer that keeps the
// keyless relay's resting COVENANT order book consistent with actual Sequentia
// chain state.
//
// Why it exists. A covenant order is FUNDED on-chain — placing it broadcasts a
// taproot UTXO whose scriptPubKey is the {FILL, REFUND} taptree, and the signed
// offer resting on the relay merely ADVERTISES that outpoint. The relay
// (seqobd) is keyless and fundless and never watches the chain, so the book can
// drift from reality: a covenant can be FILLED while the maker is offline, a
// PARTIAL fill re-creates a smaller remainder covenant at a new outpoint, a fill
// can be broadcast and then dropped/RBF'd, and — because every Sequentia block
// references a Bitcoin header (first principle: Bitcoin anchoring is SUPREME) —
// a Bitcoin reorg can make the covenant's funding UTXO VANISH in real time. A
// resting order backed by a UTXO that no longer exists at the current tip is the
// dangerous case: a taker could try to fill a ghost.
//
// The watcher reconciles the book to whatever the node's CURRENT tip says. It is
// a distinct component from the relay (which stays keyless) and from the settler
// (which broadcasts fills). Its only powers over the book are the covenant-aware
// reconcile operations on offerstore.Store; it never holds keys or funds and it
// leans entirely on the node's own anchor-following (the node already reorgs
// when Bitcoin reorgs — the watcher consumes the resulting UTXO set, never a
// cached one).
//
// This file is the PURE classifier: given the on-chain reality of one covenant
// outpoint (CovState) and the covenant's expected program (Expect), it decides
// what the book must do (Verdict). It has no I/O and no node dependency, so it
// is unit-tested against synthetic UTXO states (classify_test.go). The RPC glue
// that fills a CovState lives in chainview_rpc.go; the loop that drives the book
// lives in watcher.go.
package watcher

import "strings"

// State is the reconciled classification of one resting covenant outpoint.
type State int

const (
	// StateLive: the advertised outpoint is UNSPENT in the current UTXO set and
	// matches the covenant program (spk + asset). Keep the order; reconcile its
	// active size to the on-chain value (correcting any optimistic match-time
	// decrement that never settled — i.e. re-opening a fill that never confirmed).
	StateLive State = iota
	// StatePartialFill: the outpoint was spent by a CONFIRMED fill that re-created
	// a smaller remainder covenant at index 2k+1. Re-rest the order pointing at the
	// new outpoint with the reduced size so the rest stays fillable.
	StatePartialFill
	// StateFilled: the outpoint was spent by a CONFIRMED fill (or refund) with NO
	// remainder covenant. Remove the order.
	StateFilled
	// StateUnconfirmedSpend: the outpoint is spent only in the mempool (fill not
	// yet anchored). Hold the order pending — do NOT declare it filled and do NOT
	// re-rest a remainder yet, because a dropped/RBF'd mempool spend must restore
	// the order on a later pass.
	StateUnconfirmedSpend
	// StateGhost: the funding outpoint does NOT exist at the current tip and no
	// spender was found — the funding was undone by a Bitcoin-driven reorg
	// (anchoring supremacy) or never confirmed, or the advertised outpoint never
	// was this covenant. Remove the order: it is backed by a UTXO that is gone.
	StateGhost
)

func (s State) String() string {
	switch s {
	case StateLive:
		return "LIVE"
	case StatePartialFill:
		return "PARTIAL_FILL"
	case StateFilled:
		return "FILLED"
	case StateUnconfirmedSpend:
		return "UNCONFIRMED_SPEND"
	case StateGhost:
		return "GHOST"
	default:
		return "UNKNOWN"
	}
}

// RemainderOut is a remainder covenant output the spender re-created for the
// outpoint under inspection (asset == asset_a, spk == the covenant program, at
// consensus index 2k+1 where k is the covenant input's index in the spender).
type RemainderOut struct {
	Present bool
	Vout    uint32
	Value   uint64 // asset-A atoms re-paid into the remainder covenant
}

// CovState is the on-chain reality of one covenant outpoint at the CURRENT tip,
// as the classifier needs it. ChainView.Inspect fills it from the live node; the
// classifier never reads a cache.
type CovState struct {
	// Unspent is true iff the advertised outpoint is in the node's current
	// UTXO set (gettxout at the live tip, mempool-aware).
	Unspent bool
	// Value/AssetDisplay/SPKHex describe the UTXO when Unspent. AssetDisplay is
	// the DISPLAY (byte-reversed) asset id hex that on-chain introspection
	// returns; SPKHex is the scriptPubKey hex.
	Value        uint64
	AssetDisplay string
	SPKHex       string
	// ConfirmedUTXO reports whether the unspent outpoint is confirmed (>=1 conf)
	// versus existing only as a mempool output (e.g. a not-yet-mined remainder).
	// Advisory; logged, not decisive.
	ConfirmedUTXO bool

	// Spent-branch fields, valid when !Unspent:
	SpenderTxid      string // the tx that spends the outpoint; "" if none found
	SpenderConfirmed bool   // the spender has >=1 confirmation
	Remainder        RemainderOut
}

// Expect is the covenant's expected program, derived from the resting offer's
// advertised CovenantTerms via pkg/covenant (never trusted from the advertised
// spk). The classifier validates the on-chain UTXO against it.
type Expect struct {
	OrderSPKHex   string // covenant scriptPubKey hex, from covenant.Order.Derive
	AssetADisplay string // display (reversed) asset-A id hex
	MinLot        uint64
}

// Verdict is the classifier's decision plus the data the book op needs.
type Verdict struct {
	State  State
	Reason string

	// StateLive:
	LiveSize uint64 // reconcile active size to this on-chain UTXO value

	// StatePartialFill:
	RemainderTxid string
	RemainderVout uint32
	RemainderSize uint64

	// StateFilled / StateUnconfirmedSpend:
	SpenderTxid string
}

// Classify decides what the book must do for one covenant outpoint given its
// on-chain reality (state) and expected program (exp). It is total and pure.
//
// The decision tree, in order:
//
//	UNSPENT at tip?
//	  yes → does the UTXO match the covenant program (spk + asset)?
//	          no  → GHOST (advertised outpoint is not this covenant; unsafe)
//	          yes → LIVE (reconcile active to the UTXO value)
//	  no  → is there a spender?
//	          no                 → GHOST (funding absent at tip: reorg / never-confirmed)
//	          yes, mempool-only  → UNCONFIRMED_SPEND (hold pending)
//	          yes, confirmed     → remainder re-created?
//	                                 yes → PARTIAL_FILL (re-rest remainder)
//	                                 no  → FILLED (remove)
func Classify(state CovState, exp Expect) Verdict {
	if state.Unspent {
		if !strings.EqualFold(state.SPKHex, exp.OrderSPKHex) ||
			!strings.EqualFold(state.AssetDisplay, exp.AssetADisplay) {
			return Verdict{
				State:  StateGhost,
				Reason: "unspent outpoint does not match the covenant program (spk/asset mismatch)",
			}
		}
		return Verdict{State: StateLive, LiveSize: state.Value}
	}

	// Spent or absent.
	if state.SpenderTxid == "" {
		return Verdict{
			State:  StateGhost,
			Reason: "funding outpoint absent from the UTXO set at the current tip and no spender found (Bitcoin-driven reorg or never confirmed)",
		}
	}
	if !state.SpenderConfirmed {
		return Verdict{
			State:       StateUnconfirmedSpend,
			Reason:      "outpoint spent only in mempool; fill not yet anchored",
			SpenderTxid: state.SpenderTxid,
		}
	}
	if state.Remainder.Present {
		return Verdict{
			State:         StatePartialFill,
			Reason:        "confirmed partial fill; remainder covenant re-created",
			RemainderTxid: state.SpenderTxid,
			RemainderVout: state.Remainder.Vout,
			RemainderSize: state.Remainder.Value,
		}
	}
	return Verdict{
		State:       StateFilled,
		Reason:      "confirmed full fill (no remainder covenant)",
		SpenderTxid: state.SpenderTxid,
	}
}
