#!/usr/bin/env python3
# Copyright (c) 2026 The Sequentia developers
# Distributed under the MIT software license.
"""Fully-passive SeqOB CLOB: TWO covenant-funded orders, BOTH makers offline,
matched and settled against each other in ONE transaction by an untrusted,
always-online SETTLER.

This is the endgame of the passive order book. feature_seqob_covenant_fill.py
proved one covenant filled by an online taker; feature_seqob_matcher_covenant.py
proved the relay auto-matching a covenant order to an online taker. Here NEITHER
party is online: maker-0 funds a SELL-X covenant, maker-1 funds a BUY-X (sell-Y)
covenant, both go offline, and the settler assembles + broadcasts the joint fill.

The settler is the PRODUCTION Go component (seqdex cmd/seqob-settler): given the
two covenant offers it computes the joint-fill recipe (covenant.PlanJointSettlement)
- the fixed 2k/2k+1 index map, each maker's credit + ceil-price floor, and the
reserved "gap" slots. This test drives the on-chain broadcast from exactly that
recipe, so a bug in the Go settler fails the proof.

Trust model proven here: the settler holds NO user funds and cannot steal. Every
maker credit (asset, scriptPubKey, min value) is fixed by that maker's own FILL
leaf; the settler only adds a bitcoin fee input and broadcasts. The rejection
scenarios show a settler that underpays a maker or redirects a credit to itself
produces a transaction the covenants refuse (interpreter code -26).

Scenarios:
  PASS   both-offline EXACT cross: one tx credits maker-0 in Y and maker-1 in X,
         neither maker signs, the settler adds only a fee input
  PASS   PARTIAL cross: the larger side's remainder re-rests as a valid covenant
  REJECT settler underpays maker-0 (below its ceil price)
  REJECT settler redirects maker-1's credit to itself (theft / credit aliasing)

Requires the Go toolchain and a configured, built Sequentia node checkout
(env GO_BIN / SEQUENTIA_DIR override the $HOME/dev-tools/go/bin/go and
$HOME/Sequentia defaults); see seqob_env.py. The settler
binary (seqob-settler) is built once per run.
"""

import json
import os
import subprocess
from decimal import Decimal

import seqob_env  # noqa: F401  (puts the node's test_framework on sys.path)
from test_framework.test_framework import BitcoinTestFramework, SkipTest
from test_framework.util import assert_equal, assert_raises_rpc_error, satoshi_round, BITCOIN_ASSET
from test_framework.key import compute_xonly_pubkey, generate_privkey
from test_framework.messages import (
    COIN, COutPoint, CTransaction, CTxIn, CTxInWitness, CTxOut, CTxOutAsset,
    CTxOutValue, tx_from_hex,
)
from test_framework.script import CScript, OP_1

import seqob_covenant as cov

FEE = 5000
GAP = 10000            # a settler-funded bitcoin gap output (fills a full-fill's 2k+1 slot)
MIN_LOT = 5 * COIN

# order-0 sells X wanting Y at 3 Y per X; order-1 sells Y wanting X at 1 X per 3 Y.
NUM0, DEN0 = 3, 1
NUM1, DEN1 = 1, 3


def go_bin():
    return seqob_env.go_bin()


def seqdex_dir():
    return seqob_env.seqdex_dir()


