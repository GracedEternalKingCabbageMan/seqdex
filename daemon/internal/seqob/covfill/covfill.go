// Package covfill is the SINGLE-FILL taker driver for the SeqOB passive-CLOB
// covenant: it turns one resting covenant order (a tapscript-locked on-chain
// UTXO ANYONE may fill) into a broadcast FILL transaction funded and signed by
// an elementsd wallet.
//
// Where it sits among the existing covenant consumers:
//
//   - pkg/covenant owns the byte-exact primitive (leaf, taptree, PlanFill) and
//     stays pure — no RPC, no wallet. This package only CONSUMES it.
//   - internal/seqob/settler assembles the JOINT covenant-vs-covenant fill (two
//     covenant inputs, settler fee input); its per-piece recipe (fixed 2k/2k+1
//     output map, explicit-only outputs, sign-then-inject-witness order against
//     elementsd) is reused here for the single-fill shape: ONE covenant input at
//     index 0 plus the taker's own wallet-funded asset-B / fee inputs.
//   - the web wallet fills covenants live through the SWK wasm assembler
//     (SWK/lwk_wollet/src/seqob_covenant.rs build_covenant_fill_tx); this driver
//     is that assembler's Go port, byte-compatible in layout:
//
//     input  0 : the covenant UTXO — witness [FILL leaf, control block], NO sig
//     input 1+ : the taker wallet's asset-B (and fee-asset) funding UTXOs
//     output 0 : maker credit  — asset B, >= ceil price, spk OP_1 <maker_prog>
//     output 1 : PARTIAL -> the self-replicating remainder covenant (asset A)
//     FULL    -> a NON-asset-A explicit "gap" output (change or fee):
//     the leaf inspects slot 2k+1 and reads an asset-A output
//     there as an underpaid remainder, so a full fill must park
//     something else in the slot (same rule the settler's gap
//     outputs and the wasm assembler implement)
//     output 2 : the taker's asset-A receipt (the filled coins)
//     then the remaining change outputs and the explicit fee output.
//
// Trust model: identical to the settler's. Every maker-facing output is fixed by
// the maker's own FILL leaf and re-checked by the script interpreter, so a wrong
// assembly can only produce a tx the chain REJECTS — never a theft. The driver
// still fails closed before broadcast (trustless spk verification, on-chain UTXO
// state, min-lot floors, and a mandatory testmempoolaccept) so it never ships a
// spend the mempool would bounce.
//
// A covenant fill is ONE atomic transaction: there is no counterparty leg, no
// refund branch and no session state to resume, so the driver keeps no state
// file — on any failure before broadcast nothing moved, and after broadcast the
// tx either confirms or is dropped whole.
package covfill

import (
	"encoding/hex"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/aejkcs50/seqdex/daemon/pkg/covenant"
	"github.com/vulpemventures/go-elements/elementsutil"
	"github.com/vulpemventures/go-elements/transaction"
)

// DefaultDustFold is the atom threshold below which a leftover change would be
// dust: credit-asset dust is folded into the maker credit (the leaf enforces
// only a MINIMUM, so overpaying is always valid) and fee-asset dust into the
// fee, mirroring the settler's dustFold so no dust output is ever emitted.
const DefaultDustFold = 1000

// Node is the wallet-scoped elementsd JSON-RPC surface the driver needs
// (satisfied by xchain.(*RPC) with a wallet path; scripted by tests).
type Node interface {
	Call(out interface{}, method string, params ...interface{}) error
}

