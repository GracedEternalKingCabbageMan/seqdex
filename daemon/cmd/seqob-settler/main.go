// Command seqob-settler is the always-online, non-custodial SETTLER for the
// fully-passive SeqOB CLOB: two covenant-funded resting orders whose makers are
// BOTH offline are matched by the relay and settled against each other in ONE
// transaction that the settler assembles and broadcasts.
//
// It is a process distinct from either maker AND from the keyless relay
// (seqobd): the relay matches and advertises; the settler is the chain actor
// that funds the fee, assembles the joint FILL spend, and broadcasts. It never
// holds user funds and cannot alter a payout — the two covenants' FILL leaves
// enforce every maker credit, so the settler can only build a CONFORMING tx or
// none (see internal/seqob/settler and covenant.PlanJointSettlement).
//
// Subcommands:
//
//	plan   (reads a JSON cross on stdin)
//	    Emits the enforced joint-settlement recipe: per-covenant witnesses,
//	    the fixed credit/remainder output indices, each maker's credit value +
//	    ceil-price floor, and the reserved "gap" slots the settler must fill
//	    with a bitcoin output. feature_seqob_joint_covenant.py drives the
//	    on-chain broadcast from exactly this output, so a bug in the planner
//	    fails the regtest proof.
//
//	run    (documented) the always-online loop: subscribe to the relay's
//	    both-covenant matches, resolve each covenant's locked amount + the cross
//	    from the offers/UTXO set, then settler.Handle(match, …) to assemble +
//	    broadcast. Assembly/broadcast bind to an Elements node
//	    (sendrawtransaction); the enforcement core proven here is node-agnostic.
//
// All asset ids, programs and keys are INTERNAL byte order (32-byte hex),
// matching on-chain introspection (display-hex asset ids reversed).
package main

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/aejkcs50/seqdex/daemon/pkg/covenant"
)

func die(format string, a ...interface{}) {
	fmt.Fprintf(os.Stderr, "seqob-settler: "+format+"\n", a...)
	os.Exit(1)
}

// legInput is one maker's covenant terms + this settlement's fill of it.
type legInput struct {
	Num      uint64 `json:"num"`
	Den      uint64 `json:"den"`
	MinLot   uint64 `json:"minlot"`
	Prog     string `json:"prog"`     // 32-byte maker payout program hex
	MakerX   string `json:"makerx"`   // 32-byte hex
	Expiry   int64  `json:"expiry"`   // REFUND absolute-locktime height
	Internal string `json:"internal"` // optional 32-byte internal key hex; default NUMS
	Locked   uint64 `json:"locked"`   // atoms currently locked in the covenant UTXO
	Fill     uint64 `json:"fill"`     // atoms taken by this settlement
}

// planInput is the both-covenant cross the settler is asked to settle. order0
// sells asset X wanting Y; order1 sells Y wanting X.
type planInput struct {
	AssetX string   `json:"asset_x"`
	AssetY string   `json:"asset_y"`
	Order0 legInput `json:"order0"`
	Order1 legInput `json:"order1"`
}

func hx32(s, name string) [32]byte {
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 32 {
		die("%s must be 32-byte hex (got %q)", name, s)
	}
	var out [32]byte
	copy(out[:], b)
	return out
}

func order(assetA, assetB [32]byte, l legInput) covenant.Order {
	prog, err := hex.DecodeString(l.Prog)
	if err != nil || len(prog) != 32 {
		die("prog must be 32-byte hex")
	}
	o := covenant.Order{
		AssetA:         assetA,
		AssetB:         assetB,
		RateNum:        l.Num,
		RateDen:        l.Den,
		MakerProg:      prog,
		MakerVer:       1,
		MinLot:         l.MinLot,
		ExpiryLocktime: l.Expiry,
		MakerX:         hx32(l.MakerX, "makerx"),
		InternalKey:    covenant.NUMS,
	}
	if l.Internal != "" {
		o.InternalKey = hx32(l.Internal, "internal")
	}
	return o
}

func legOut(l covenant.CovLeg) map[string]interface{} {
	return map[string]interface{}{
		"input_index":     l.InputIndex,
		"credit_index":    l.CreditIndex,
		"credit_asset":    hex.EncodeToString(l.CreditAsset[:]),
		"credit_spk":      hex.EncodeToString(l.CreditSPK),
		"credit_value":    l.CreditValue,
		"credit_floor":    l.CreditFloor,
		"remainder_index": l.RemainderIndex,
		"partial":         l.Partial,
		"remainder":       l.Remainder,
		"remainder_asset": hex.EncodeToString(l.RemainderAsset[:]),
		"order_spk":       hex.EncodeToString(l.OrderSPK),
		"fill_leaf":       hex.EncodeToString(l.FillLeaf),
		"control_block":   hex.EncodeToString(l.ControlBlock),
	}
}

func main() {
	if len(os.Args) < 2 {
		die("usage: seqob-settler plan [< cross.json] | run [-relay ... -node-rpc host:port -fee-wallet NAME ...]")
	}
	switch os.Args[1] {
	case "plan":
		raw, err := io.ReadAll(os.Stdin)
		if err != nil {
			die("read stdin: %v", err)
		}
		var in planInput
		if err := json.Unmarshal(raw, &in); err != nil {
			die("parse cross json: %v", err)
		}
		x := hx32(in.AssetX, "asset_x")
		y := hx32(in.AssetY, "asset_y")
		orderA := order(x, y, in.Order0) // sells X wants Y
		orderB := order(y, x, in.Order1) // sells Y wants X
		s, err := covenant.PlanJointSettlement(
			orderA, in.Order0.Locked, in.Order0.Fill,
			orderB, in.Order1.Locked, in.Order1.Fill)
		if err != nil {
			die("plan: %v", err)
		}
		out, err := json.Marshal(map[string]interface{}{
			"min_outputs": s.MinOutputs,
			"gap_slots":   s.GapSlots,
			"leg0":        legOut(s.Leg0),
			"leg1":        legOut(s.Leg1),
		})
		if err != nil {
			die("marshal: %v", err)
		}
		fmt.Println(string(out))
	case "run":
		runSettler(os.Args[2:])
	default:
		die("unknown subcommand %q (want plan or run)", os.Args[1])
	}
}