class SeqObJointCovenantTest(BitcoinTestFramework):

    def set_test_params(self):
        self.setup_clean_chain = True
        self.num_nodes = 1
        self.extra_args = [[
            "-initialfreecoins=2100000000000000",
            "-anyonecanspendaremine=1",
            "-blindedaddresses=0",
            "-validatepegin=0",
            "-con_parent_chain_signblockscript=51",
            "-con_any_asset_fees=1",
            "-maxtxfee=100.0",
            "-txindex=1",
        ]]

    def skip_test_if_missing_module(self):
        self.skip_if_no_wallet()
        # The Go daemons under test live in the seqdex repo, which a bare
        # checkout (CI included) does not have. Skip, rather than fail, when
        # the toolchain or the repo is absent: their absence says nothing
        # about the node.
        if not os.path.exists(go_bin()):
            raise SkipTest("Go toolchain not found at %s (set GO_BIN)" % go_bin())
        if not os.path.isdir(os.path.join(seqdex_dir(), "daemon")):
            raise SkipTest("seqdex daemon dir not found at %s (set SEQDEX_DIR)"
                           % os.path.join(seqdex_dir(), "daemon"))

    def setup_network(self, split=False):
        self.setup_nodes()

    # --- build the Go settler ----------------------------------------------

    def build_settler(self):
        go = go_bin()
        sdir = os.path.join(seqdex_dir(), "daemon")
        if not os.path.exists(go):
            raise RuntimeError("Go toolchain not found at %s (set GO_BIN)" % go)
        if not os.path.isdir(sdir):
            raise RuntimeError("seqdex daemon dir not found at %s (set SEQDEX_DIR)" % sdir)
        outdir = os.path.join(self.options.tmpdir, "seqob-bins")
        os.makedirs(outdir, exist_ok=True)
        env = dict(os.environ)
        env["PATH"] = os.path.dirname(go) + os.pathsep + env.get("PATH", "")
        out = os.path.join(outdir, "seqob-settler")
        self.log.info("building seqob-settler ...")
        subprocess.run([go, "build", "-o", out, "./cmd/seqob-settler"], cwd=sdir, env=env, check=True)
        return out

    def settler_plan(self, locked0, fill0, locked1, fill1):
        """Run the PRODUCTION Go settler to compute the joint-fill recipe for the
        both-offline cross. Its output drives the transaction the test broadcasts."""
        cross = {
            "asset_x": self.asset_x.hex(), "asset_y": self.asset_y.hex(),
            "order0": {"num": NUM0, "den": DEN0, "minlot": MIN_LOT,
                       "prog": self.maker0_x.hex(), "makerx": self.maker0_x.hex(),
                       "expiry": self.expiry, "locked": locked0, "fill": fill0},
            "order1": {"num": NUM1, "den": DEN1, "minlot": MIN_LOT,
                       "prog": self.maker1_x.hex(), "makerx": self.maker1_x.hex(),
                       "expiry": self.expiry, "locked": locked1, "fill": fill1},
        }
        p = subprocess.run([self.settler, "plan"], input=json.dumps(cross),
                           capture_output=True, text=True, check=True)
        return json.loads(p.stdout)

    # --- on-chain helpers ---------------------------------------------------

    def wallet_spk(self):
        addr = self.nodes[0].getnewaddress()
        unconf = self.nodes[0].getaddressinfo(addr)["unconfidential"]
        return bytes.fromhex(self.nodes[0].getaddressinfo(unconf)["scriptPubKey"])

    def asset_out(self, display_hex):
        return b"\x01" + bytes.fromhex(display_hex)[::-1]

    def ctxout(self, amount, spk, asset_out):
        return CTxOut(nValue=CTxOutValue(amount), scriptPubKey=spk, nAsset=CTxOutAsset(asset_out))

    def fresh_segwit_utxo(self, amount, asset_display=None):
        node = self.nodes[0]
        bech = node.getnewaddress("", "bech32")
        unconf = node.getaddressinfo(bech)["unconfidential"]
        if asset_display is None:
            # SEQUENTIA: an open-fee-market chain has no default fee asset.
            node.sendtoaddress(address=unconf, amount=amount,
                               fee_asset_label=BITCOIN_ASSET)
            target = BITCOIN_ASSET
        else:
            node.sendtoaddress(address=unconf, amount=amount, assetlabel=asset_display,
                               fee_asset_label=BITCOIN_ASSET)
            target = asset_display
        self.generate(node, 1)
        for u in node.listunspent():
            if (u["asset"] == target and abs(float(u["amount"]) - amount) < 1e-9
                    and u["scriptPubKey"].startswith("0014") and u["spendable"]):
                return u
        raise AssertionError("no fresh segwit utxo for %s" % target)

    def find_asset_utxo(self, asset_display, min_units):
        for u in self.nodes[0].listunspent():
            if u["asset"] == asset_display and float(u["amount"]) >= min_units and u["spendable"]:
                return u
        raise AssertionError("no wallet utxo of %s" % asset_display)

    def fund_covenant(self, order_tap, asset_display, asset_out, units):
        """Fund ONE covenant order UTXO (`units` whole units of the asset paying
        the covenant scriptPubKey). Returns (txid, vout, atoms)."""
        node = self.nodes[0]
        amt = units * COIN
        # Source the asset from a FRESH explicit (unconfidential) utxo: prior
        # scenarios leave blinded asset change in the wallet, and mixing a blinded
        # input with explicit outputs in a hand-built tx does not balance.
        a_utxo = self.fresh_segwit_utxo(units + 1, asset_display)
        a_in = int(satoshi_round(a_utxo["amount"]) * COIN)
        btc = self.fresh_segwit_utxo(1)
        btc_in = int(satoshi_round(btc["amount"]) * COIN)

        tx = CTransaction()
        tx.nVersion = 2
        tx.vin.append(CTxIn(COutPoint(int(a_utxo["txid"], 16), a_utxo["vout"])))
        tx.vin.append(CTxIn(COutPoint(int(btc["txid"], 16), btc["vout"])))
        order_spk = bytes(order_tap.scriptPubKey)
        tx.vout.append(self.ctxout(amt, order_spk, asset_out))
        tx.vout.append(self.ctxout(a_in - amt, self.wallet_spk(), asset_out))
        tx.vout.append(self.ctxout(btc_in - FEE, self.wallet_spk(), self.BTC_OUT))
        tx.vout.append(CTxOut(CTxOutValue(FEE)))
        signed = node.signrawtransactionwithwallet(tx.serialize().hex())
        assert signed["complete"], signed
        txid = node.sendrawtransaction(signed["hex"])
        self.generate(node, 1)
        return txid, 0, amt

    def assemble_joint(self, plan, in0, in1, slot_outs, settler_btc):
        """Assemble the joint FILL tx: covenant inputs 0 and 1 (introspection-only
        witnesses from the Go settler), the settler's bitcoin fee input, the four
        covenant-read output slots (slot_outs[0..3]), then bitcoin change + fee.
        Neither maker key is used. Returns the signed CTransaction."""
        node = self.nodes[0]
        tx = CTransaction()
        tx.nVersion = 2
        tx.vin.append(CTxIn(COutPoint(int(in0[0], 16), in0[1])))          # covenant 0
        tx.vin.append(CTxIn(COutPoint(int(in1[0], 16), in1[1])))          # covenant 1
        tx.vin.append(CTxIn(COutPoint(int(settler_btc["txid"], 16), settler_btc["vout"])))
        used_btc = 0
        for (amt, spk, aout) in slot_outs:
            tx.vout.append(self.ctxout(amt, spk, aout))
            if aout == self.BTC_OUT:
                used_btc += amt
        btc_in = int(satoshi_round(settler_btc["amount"]) * COIN)
        tx.vout.append(self.ctxout(btc_in - used_btc - FEE, self.wallet_spk(), self.BTC_OUT))  # settler change
        tx.vout.append(CTxOut(CTxOutValue(FEE)))                                                # fee
        partial = node.signrawtransactionwithwallet(tx.serialize().hex())
        tx = tx_from_hex(partial["hex"])
        while len(tx.wit.vtxinwit) < len(tx.vin):
            tx.wit.vtxinwit.append(CTxInWitness())
        tx.wit.vtxinwit[0].scriptWitness.stack = [
            bytes.fromhex(plan["leg0"]["fill_leaf"]), bytes.fromhex(plan["leg0"]["control_block"])]
        tx.wit.vtxinwit[1].scriptWitness.stack = [
            bytes.fromhex(plan["leg1"]["fill_leaf"]), bytes.fromhex(plan["leg1"]["control_block"])]
        return tx

    def slots_from_plan(self, plan):
        """Turn the Go settler's recipe into the four ordered covenant-read output
        slots. Credit slots pay the maker; a partial remainder self-replicates the
        covenant; a full-fill reserved slot is a settler bitcoin gap output."""
        slots = [None] * int(plan["min_outputs"])
        for legname in ("leg0", "leg1"):
            leg = plan[legname]
            ci = leg["credit_index"]
            slots[ci] = (int(leg["credit_value"]), bytes.fromhex(leg["credit_spk"]),
                         b"\x01" + bytes.fromhex(leg["credit_asset"]))
            ri = leg["remainder_index"]
            if leg["partial"]:
                slots[ri] = (int(leg["remainder"]), bytes.fromhex(leg["order_spk"]),
                             b"\x01" + bytes.fromhex(leg["remainder_asset"]))
            else:
                slots[ri] = (GAP, self.wallet_spk(), self.BTC_OUT)  # settler gap (non-sold-asset)
        return slots

    # --- the test -----------------------------------------------------------

    def run_test(self):
        node = self.nodes[0]
        self.settler = self.build_settler()

        self.generate(node, 101)
        node.sendtoaddress(address=node.getnewaddress(), amount=1000000,
                           fee_asset_label=BITCOIN_ASSET)
        self.generate(node, 1)
        self.genesis_hash = node.getblockhash(0)

        # X rests in maker-0's covenant; Y rests in maker-1's covenant.
        self.X_display = node.issueasset(assetamount=100000, tokenamount=0,
                                          blind=False, fee_asset=BITCOIN_ASSET)["asset"]
        self.generate(node, 1)
        self.Y_display = node.issueasset(assetamount=100000, tokenamount=0,
                                          blind=False, fee_asset=BITCOIN_ASSET)["asset"]
        self.generate(node, 1)
        self.X_OUT = self.asset_out(self.X_display)
        self.Y_OUT = self.asset_out(self.Y_display)
        self.BTC_OUT = b"\x01" + bytes.fromhex(BITCOIN_ASSET)[::-1]
        self.asset_x = bytes.fromhex(self.X_display)[::-1]   # internal-order ids
        self.asset_y = bytes.fromhex(self.Y_display)[::-1]

        # Two independent makers (distinct keys); each key is both its payout
        # program and its REFUND key. After funding they are OFFLINE.
        maker0_sec = generate_privkey()
        self.maker0_x = compute_xonly_pubkey(maker0_sec)[0]
        maker1_sec = generate_privkey()
        self.maker1_x = compute_xonly_pubkey(maker1_sec)[0]
        self.maker0_spk = bytes(CScript([OP_1, self.maker0_x]))
        self.maker1_spk = bytes(CScript([OP_1, self.maker1_x]))
        self.expiry = node.getblockcount() + 300

        # Covenant 0: sells X wants Y (3 Y per X). Covenant 1: sells Y wants X.
        self.tap0, self.fill0, _ = cov.order_taptree(
            self.asset_x, self.asset_y, NUM0, DEN0, self.maker0_x, MIN_LOT, self.expiry, self.maker0_x)
        self.tap1, self.fill1, _ = cov.order_taptree(
            self.asset_y, self.asset_x, NUM1, DEN1, self.maker1_x, MIN_LOT, self.expiry, self.maker1_x)

        self._exact_cross(node)
        self._partial_cross(node)
        self._reject_underpay(node)
        self._reject_credit_theft(node)

        self.log.info("SeqOB fully-passive settlement proven: two covenant orders, "
                      "both makers offline, matched and settled in one tx by an "
                      "untrusted settler that cannot steal.")

    def _verify_go_matches_python(self, plan):
        """The Go settler's covenant artifacts must be byte-identical to the proven
        Python builder, and its credit scriptPubKeys the two makers' payouts."""
        assert_equal(plan["leg0"]["fill_leaf"], bytes(self.fill0).hex())
        assert_equal(plan["leg1"]["fill_leaf"], bytes(self.fill1).hex())
        assert_equal(plan["leg0"]["control_block"], cov.control_block(self.tap0, "fill").hex())
        assert_equal(plan["leg1"]["control_block"], cov.control_block(self.tap1, "fill").hex())
        assert_equal(plan["leg0"]["credit_spk"], self.maker0_spk.hex())
        assert_equal(plan["leg1"]["credit_spk"], self.maker1_spk.hex())
        assert_equal(plan["leg0"]["credit_index"], 0)
        assert_equal(plan["leg1"]["credit_index"], 2)

    def _exact_cross(self, node):
        self.log.info("PASS: both-offline EXACT cross settled in one tx")
        locked0 = 30 * COIN     # maker-0 locks 30 X
        locked1 = 90 * COIN     # maker-1 locks 90 Y
        in0 = self.fund_covenant(self.tap0, self.X_display, self.X_OUT, 30)
        in1 = self.fund_covenant(self.tap1, self.Y_display, self.Y_OUT, 90)
        # Both makers are now offline: only funded UTXOs + advertised terms remain.

        plan = self.settler_plan(locked0, locked0, locked1, locked1)
        self._verify_go_matches_python(plan)
        # maker-0 must receive all Y (90); maker-1 all X (30); both full fills.
        assert_equal(int(plan["leg0"]["credit_value"]), 90 * COIN)
        assert_equal(int(plan["leg1"]["credit_value"]), 30 * COIN)
        assert_equal(plan["leg0"]["partial"], False)
        assert_equal(plan["leg1"]["partial"], False)
        assert_equal(sorted(plan["gap_slots"]), [1, 3])

        settler_btc = self.fresh_segwit_utxo(1)
        tx = self.assemble_joint(plan, in0, in1, self.slots_from_plan(plan), settler_btc)
        # The settler added exactly one (fee) input; the makers added nothing.
        assert_equal(len(tx.vin), 3)
        assert_equal(len(tx.wit.vtxinwit[0].scriptWitness.stack), 2)  # [leaf, control] only
        assert_equal(len(tx.wit.vtxinwit[1].scriptWitness.stack), 2)
        txid = node.sendrawtransaction(tx.serialize().hex())
        self.generate(node, 1)

        c0 = node.gettxout(txid, 0)   # maker-0 credited in Y
        assert_equal(c0["scriptPubKey"]["hex"], self.maker0_spk.hex())
        assert_equal(c0["asset"], self.Y_display)
        assert satoshi_round(c0["value"]) * COIN >= Decimal(90 * COIN)
        c1 = node.gettxout(txid, 2)   # maker-1 credited in X
        assert_equal(c1["scriptPubKey"]["hex"], self.maker1_spk.hex())
        assert_equal(c1["asset"], self.X_display)
        assert satoshi_round(c1["value"]) * COIN >= Decimal(30 * COIN)
        self.log.info("  maker-0 (offline) +%s Y at its own script; maker-1 (offline) +%s X",
                      c0["value"], c1["value"])

    def _partial_cross(self, node):
        self.log.info("PASS: PARTIAL cross re-rests the larger side's remainder as a covenant")
        locked0 = 30 * COIN
        locked1 = 180 * COIN    # maker-1 over-supplies; only 90 Y is taken
        fill1 = 90 * COIN
        in0 = self.fund_covenant(self.tap0, self.X_display, self.X_OUT, 30)
        in1 = self.fund_covenant(self.tap1, self.Y_display, self.Y_OUT, 180)

        plan = self.settler_plan(locked0, locked0, locked1, fill1)
        self._verify_go_matches_python(plan)
        assert_equal(plan["leg1"]["partial"], True)
        assert_equal(int(plan["leg1"]["remainder"]), locked1 - fill1)
        assert_equal(int(plan["leg1"]["remainder_index"]), 3)
        assert_equal(plan["gap_slots"], [1])  # only leg0 (full) needs a gap

        settler_btc = self.fresh_segwit_utxo(1)
        tx = self.assemble_joint(plan, in0, in1, self.slots_from_plan(plan), settler_btc)
        txid = node.sendrawtransaction(tx.serialize().hex())
        self.generate(node, 1)

        # The remainder output is a fresh covenant UTXO re-paying maker-1's own spk.
        rem = node.gettxout(txid, 3)
        assert_equal(rem["scriptPubKey"]["hex"], bytes(self.tap1.scriptPubKey).hex())
        assert_equal(rem["asset"], self.Y_display)
        assert_equal(satoshi_round(rem["value"]) * COIN, Decimal(locked1 - fill1))
        self.log.info("  maker-1's %s-Y remainder re-rests as a valid covenant order",
                      rem["value"])

        # Prove the re-rested remainder is still a fillable covenant: fill it with a
        # plain taker (single-covenant FILL), maker-1 still offline. The FILL
        # witness is introspection-only and identical for this covenant at any
        # fill size, so it is the same [leaf, control_block] the Go settler emits
        # (already asserted byte-equal above).
        f2 = 30 * COIN
        req2 = (f2 * NUM1 + DEN1 - 1) // DEN1
        rem_atoms = locked1 - fill1
        leg1_witness = [bytes(self.fill1), cov.control_block(self.tap1, "fill")]
        x_in = self.fresh_segwit_utxo(100, self.X_display)
        x_amt = int(satoshi_round(x_in["amount"]) * COIN)
        btc = self.fresh_segwit_utxo(1)
        btc_amt = int(satoshi_round(btc["amount"]) * COIN)
        node2 = self.nodes[0]
        t = CTransaction()
        t.nVersion = 2
        t.vin.append(CTxIn(COutPoint(int(txid, 16), 3)))                    # the remainder covenant
        t.vin.append(CTxIn(COutPoint(int(x_in["txid"], 16), x_in["vout"]))) # taker pays X
        t.vin.append(CTxIn(COutPoint(int(btc["txid"], 16), btc["vout"])))
        t.vout.append(self.ctxout(req2, self.maker1_spk, self.X_OUT))        # 0 credit X -> maker-1
        t.vout.append(self.ctxout(rem_atoms - f2, bytes(self.tap1.scriptPubKey), self.Y_OUT))  # 1 remainder Y
        t.vout.append(self.ctxout(f2, self.wallet_spk(), self.Y_OUT))        # 2 taker Y receipt
        t.vout.append(self.ctxout(x_amt - req2, self.wallet_spk(), self.X_OUT))
        t.vout.append(self.ctxout(btc_amt - FEE, self.wallet_spk(), self.BTC_OUT))
        t.vout.append(CTxOut(CTxOutValue(FEE)))
        signed = node2.signrawtransactionwithwallet(t.serialize().hex())
        t = tx_from_hex(signed["hex"])
        while len(t.wit.vtxinwit) < len(t.vin):
            t.wit.vtxinwit.append(CTxInWitness())
        t.wit.vtxinwit[0].scriptWitness.stack = leg1_witness
        rtxid = node2.sendrawtransaction(t.serialize().hex())
        self.generate(node2, 1)
        assert_equal(node2.gettxout(rtxid, 0)["asset"], self.X_display)
        self.log.info("  re-rested remainder filled again -> proven a valid covenant")

    def _reject_underpay(self, node):
        self.log.info("REJECT: settler underpays maker-0 (below its ceil price)")
        locked0, locked1 = 30 * COIN, 90 * COIN
        in0 = self.fund_covenant(self.tap0, self.X_display, self.X_OUT, 30)
        in1 = self.fund_covenant(self.tap1, self.Y_display, self.Y_OUT, 90)
        plan = self.settler_plan(locked0, locked0, locked1, locked1)
        slots = self.slots_from_plan(plan)
        # Pay maker-0 one atom short and pocket the atom (still asset-balanced).
        good0 = slots[0]
        slots[0] = (good0[0] - 1, good0[1], good0[2])
        slots.append((1, self.wallet_spk(), self.Y_OUT))  # the skimmed Y atom to the settler
        settler_btc = self.fresh_segwit_utxo(1)
        tx = self.assemble_joint(plan, in0, in1, slots, settler_btc)
        assert_raises_rpc_error(-26, "script-verify-flag-failed",
                                node.sendrawtransaction, tx.serialize().hex())

    def _reject_credit_theft(self, node):
        self.log.info("REJECT: settler redirects maker-1's credit to itself (theft / aliasing)")
        locked0, locked1 = 30 * COIN, 90 * COIN
        in0 = self.fund_covenant(self.tap0, self.X_display, self.X_OUT, 30)
        in1 = self.fund_covenant(self.tap1, self.Y_display, self.Y_OUT, 90)
        plan = self.settler_plan(locked0, locked0, locked1, locked1)
        slots = self.slots_from_plan(plan)
        # Keep maker-0 honest but grab maker-1's X (out 2) to the settler's own spk.
        # The fixed 2k index map forces covenant-1's credit onto output 2, and its
        # FILL leaf demands maker-1's scriptPubKey there -> the covenant rejects.
        good2 = slots[2]
        slots[2] = (good2[0], self.wallet_spk(), good2[2])
        settler_btc = self.fresh_segwit_utxo(1)
        tx = self.assemble_joint(plan, in0, in1, slots, settler_btc)
        assert_raises_rpc_error(-26, "script-verify-flag-failed",
                                node.sendrawtransaction, tx.serialize().hex())


if __name__ == "__main__":
    SeqObJointCovenantTest().main()