// Params configures one covenant fill.
type Params struct {
	// Order is the covenant's terms (from the relay's CovenantTerms via
	// bridge.OrderFromTerms). Asset ids/keys are INTERNAL byte order.
	Order covenant.Order
	// Txid/Vout locate the funded covenant UTXO being filled.
	Txid string
	Vout uint32
	// TakeAtoms is the asset-A atoms to take; 0 = the whole covenant. A take
	// leaving a remainder in (0, min_lot) is SHRUNK to locked-min_lot (the
	// largest fill leaving a valid remainder), exactly as the crosser's
	// ClampOverlap and the web's planFill dust rule size it.
	TakeAtoms uint64
	// FeeAtoms/FeeAssetDisplay are the explicit network fee (open fee market:
	// any accepted asset, denominated in that asset's own atoms). The fee asset
	// must NOT be the covenant's sold asset A: the taker never funds asset A
	// (it comes from the covenant input), and an asset-A fee output could be
	// misread by the leaf at the gap slot.
	FeeAtoms        uint64
	FeeAssetDisplay string
	// Node is the wallet-scoped RPC used for funding, signing and broadcast.
	Node Node
	// DustFoldAtoms overrides DefaultDustFold when > 0.
	DustFoldAtoms uint64
	// AllowMakerCancellable opts in to filling a non-NUMS-internal-key order (a
	// maker-cancellable/key-path-rug covenant). Default false: fail closed.
	AllowMakerCancellable bool
}

// Result is a settled fill.
type Result struct {
	Txid      string
	RawHex    string
	Locked    uint64 // asset-A atoms the covenant held before this fill
	Filled    uint64 // asset-A atoms taken (after any dust-shrink)
	Remainder uint64 // asset-A atoms re-rested to the covenant (0 = full fill)
	PaidB     uint64 // asset-B atoms actually credited to the maker (>= ceil price)
	Partial   bool
}

// ShrinkTake applies the covenant dust-remainder rule to a requested take:
// take==0 means the whole covenant; a take leaving 0 < remainder < minLot is
// shrunk to locked-minLot (the largest fill with a valid remainder). It does NOT
// enforce the other floors (take >= minLot etc.) — covenant.Order.PlanFill
// re-validates everything and fails closed.
func ShrinkTake(locked, take, minLot uint64) uint64 {
	if take == 0 || take > locked {
		return locked
	}
	if rem := locked - take; rem > 0 && rem < minLot {
		return locked - minLot
	}
	return take
}

