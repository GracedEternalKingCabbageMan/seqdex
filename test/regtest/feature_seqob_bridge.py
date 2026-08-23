#!/usr/bin/env python3
# Copyright (c) 2026 The Sequentia developers
# Distributed under the MIT software license.
"""SILENT cross-rail settlement BRIDGE: an order placed ON-CHAIN (a funded
covenant resting order) matched against an order placed on LIGHTNING, settled
invisibly by the bridge.

feature_seqob_covenant_fill.py proved a single covenant filled by an online
taker; feature_seqob_joint_covenant.py proved TWO covenants settled by a fundless
settler. Here the counterparty is on a DIFFERENT RAIL: it rests on Lightning. The
bridge crosses the two rails under one preimage. It FILLS the on-chain covenant
(paying the on-chain maker its own pinned ceil price in asset Y, receiving the
sold asset X) and settles the Lightning leg off-chain. This test proves the
ON-CHAIN leg end to end on regtest and MOCKS the Lightning leg at the orchestrator
seam (asserting the bridge's computed LN amount + fronting decision + fee), which
is the honest scope this session: the box LN nodes are busy and the pure-LN rail
is down pending a libwally node fix, so a real live-LN proof is not available.

The bridge is the PRODUCTION Go component (seqdex cmd/seqob-bridge): given the
covenant order + the Lightning counterparty's price it computes the settlement
recipe (internal/seqob/bridge.Plan) - the covenant FILL leg (credit index/value/
floor/spk, the introspection-only witness) plus the priced LN leg and the
fronting decision. This test drives the on-chain broadcast from exactly that
recipe, so a bug in the Go bridge fails the proof.

Trust model proven here: the bridge cannot steal from the on-chain maker. The
maker credit (asset, scriptPubKey, min value) is fixed by its own FILL leaf; a
bridge that underpays it produces a transaction the covenant refuses (interpreter
code -26), identical to the settler's guarantee. What the bridge risks is only its
OWN fronted inventory, bounded by the per-offer 0-conf cap.

Scenarios:
  PASS   on-chain covenant crosses an LN order: the bridge FILLs the covenant,
         pays the maker its ceil price (confirmed on-chain), receives X, and the
         plan carries the correct LN-leg amount + a 0-conf front + a kept fee
  PASS   over-cap fill: the bridge REFUSES to 0-conf-front and requires confs
  REJECT bridge underpays the on-chain maker -> covenant consensus-rejects

Requires the Go toolchain and a configured, built Sequentia node checkout
(env GO_BIN / SEQUENTIA_DIR override the $HOME/dev-tools/go/bin/go and
$HOME/Sequentia defaults); see seqob_env.py.
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
MIN_LOT = 5 * COIN

# The on-chain covenant sells X wanting Y at 3 Y per X.
NUM, DEN = 3, 1


def go_bin():
    return seqob_env.go_bin()


def seqdex_dir():
    return seqob_env.seqdex_dir()


class SeqObBridgeTest(BitcoinTestFramework):

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

    # --- build the Go bridge ------------------------------------------------

    def build_bridge(self):
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
        out = os.path.join(outdir, "seqob-bridge")
        self.log.info("building seqob-bridge ...")
        subprocess.run([go, "build", "-o", out, "./cmd/seqob-bridge"], cwd=sdir, env=env, check=True)
        return out

    def bridge_plan(self, locked, fill, offer_y, want_x, max_0conf):
        """Run the PRODUCTION Go bridge to compute the cross-rail settlement recipe.
        Its output drives the on-chain FILL the test broadcasts and carries the
        (mocked) LN-leg amount + fronting decision + fee the test asserts."""
        cross = {
            "asset_x": self.asset_x.hex(), "asset_y": self.asset_y.hex(),
            "covenant": {"num": NUM, "den": DEN, "minlot": MIN_LOT,
                         "prog": self.maker_x.hex(), "makerx": self.maker_x.hex(),
                         "expiry": self.expiry, "locked": locked, "fill": fill},
            "lightning": {"offer_y": offer_y, "want_x": want_x, "max_0conf": max_0conf},
            "policy_fee_sats": 0, "rate_y": 0,
        }
        p = subprocess.run([self.bridge, "plan"], input=json.dumps(cross),
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

    def fund_covenant(self, asset_display, asset_out, units):
        """Fund the on-chain covenant order UTXO (`units` of asset X paying the
        covenant scriptPubKey). Returns (txid, vout, atoms). The maker then goes
        OFFLINE: only this UTXO + the advertised terms remain."""
        node = self.nodes[0]
        amt = units * COIN
        a_utxo = self.fresh_segwit_utxo(units + 1, asset_display)
        a_in = int(satoshi_round(a_utxo["amount"]) * COIN)
        btc = self.fresh_segwit_utxo(1)
        btc_in = int(satoshi_round(btc["amount"]) * COIN)

        tx = CTransaction()
        tx.nVersion = 2
        tx.vin.append(CTxIn(COutPoint(int(a_utxo["txid"], 16), a_utxo["vout"])))
        tx.vin.append(CTxIn(COutPoint(int(btc["txid"], 16), btc["vout"])))
        order_spk = bytes(self.order_tap.scriptPubKey)
        tx.vout.append(self.ctxout(amt, order_spk, asset_out))
        tx.vout.append(self.ctxout(a_in - amt, self.wallet_spk(), asset_out))
        tx.vout.append(self.ctxout(btc_in - FEE, self.wallet_spk(), self.BTC_OUT))
        tx.vout.append(CTxOut(CTxOutValue(FEE)))
        signed = node.signrawtransactionwithwallet(tx.serialize().hex())
        assert signed["complete"], signed
        txid = node.sendrawtransaction(signed["hex"])
        self.generate(node, 1)
        return txid, 0, amt

    def assemble_fill(self, plan, cov_in, pay_y, credit_spk):
        """Assemble the on-chain FILL: the bridge (as taker of the covenant) pays
        the maker `pay_y` of Y at `credit_spk` and receives `recv_x` of X, funding
        the Y payment + fee from its OWN inputs and adding the introspection-only
        FILL witness. Neither the maker key nor any signature over the covenant is
        used. Returns the signed CTransaction."""
        node = self.nodes[0]
        recv_x = int(plan["recv_x"])
        y_in = self.fresh_segwit_utxo(100, self.Y_display)
        y_amt = int(satoshi_round(y_in["amount"]) * COIN)
        btc = self.fresh_segwit_utxo(1)
        btc_amt = int(satoshi_round(btc["amount"]) * COIN)

        tx = CTransaction()
        tx.nVersion = 2
        tx.vin.append(CTxIn(COutPoint(int(cov_in[0], 16), cov_in[1])))       # covenant (X)
        tx.vin.append(CTxIn(COutPoint(int(y_in["txid"], 16), y_in["vout"]))) # bridge pays Y
        tx.vin.append(CTxIn(COutPoint(int(btc["txid"], 16), btc["vout"])))
        # vout0 credit -> maker (Y); vout1 the 2k+1 remainder slot: a FULL fill
        # here, so a NON-X output (bridge Y change) -> the leaf reads zero remainder.
        tx.vout.append(self.ctxout(pay_y, credit_spk, self.Y_OUT))               # 0 credit
        tx.vout.append(self.ctxout(y_amt - pay_y, self.wallet_spk(), self.Y_OUT))# 1 bridge Y change (non-X)
        tx.vout.append(self.ctxout(recv_x, self.wallet_spk(), self.X_OUT))       # 2 bridge X receipt
        tx.vout.append(self.ctxout(btc_amt - FEE, self.wallet_spk(), self.BTC_OUT))
        tx.vout.append(CTxOut(CTxOutValue(FEE)))
        partial = node.signrawtransactionwithwallet(tx.serialize().hex())
        tx = tx_from_hex(partial["hex"])
        while len(tx.wit.vtxinwit) < len(tx.vin):
            tx.wit.vtxinwit.append(CTxInWitness())
        tx.wit.vtxinwit[0].scriptWitness.stack = [
            bytes.fromhex(plan["fill_leaf"]), bytes.fromhex(plan["control_block"])]
        return tx

    # --- the test -----------------------------------------------------------

    def run_test(self):
        node = self.nodes[0]
        self.bridge = self.build_bridge()

        self.generate(node, 101)
        node.sendtoaddress(address=node.getnewaddress(), amount=1000000,
                           fee_asset_label=BITCOIN_ASSET)
        self.generate(node, 1)

        # X rests in the on-chain covenant; Y is what the LN counterparty pays and
        # what the bridge fronts to the maker on-chain.
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

        maker_sec = generate_privkey()
        self.maker_x = compute_xonly_pubkey(maker_sec)[0]
        self.maker_spk = bytes(CScript([OP_1, self.maker_x]))
        self.expiry = node.getblockcount() + 300

        # The covenant: sells X wants Y at 3 Y per X (min_lot 5). Maker key is both
        # its payout program and its REFUND key.
        self.order_tap, self.fill, _ = cov.order_taptree(
            self.asset_x, self.asset_y, NUM, DEN, self.maker_x, MIN_LOT, self.expiry, self.maker_x)

        self._pass_cross(node)
        self._over_cap(node)
        self._reject_underpay(node)

        self.log.info("SeqOB silent bridge proven: an on-chain covenant order and a "
                      "Lightning order cross invisibly - the bridge fills the covenant "
                      "on-chain (maker paid its own price, cannot be cheated) and the LN "
                      "leg amount/fronting/fee are computed correctly (LN leg mocked).")

    def _verify_go_matches_python(self, plan):
        """The Go bridge's covenant artifacts must be byte-identical to the proven
        Python builder, and its credit scriptPubKey the maker's payout."""
        assert_equal(plan["fill_leaf"], bytes(self.fill).hex())
        assert_equal(plan["control_block"], cov.control_block(self.order_tap, "fill").hex())
        assert_equal(plan["credit_spk"], self.maker_spk.hex())
        assert_equal(plan["credit_index"], 0)
        assert_equal(plan["remainder_index"], 1)

    def _pass_cross(self, node):
        self.log.info("PASS: on-chain covenant crosses an LN order (bridge fills + fronts)")
        locked = 30 * COIN
        in0 = self.fund_covenant(self.X_display, self.X_OUT, 30)

        # LN counterparty offers 100 Y for 30 X (a richer bid); 0-conf cap 50 X.
        offer_y, want_x, cap = 100 * COIN, 30 * COIN, 50 * COIN
        plan = self.bridge_plan(locked, locked, offer_y, want_x, cap)
        self._verify_go_matches_python(plan)

        # Economics: maker paid its ceil price (90 Y); bridge collects 100 Y over
        # LN and keeps a 10-Y fee; delivers 30 X over LN; 0-conf front (30 <= 50).
        pay_y = int(plan["credit_value"])
        assert_equal(pay_y, 90 * COIN)
        assert_equal(int(plan["credit_floor"]), 90 * COIN)
        assert_equal(int(plan["recv_x"]), 30 * COIN)
        assert_equal(int(plan["ln_deliver_x"]), 30 * COIN)
        assert_equal(int(plan["ln_recv_y"]), 100 * COIN)
        assert_equal(int(plan["fee_y"]), 10 * COIN)
        assert_equal(plan["zero_conf_front"], True)
        assert_equal(plan["require_confs"], False)
        assert_equal(plan["partial"], False)

        # Broadcast the ON-CHAIN FILL leg (the LN leg is mocked at the seam).
        tx = self.assemble_fill(plan, in0, pay_y, bytes.fromhex(plan["credit_spk"]))
        assert_equal(len(tx.wit.vtxinwit[0].scriptWitness.stack), 2)  # [leaf, control] only
        txid = node.sendrawtransaction(tx.serialize().hex())
        self.generate(node, 1)

        c0 = node.gettxout(txid, 0)   # maker credited its ceil price in Y
        assert_equal(c0["scriptPubKey"]["hex"], self.maker_spk.hex())
        assert_equal(c0["asset"], self.Y_display)
        assert_equal(satoshi_round(c0["value"]) * COIN, Decimal(pay_y))
        rx = node.gettxout(txid, 2)   # bridge received X on-chain
        assert_equal(rx["asset"], self.X_display)
        assert_equal(satoshi_round(rx["value"]) * COIN, Decimal(int(plan["recv_x"])))
        self.log.info("  on-chain: maker (offline) +%s Y at its own script; bridge +%s X. "
                      "LN (mocked): deliver %s X, collect %s Y, fee %s Y, 0-conf front",
                      c0["value"], rx["value"], Decimal(int(plan["ln_deliver_x"])) / COIN,
                      Decimal(int(plan["ln_recv_y"])) / COIN, Decimal(int(plan["fee_y"])) / COIN)

    def _over_cap(self, node):
        self.log.info("PASS: over-cap fill -> bridge refuses to 0-conf-front, requires confs")
        locked = 30 * COIN
        # Same cross but the per-offer 0-conf cap is only 20 X < the 30-X fill.
        plan = self.bridge_plan(locked, locked, 100 * COIN, 30 * COIN, 20 * COIN)
        assert_equal(plan["zero_conf_front"], False)
        assert_equal(plan["require_confs"], True)
        self.log.info("  reason: %s", plan["front_reason"])

    def _reject_underpay(self, node):
        self.log.info("REJECT: bridge underpays the on-chain maker (below its ceil price)")
        locked = 30 * COIN
        in0 = self.fund_covenant(self.X_display, self.X_OUT, 30)
        plan = self.bridge_plan(locked, locked, 100 * COIN, 30 * COIN, 50 * COIN)
        pay_y = int(plan["credit_value"])
        # Pay the maker ONE atom short: still asset-balanced, but the FILL leaf
        # demands >= the ceil price at the maker's own scriptPubKey -> reject.
        tx = self.assemble_fill(plan, in0, pay_y - 1, bytes.fromhex(plan["credit_spk"]))
        assert_raises_rpc_error(-26, "script-verify-flag-failed",
                                node.sendrawtransaction, tx.serialize().hex())


if __name__ == "__main__":
    SeqObBridgeTest().main()