// FillCovenant verifies the resting covenant on-chain, assembles the FILL spend
// (wallet-funded), signs the wallet inputs via elementsd, injects the covenant's
// introspection-only witness, gates on testmempoolaccept and broadcasts.
// It fails closed on ANY constraint mismatch — a spend the chain or mempool
// would reject is never broadcast.
func FillCovenant(p Params) (*Result, error) {
	if p.Node == nil {
		return nil, fmt.Errorf("no node RPC")
	}
	dust := p.DustFoldAtoms
	if dust == 0 {
		dust = DefaultDustFold
	}

	// --- static constraint checks (fail closed before any I/O) --------------
	if p.Order.AssetA == p.Order.AssetB {
		return nil, fmt.Errorf("degenerate covenant: sold asset A equals payment asset B")
	}
	assetADisplay := displayHex(p.Order.AssetA)
	assetBDisplay := displayHex(p.Order.AssetB)
	feeDisplay := strings.ToLower(p.FeeAssetDisplay)
	if b, err := hex.DecodeString(feeDisplay); err != nil || len(b) != 32 {
		return nil, fmt.Errorf("fee asset must be 32-byte display hex (got %q)", p.FeeAssetDisplay)
	}
	if feeDisplay == assetADisplay {
		return nil, fmt.Errorf("fee asset must not be the covenant's sold asset A (%s)", assetADisplay)
	}
	if p.FeeAtoms == 0 {
		return nil, fmt.Errorf("fee must be > 0 atoms")
	}

	// --- read + trustlessly verify the covenant UTXO at the live tip --------
	locked, onchainSPK, err := verifyCovenantUTXO(p, assetADisplay)
	if err != nil {
		return nil, err
	}
	tap, err := p.Order.VerifyAgainstSPK(onchainSPK, p.AllowMakerCancellable)
	if err != nil {
		return nil, fmt.Errorf("covenant %s:%d fails trustless verify: %w", p.Txid, p.Vout, err)
	}
	_ = tap // PlanFill re-derives; the verify above is the gate

	// --- size the fill (dust-shrink, then the covenant's own floors) --------
	take := ShrinkTake(locked, p.TakeAtoms, p.Order.MinLot)
	plan, err := p.Order.PlanFill(locked, take, 0)
	if err != nil {
		return nil, fmt.Errorf("fill plan: %w", err)
	}

	// --- select the taker's funding (credit asset B + the fee asset) --------
	need := map[string]uint64{}
	need[assetBDisplay] += plan.RequiredB
	need[feeDisplay] += p.FeeAtoms
	sel, err := selectFunding(p.Node, need, p.Txid, p.Vout, assetADisplay)
	if err != nil {
		return nil, err
	}

	// Per-asset change, with dust folded where it cannot hurt: credit-asset dust
	// overpays the maker (the leaf enforces value >= required, never ==), fee-
	// asset dust raises the fee. Both stay the taker's own money to give away.
	creditValue := plan.RequiredB
	feeValue := p.FeeAtoms
	var bChange, feeChange uint64
	if assetBDisplay == feeDisplay {
		change := sel.total[assetBDisplay] - creditValue - feeValue
		if change > 0 && change < dust {
			feeValue += change
		} else {
			bChange = change
		}
	} else {
		if change := sel.total[assetBDisplay] - creditValue; change > 0 && change < dust {
			creditValue += change
		} else {
			bChange = change
		}
		if change := sel.total[feeDisplay] - feeValue; change > 0 && change < dust {
			feeValue += change
		} else {
			feeChange = change
		}
	}

	// --- assemble the FILL transaction --------------------------------------
	rawHex, err := assembleFill(p, plan, sel, creditValue, feeValue, bChange, feeChange,
		assetADisplay, assetBDisplay, feeDisplay)
	if err != nil {
		return nil, err
	}

	// Sign the wallet inputs. The covenant input is not the wallet's, so
	// complete=false is expected; its witness is injected next (the settler's
	// proven sign-then-inject order).
	var signed struct {
		Hex      string `json:"hex"`
		Complete bool   `json:"complete"`
	}
	if err := p.Node.Call(&signed, "signrawtransactionwithwallet", rawHex); err != nil {
		return nil, fmt.Errorf("signrawtransactionwithwallet: %w", err)
	}
	if signed.Hex == "" {
		return nil, fmt.Errorf("signrawtransactionwithwallet returned empty hex")
	}
	ftx, err := transaction.NewTxFromHex(signed.Hex)
	if err != nil {
		return nil, fmt.Errorf("parse signed tx: %w", err)
	}
	if len(ftx.Inputs) < 1 {
		return nil, fmt.Errorf("signed tx has no inputs")
	}
	ftx.Inputs[0].Witness = plan.Witness()
	finalHex, err := ftx.ToHex()
	if err != nil {
		return nil, fmt.Errorf("serialize final tx: %w", err)
	}

	// --- mempool gate, then broadcast ----------------------------------------
	var accept []struct {
		Allowed      bool   `json:"allowed"`
		RejectReason string `json:"reject-reason"`
	}
	if err := p.Node.Call(&accept, "testmempoolaccept", []string{finalHex}); err != nil {
		return nil, fmt.Errorf("testmempoolaccept: %w", err)
	}
	if len(accept) == 0 || !accept[0].Allowed {
		reason := "unknown"
		if len(accept) > 0 && accept[0].RejectReason != "" {
			reason = accept[0].RejectReason
		}
		return nil, fmt.Errorf("refusing to broadcast: node would reject the FILL spend (%s)", reason)
	}
	var txid string
	if err := p.Node.Call(&txid, "sendrawtransaction", finalHex); err != nil {
		return nil, fmt.Errorf("sendrawtransaction: %w", err)
	}
	return &Result{
		Txid:      txid,
		RawHex:    finalHex,
		Locked:    locked,
		Filled:    plan.Filled,
		Remainder: plan.Remainder,
		PaidB:     creditValue,
		Partial:   plan.Partial,
	}, nil
}

// verifyCovenantUTXO reads the covenant outpoint at the live tip (gettxout is
// mempool-aware and returns null once spent) and checks it is unspent, explicit,
// and funded with the advertised sold asset. Returns the locked atoms and the
// on-chain scriptPubKey for the trustless spk equality check.
func verifyCovenantUTXO(p Params, assetADisplay string) (uint64, []byte, error) {
	var utxo *struct {
		Confirmations int64   `json:"confirmations"`
		Value         float64 `json:"value"`
		Asset         string  `json:"asset"`
		ScriptPubKey  struct {
			Hex string `json:"hex"`
		} `json:"scriptPubKey"`
	}
	if err := p.Node.Call(&utxo, "gettxout", p.Txid, p.Vout, true); err != nil {
		return 0, nil, fmt.Errorf("gettxout %s:%d: %w", p.Txid, p.Vout, err)
	}
	if utxo == nil {
		return 0, nil, fmt.Errorf("covenant %s:%d is not an unspent utxo (spent, reorged away, or never funded)", p.Txid, p.Vout)
	}
	if utxo.Asset == "" {
		return 0, nil, fmt.Errorf("covenant %s:%d has no explicit asset (blinded output cannot be a covenant)", p.Txid, p.Vout)
	}
	if !strings.EqualFold(utxo.Asset, assetADisplay) {
		return 0, nil, fmt.Errorf("covenant %s:%d holds asset %s, want sold-asset %s", p.Txid, p.Vout, utxo.Asset, assetADisplay)
	}
	spk, err := hex.DecodeString(utxo.ScriptPubKey.Hex)
	if err != nil {
		return 0, nil, fmt.Errorf("bad on-chain spk hex: %w", err)
	}
	locked := coinsToAtoms(utxo.Value)
	if locked == 0 {
		return 0, nil, fmt.Errorf("covenant %s:%d has zero locked value", p.Txid, p.Vout)
	}
	return locked, spk, nil
}

// funding is a per-asset selection of the taker wallet's own UTXOs.
type funding struct {
	inputs []fundUTXO
	total  map[string]uint64
}

type fundUTXO struct {
	txid  string
	vout  uint32
	atoms uint64
	asset string
}

// selectFunding picks spendable wallet UTXOs covering each needed asset amount:
// per asset, the smallest single UTXO that covers the target, else smallest-
// first accumulation (the settler's proven selection). The covenant outpoint and
// any asset-A coin are never candidates — the taker must not fund the sold asset
// (asset A comes entirely from the covenant input; the wasm assembler enforces
// the same).
func selectFunding(node Node, need map[string]uint64, covTxid string, covVout uint32, assetADisplay string) (*funding, error) {
	var raw []struct {
		Txid          string  `json:"txid"`
		Vout          uint32  `json:"vout"`
		Amount        float64 `json:"amount"`
		Asset         string  `json:"asset"`
		Spendable     bool    `json:"spendable"`
		Confirmations int     `json:"confirmations"`
	}
	if err := node.Call(&raw, "listunspent", 1); err != nil {
		return nil, fmt.Errorf("listunspent: %w", err)
	}
	byAsset := map[string][]fundUTXO{}
	for _, u := range raw {
		a := strings.ToLower(u.Asset)
		if !u.Spendable || a == assetADisplay {
			continue
		}
		if u.Txid == covTxid && u.Vout == covVout {
			continue // never re-select the covenant itself
		}
		if _, wanted := need[a]; !wanted {
			continue
		}
		byAsset[a] = append(byAsset[a], fundUTXO{txid: u.Txid, vout: u.Vout, atoms: coinsToAtoms(u.Amount), asset: a})
	}

	sel := &funding{total: map[string]uint64{}}
	// Deterministic asset order for a stable input layout (tests + logs).
	assets := make([]string, 0, len(need))
	for a := range need {
		assets = append(assets, a)
	}
	sort.Strings(assets)
	for _, asset := range assets {
		target := need[asset]
		if target == 0 {
			continue
		}
		cands := byAsset[asset]
		sort.Slice(cands, func(i, j int) bool { return cands[i].atoms < cands[j].atoms })
		picked, sum := pickCoins(cands, target)
		if sum < target {
			return nil, fmt.Errorf("wallet has only %d atoms of %s across %d spendable utxo(s), need %d",
				sum, asset, len(cands), target)
		}
		sel.inputs = append(sel.inputs, picked...)
		sel.total[asset] += sum
	}
	return sel, nil
}

func pickCoins(cands []fundUTXO, target uint64) ([]fundUTXO, uint64) {
	for _, u := range cands { // smallest single cover (minimal change)
		if u.atoms >= target {
			return []fundUTXO{u}, u.atoms
		}
	}
	var picked []fundUTXO
	var sum uint64
	for _, u := range cands {
		picked = append(picked, u)
		sum += u.atoms
		if sum >= target {
			break
		}
	}
	return picked, sum
}

// assembleFill lays out the raw FILL transaction: covenant input 0, wallet
// funding inputs, and the consensus-fixed output order the wasm assembler pins
// (credit at 0, remainder/gap at 1, receipt, changes, fee).
func assembleFill(p Params, plan *covenant.FillPlan, sel *funding,
	creditValue, feeValue, bChange, feeChange uint64,
	assetADisplay, assetBDisplay, feeDisplay string) (string, error) {

	if len(p.Order.MakerProg) != 32 || p.Order.MakerVer != 1 {
		return "", fmt.Errorf("maker payout must be a v1 taproot program (32 bytes)")
	}
	creditSPK := append([]byte{0x51, 0x20}, p.Order.MakerProg...) // OP_1 <prog>

	receiptSPK, err := freshUnconfSPK(p.Node)
	if err != nil {
		return "", fmt.Errorf("receipt spk: %w", err)
	}
	var changeSPK []byte
	if bChange > 0 || feeChange > 0 {
		if changeSPK, err = freshUnconfSPK(p.Node); err != nil {
			return "", fmt.Errorf("change spk: %w", err)
		}
	}

	commitA, err := elementsutil.AssetHashToBytes(assetADisplay)
	if err != nil {
		return "", fmt.Errorf("asset A: %w", err)
	}
	commitB, err := elementsutil.AssetHashToBytes(assetBDisplay)
	if err != nil {
		return "", fmt.Errorf("asset B: %w", err)
	}
	commitFee, err := elementsutil.AssetHashToBytes(feeDisplay)
	if err != nil {
		return "", fmt.Errorf("fee asset: %w", err)
	}

	out := func(commit []byte, atoms uint64, spk []byte) (*transaction.TxOutput, error) {
		v, verr := elementsutil.ValueToBytes(atoms)
		if verr != nil {
			return nil, verr
		}
		return transaction.NewTxOutput(commit, v, spk), nil
	}

	creditOut, err := out(commitB, creditValue, creditSPK)
	if err != nil {
		return "", err
	}
	receiptOut, err := out(commitA, plan.Filled, receiptSPK)
	if err != nil {
		return "", err
	}
	feeOut, err := out(commitFee, feeValue, []byte{})
	if err != nil {
		return "", err
	}
	var bChangeOut, feeChangeOut *transaction.TxOutput
	if bChange > 0 {
		if bChangeOut, err = out(commitB, bChange, changeSPK); err != nil {
			return "", err
		}
	}
	if feeChange > 0 {
		if feeChangeOut, err = out(commitFee, feeChange, changeSPK); err != nil {
			return "", err
		}
	}

	// Output order — byte-compatible with build_covenant_fill_tx:
	//   PARTIAL: [credit, remainder(A->covenant spk), receipt(A), B-change?, fee-change?, fee]
	//   FULL:    [credit, <non-A gap>, receipt(A), <rest>, fee]
	//     gap preference: B-change, else fee-asset change, else the fee output
	//     itself (fee asset != A is enforced above, so the leaf reads remainder 0).
	var outs []*transaction.TxOutput
	outs = append(outs, creditOut)
	switch {
	case plan.Partial:
		remOut, rerr := out(commitA, plan.Remainder, plan.OrderSPK)
		if rerr != nil {
			return "", rerr
		}
		outs = append(outs, remOut, receiptOut)
		if bChangeOut != nil {
			outs = append(outs, bChangeOut)
		}
		if feeChangeOut != nil {
			outs = append(outs, feeChangeOut)
		}
		outs = append(outs, feeOut)
	case bChangeOut != nil:
		outs = append(outs, bChangeOut, receiptOut)
		if feeChangeOut != nil {
			outs = append(outs, feeChangeOut)
		}
		outs = append(outs, feeOut)
	case feeChangeOut != nil:
		outs = append(outs, feeChangeOut, receiptOut, feeOut)
	default:
		outs = append(outs, feeOut, receiptOut) // fee output is the non-A gap
	}

	tx := transaction.NewTx(2)
	covHash, err := elementsutil.TxIDToBytes(p.Txid)
	if err != nil {
		return "", fmt.Errorf("covenant txid %q: %w", p.Txid, err)
	}
	tx.AddInput(transaction.NewTxInput(covHash, p.Vout))
	for _, u := range sel.inputs {
		h, herr := elementsutil.TxIDToBytes(u.txid)
		if herr != nil {
			return "", fmt.Errorf("funding txid %q: %w", u.txid, herr)
		}
		tx.AddInput(transaction.NewTxInput(h, u.vout))
	}
	for _, o := range outs {
		tx.AddOutput(o)
	}
	return tx.ToHex()
}

// freshUnconfSPK returns a fresh UNBLINDED wallet scriptPubKey (the settler's
// getnewaddress + unconfidential-form pattern): every output the driver emits
// must be explicit or the covenant leaf cannot introspect the tx.
func freshUnconfSPK(node Node) ([]byte, error) {
	var addr string
	if err := node.Call(&addr, "getnewaddress"); err != nil {
		return nil, fmt.Errorf("getnewaddress: %w", err)
	}
	var info struct {
		Unconfidential string `json:"unconfidential"`
		ScriptPubKey   string `json:"scriptPubKey"`
	}
	if err := node.Call(&info, "getaddressinfo", addr); err != nil {
		return nil, fmt.Errorf("getaddressinfo: %w", err)
	}
	spkHex := info.ScriptPubKey
	if info.Unconfidential != "" && info.Unconfidential != addr {
		var uinfo struct {
			ScriptPubKey string `json:"scriptPubKey"`
		}
		if err := node.Call(&uinfo, "getaddressinfo", info.Unconfidential); err == nil && uinfo.ScriptPubKey != "" {
			spkHex = uinfo.ScriptPubKey
		}
	}
	if spkHex == "" {
		return nil, fmt.Errorf("no scriptPubKey for address %s", addr)
	}
	return hex.DecodeString(spkHex)
}

// displayHex converts a 32-byte internal-order asset id to its display hex.
func displayHex(internal [32]byte) string {
	return hex.EncodeToString(elementsutil.ReverseBytes(internal[:]))
}

// coinsToAtoms converts decimal coins to integer atoms (1e8 per coin).
func coinsToAtoms(v float64) uint64 {
	if v <= 0 {
		return 0
	}
	return uint64(math.Round(v * 1e8))
}
