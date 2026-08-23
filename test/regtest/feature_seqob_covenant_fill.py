#!/usr/bin/env python3
# Copyright (c) 2026 The Sequentia developers
# Distributed under the MIT software license.
"""SeqOB covenant FILL/REFUND: passive resting DEX orders, filled unilaterally.

Proves the foundational primitive for funded, non-interactive resting offers:
an OFFLINE maker locks N units of explicit asset A in one taproot UTXO
(internal key NUMS, tree {FILL, REFUND}); ANYONE who pays the maker's baked-in
price of asset B can spend it, enforced entirely by the script interpreter. No
maker signature, no server, no consensus change (a 0xc4 tapscript leaf gated
only by always-active SCRIPT_VERIFY_TAPROOT). See seqob_covenant.py and
docs/simplicity-dex-covenant-offers-design.md.

Scenarios (each rejection is the node's own interpreter, code -26):

  PASS  unilateral FULL fill        (taker pays ceil(N*num/den) of B to maker)
  PASS  PARTIAL fill                (pro-rata B + remainder re-paid to covenant)
  PASS  second partial fill of the remainder
  REJECT wrong payment asset
  REJECT underpayment (< ceil price)
  REJECT confidential/blinded credit output
  REJECT output-aliasing (two covenant inputs, one shared maker credit)
  REJECT remainder paid to a DIFFERENT script (covenant not self-replicated)
  REJECT below-min_lot fill
  PASS  REFUND after expiry; REJECT REFUND before expiry
"""

from decimal import Decimal

import seqob_env  # noqa: F401  (puts the node's test_framework on sys.path)
from test_framework.test_framework import BitcoinTestFramework
from test_framework.util import assert_equal, satoshi_round, BITCOIN_ASSET
from test_framework.key import compute_xonly_pubkey, generate_privkey, sign_schnorr
from test_framework.messages import (
    COIN, COutPoint, CTransaction, CTxIn, CTxInWitness, CTxOut, CTxOutAsset,
    CTxOutNonce, CTxOutValue, uint256_from_str, tx_from_hex,
)
from test_framework.script import CScript, OP_1, TaprootSignatureHash

import seqob_covenant as cov

FEE = 5000                      # bitcoin network fee (atoms)
N = 90 * COIN                   # asset A locked per order UTXO (90 units)
RATE_NUM, RATE_DEN = 1, 3       # price: required_B = ceil(filled * 1/3)
MIN_LOT = 5 * COIN              # dust-griefing floor (5 units)


def ceil_price(filled):
    return (filled * RATE_NUM + RATE_DEN - 1) // RATE_DEN


class SeqObCovenantFillTest(BitcoinTestFramework):

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

    def setup_network(self, split=False):
        self.setup_nodes()

    # --- small helpers -----------------------------------------------------

    def wallet_spk(self):
        addr = self.nodes[0].getnewaddress()
        unconf = self.nodes[0].getaddressinfo(addr)["unconfidential"]
        return bytes.fromhex(self.nodes[0].getaddressinfo(unconf)["scriptPubKey"])

    def asset_out(self, display_hex):
        return b"\x01" + bytes.fromhex(display_hex)[::-1]

    def ctxout(self, amount, spk, asset_out):
        return CTxOut(nValue=CTxOutValue(amount), scriptPubKey=spk, nAsset=CTxOutAsset(asset_out))

    def fresh_segwit_utxo(self, amount, asset_display=None):
        """A fresh bech32 (segwit v0) utxo holding `amount` whole units of the
        asset (bitcoin if asset_display is None), for taker payment / fee inputs."""
        node = self.nodes[0]
        bech = node.getnewaddress("", "bech32")
        unconf = node.getaddressinfo(bech)["unconfidential"]
        if asset_display is None:
            # SEQUENTIA: an open-fee-market chain has no default fee asset.
            node.sendtoaddress(address=unconf, amount=amount,
                               fee_asset_label=BITCOIN_ASSET)
            target = BITCOIN_ASSET
        else:
            # No fee asset is inferred from the asset being sent; name bitcoin
            # (the policy asset), which this node prices.
            node.sendtoaddress(address=unconf, amount=amount, assetlabel=asset_display,
                               fee_asset_label=BITCOIN_ASSET)
            target = asset_display
        self.generate(node, 1)
        for u in node.listunspent():
            if (u["asset"] == target and abs(float(u["amount"]) - amount) < 1e-9
                    and u["scriptPubKey"].startswith("0014") and u["spendable"]):
                return u
        raise AssertionError("no fresh segwit utxo for %s" % target)

    def addr_spk(self, addr):
        info = self.nodes[0].getaddressinfo(addr)
        unconf = info.get("unconfidential", addr)
        return bytes.fromhex(self.nodes[0].getaddressinfo(unconf)["scriptPubKey"])

    def find_asset_utxo(self, asset_display, min_units):
        for u in self.nodes[0].listunspent():
            if u["asset"] == asset_display and float(u["amount"]) >= min_units and u["spendable"]:
                return u
        raise AssertionError("no wallet utxo of %s" % asset_display)

    def fund_orders(self, count):
        """Fund `count` covenant order UTXOs (N of asset A each, paying order_spk)
        in one transaction. Returns [(txid, vout, N), ...]."""
        node = self.nodes[0]
        need_units = count * 90 + 1
        a_utxo = self.find_asset_utxo(self.A_display, need_units)
        a_in = int(satoshi_round(a_utxo["amount"]) * COIN)
        btc = self.fresh_segwit_utxo(1)
        btc_in = int(satoshi_round(btc["amount"]) * COIN)

        tx = CTransaction()
        tx.nVersion = 2
        tx.vin.append(CTxIn(COutPoint(int(a_utxo["txid"], 16), a_utxo["vout"])))
        tx.vin.append(CTxIn(COutPoint(int(btc["txid"], 16), btc["vout"])))
        order_spk = bytes(self.order_tap.scriptPubKey)
        for _ in range(count):
            tx.vout.append(self.ctxout(N, order_spk, self.A_OUT))
        tx.vout.append(self.ctxout(a_in - count * N, self.wallet_spk(), self.A_OUT))  # A change
        tx.vout.append(self.ctxout(btc_in - FEE, self.wallet_spk(), self.BTC_OUT))    # btc change
        tx.vout.append(CTxOut(CTxOutValue(FEE)))                                      # fee
        signed = node.signrawtransactionwithwallet(tx.serialize().hex())
        assert signed["complete"], signed
        txid = node.sendrawtransaction(signed["hex"])
        self.generate(node, 1)
        return [(txid, i, N) for i in range(count)]

    def assert_rejected(self, tx, want=None):
        """Assert the node rejects the tx, surfacing the exact reject reason so we
        can confirm WHICH layer refused it (the covenant interpreter, not some
        incidental error)."""
        res = self.nodes[0].testmempoolaccept([tx.serialize().hex()])[0]
        assert not res["allowed"], res
        reason = res.get("reject-reason", "")
        self.log.info("  reject-reason: %s", reason)
        if want is not None:
            assert want in reason, "expected %r in %r" % (want, reason)

    def assemble_fill(self, cov_ins, wallet_ins, outs):
        """Build a FILL-spend: covenant inputs first (indices 0..len-1), then
        wallet inputs; `outs` = [(amount, spk, asset_out or None-for-fee)].
        The wallet inputs are signed by the wallet; every covenant input gets the
        introspection-only FILL witness. Returns the signed CTransaction."""
        node = self.nodes[0]
        tx = CTransaction()
        tx.nVersion = 2
        for (txid, vout, _amt) in cov_ins:
            tx.vin.append(CTxIn(COutPoint(int(txid, 16), vout)))
        for u in wallet_ins:
            tx.vin.append(CTxIn(COutPoint(int(u["txid"], 16), u["vout"])))
        for (amt, spk, aout) in outs:
            if aout is None:
                tx.vout.append(CTxOut(CTxOutValue(amt)))     # bitcoin fee output
            else:
                tx.vout.append(self.ctxout(amt, spk, aout))
        partial = node.signrawtransactionwithwallet(tx.serialize().hex())
        tx = tx_from_hex(partial["hex"])
        while len(tx.wit.vtxinwit) < len(tx.vin):
            tx.wit.vtxinwit.append(CTxInWitness())
        for i in range(len(cov_ins)):
            tx.wit.vtxinwit[i].scriptWitness.stack = cov.fill_witness(self.order_tap, self.fill)
        return tx

    # --- the test ----------------------------------------------------------

    def run_test(self):
        node = self.nodes[0]
        self.generate(node, 101)
        # Move the initialfreecoins anyone-can-spend output into ordinary wallet
        # utxos (there is no coinbase subsidy on Sequentia; this is the only
        # bitcoin) so multi-asset coin selection has plain inputs to work with.
        node.sendtoaddress(address=node.getnewaddress(), amount=1000000,
                           fee_asset_label=BITCOIN_ASSET)
        self.generate(node, 1)
        self.genesis = uint256_from_str(bytes.fromhex(node.getblockhash(0))[::-1])

        # Two ordinary explicit assets: A rests in orders, B pays for them.
        self.A_display = node.issueasset(assetamount=100000, tokenamount=0,
                                          blind=False, fee_asset=BITCOIN_ASSET)["asset"]
        self.generate(node, 1)
        self.B_display = node.issueasset(assetamount=100000, tokenamount=0,
                                          blind=False, fee_asset=BITCOIN_ASSET)["asset"]
        self.generate(node, 1)
        self.A_OUT = self.asset_out(self.A_display)
        self.B_OUT = self.asset_out(self.B_display)
        self.BTC_OUT = b"\x01" + bytes.fromhex(BITCOIN_ASSET)[::-1]
        asset_a = bytes.fromhex(self.A_display)[::-1]     # internal byte order
        asset_b = bytes.fromhex(self.B_display)[::-1]

        # The maker: one keypair. maker_prog is a v1 payout program; the same key
        # authorises the REFUND leaf.
        maker_sec = generate_privkey()
        maker_x = compute_xonly_pubkey(maker_sec)[0]
        maker_spk = bytes(CScript([OP_1, maker_x]))       # the maker credit scriptPubKey

        self.expiry = node.getblockcount() + 300          # absolute-height CLTV expiry
        self.order_tap, self.fill, self.refund = cov.order_taptree(
            asset_a, asset_b, RATE_NUM, RATE_DEN, maker_x, MIN_LOT, self.expiry, maker_x)
        self.log.info("order spk %s  (FILL leaf %d bytes, expiry height %d)"
                      % (bytes(self.order_tap.scriptPubKey).hex(), len(bytes(self.fill)), self.expiry))

        u = self.fund_orders(10)
        ff, pf, wa, up, al0, al1, bml, rws, ff2, rf = u
        self.log.info("funded 10 order UTXOs of %d units each" % (N // COIN))

        req_full = ceil_price(N)

        # -- PASS: unilateral FULL fill -------------------------------------
        self.log.info("PASS: unilateral full fill")
        b_in = self.fresh_segwit_utxo(100, self.B_display)
        b_amt = int(satoshi_round(b_in["amount"]) * COIN)
        btc = self.fresh_segwit_utxo(1)
        btc_amt = int(satoshi_round(btc["amount"]) * COIN)
        tx = self.assemble_fill([ff], [b_in, btc], [
            (req_full, maker_spk, self.B_OUT),                 # vout0 credit (B) -> maker
            (b_amt - req_full, self.wallet_spk(), self.B_OUT), # vout1 taker B change (not A)
            (N, self.wallet_spk(), self.A_OUT),                # vout2 taker A receipt
            (btc_amt - FEE, self.wallet_spk(), self.BTC_OUT),  # vout3 btc change
            (FEE, b"", None),                                  # vout4 fee
        ])
        txid = node.sendrawtransaction(tx.serialize().hex())
        self.generate(node, 1)
        assert_equal(node.gettxout(txid, 0)["scriptPubKey"]["hex"], maker_spk.hex())
        assert_equal(satoshi_round(node.gettxout(txid, 0)["value"]) * COIN, Decimal(req_full))

        # -- PASS: partial fill (ceil price exercised) ----------------------
        self.log.info("PASS: partial fill, remainder re-paid to the covenant")
        f1 = 10 * COIN
        req1 = ceil_price(f1)
        rem1 = N - f1
        assert req1 * RATE_DEN != f1 * RATE_NUM  # this fill genuinely rounds up
        b_in = self.fresh_segwit_utxo(100, self.B_display)
        b_amt = int(satoshi_round(b_in["amount"]) * COIN)
        btc = self.fresh_segwit_utxo(1)
        btc_amt = int(satoshi_round(btc["amount"]) * COIN)
        order_spk = bytes(self.order_tap.scriptPubKey)
        tx = self.assemble_fill([pf], [b_in, btc], [
            (req1, maker_spk, self.B_OUT),                     # vout0 credit
            (rem1, order_spk, self.A_OUT),                     # vout1 remainder -> SAME covenant
            (f1, self.wallet_spk(), self.A_OUT),               # vout2 taker A receipt
            (b_amt - req1, self.wallet_spk(), self.B_OUT),     # vout3 taker B change
            (btc_amt - FEE, self.wallet_spk(), self.BTC_OUT),  # vout4 btc change
            (FEE, b"", None),                                  # vout5 fee
        ])
        ptxid = node.sendrawtransaction(tx.serialize().hex())
        self.generate(node, 1)
        assert_equal(node.gettxout(ptxid, 1)["scriptPubKey"]["hex"], order_spk.hex())
        assert_equal(satoshi_round(node.gettxout(ptxid, 1)["value"]) * COIN, Decimal(rem1))

        # -- PASS: second partial fill of the remainder ---------------------
        self.log.info("PASS: second partial fill of the resting remainder")
        f2 = 20 * COIN
        req2 = ceil_price(f2)
        rem2 = rem1 - f2
        b_in = self.fresh_segwit_utxo(100, self.B_display)
        b_amt = int(satoshi_round(b_in["amount"]) * COIN)
        btc = self.fresh_segwit_utxo(1)
        btc_amt = int(satoshi_round(btc["amount"]) * COIN)
        tx = self.assemble_fill([(ptxid, 1, rem1)], [b_in, btc], [
            (req2, maker_spk, self.B_OUT),
            (rem2, order_spk, self.A_OUT),
            (f2, self.wallet_spk(), self.A_OUT),
            (b_amt - req2, self.wallet_spk(), self.B_OUT),
            (btc_amt - FEE, self.wallet_spk(), self.BTC_OUT),
            (FEE, b"", None),
        ])
        p2txid = node.sendrawtransaction(tx.serialize().hex())
        self.generate(node, 1)
        assert_equal(satoshi_round(node.gettxout(p2txid, 1)["value"]) * COIN, Decimal(rem2))

        # -- REJECT: wrong payment asset (credit paid in bitcoin, not B) ----
        self.log.info("REJECT: wrong payment asset")
        btc = self.fresh_segwit_utxo(40)
        btc_amt = int(satoshi_round(btc["amount"]) * COIN)
        tx = self.assemble_fill([wa], [btc], [
            (req_full, maker_spk, self.BTC_OUT),               # credit in BITCOIN, not B
            (btc_amt - req_full - FEE, self.wallet_spk(), self.BTC_OUT),
            (N, self.wallet_spk(), self.A_OUT),
            (FEE, b"", None),
        ])
        self.assert_rejected(tx, "script-verify-flag-failed")

        # -- REJECT: underpayment (one atom below the ceil price) -----------
        self.log.info("REJECT: underpayment")
        b_in = self.fresh_segwit_utxo(100, self.B_display)
        b_amt = int(satoshi_round(b_in["amount"]) * COIN)
        btc = self.fresh_segwit_utxo(1)
        btc_amt = int(satoshi_round(btc["amount"]) * COIN)
        tx = self.assemble_fill([up], [b_in, btc], [
            (req_full - 1, maker_spk, self.B_OUT),             # one atom short
            (b_amt - (req_full - 1), self.wallet_spk(), self.B_OUT),
            (N, self.wallet_spk(), self.A_OUT),
            (btc_amt - FEE, self.wallet_spk(), self.BTC_OUT),
            (FEE, b"", None),
        ])
        self.assert_rejected(tx, "script-verify-flag-failed")

        # -- REJECT: output-aliasing (two covenant inputs, one shared credit) --
        self.log.info("REJECT: output-aliasing across two covenant inputs")
        b_in = self.fresh_segwit_utxo(100, self.B_display)
        b_amt = int(satoshi_round(b_in["amount"]) * COIN)
        btc = self.fresh_segwit_utxo(1)
        btc_amt = int(satoshi_round(btc["amount"]) * COIN)
        # input0 (k=0) credit at vout0, remainder slot vout1; input1 (k=1) credit
        # at vout2, remainder slot vout3. The attacker pays the maker ONCE (vout0)
        # and grabs both inputs' A at vout2. Input1's 2k map forces it to demand
        # its own credit at vout2, which is asset A, not B -> rejected.
        tx = self.assemble_fill([al0, al1], [b_in, btc], [
            (req_full, maker_spk, self.B_OUT),                 # vout0 credit for input0
            (b_amt - req_full, self.wallet_spk(), self.B_OUT), # vout1 (not A -> input0 full fill)
            (2 * N, self.wallet_spk(), self.A_OUT),            # vout2 grabs both A's (input1 credit slot)
            (btc_amt - FEE, self.wallet_spk(), self.BTC_OUT),  # vout3 (input1 remainder slot)
            (FEE, b"", None),                                  # vout4 fee
        ])
        self.assert_rejected(tx, "script-verify-flag-failed")

        # -- REJECT: remainder paid to a DIFFERENT script -------------------
        self.log.info("REJECT: remainder not self-replicated to the covenant")
        b_in = self.fresh_segwit_utxo(100, self.B_display)
        b_amt = int(satoshi_round(b_in["amount"]) * COIN)
        btc = self.fresh_segwit_utxo(1)
        btc_amt = int(satoshi_round(btc["amount"]) * COIN)
        tx = self.assemble_fill([rws], [b_in, btc], [
            (req1, maker_spk, self.B_OUT),
            (rem1, self.wallet_spk(), self.A_OUT),             # remainder to WALLET, not order_spk
            (f1, self.wallet_spk(), self.A_OUT),
            (b_amt - req1, self.wallet_spk(), self.B_OUT),
            (btc_amt - FEE, self.wallet_spk(), self.BTC_OUT),
            (FEE, b"", None),
        ])
        self.assert_rejected(tx, "script-verify-flag-failed")

        # -- REJECT: below-min_lot fill -------------------------------------
        self.log.info("REJECT: below-min_lot fill")
        fsmall = 2 * COIN
        reqs = ceil_price(fsmall)
        rems = N - fsmall
        b_in = self.fresh_segwit_utxo(100, self.B_display)
        b_amt = int(satoshi_round(b_in["amount"]) * COIN)
        btc = self.fresh_segwit_utxo(1)
        btc_amt = int(satoshi_round(btc["amount"]) * COIN)
        tx = self.assemble_fill([bml], [b_in, btc], [
            (reqs, maker_spk, self.B_OUT),
            (rems, order_spk, self.A_OUT),                     # remainder ok (>= min_lot)
            (fsmall, self.wallet_spk(), self.A_OUT),           # but filled 2 units < 5-unit min_lot
            (b_amt - reqs, self.wallet_spk(), self.B_OUT),
            (btc_amt - FEE, self.wallet_spk(), self.BTC_OUT),
            (FEE, b"", None),
        ])
        self.assert_rejected(tx, "script-verify-flag-failed")

        # -- REJECT: confidential/blinded credit output ---------------------
        self.reject_blinded_credit(ff2, maker_spk)

        # -- REFUND leaf ----------------------------------------------------
        self.refund_tests(rf, maker_sec)

        self.log.info("SeqOB covenant fill: primitive proven - offline maker's "
                      "order filled unilaterally, enforced by consensus")

    def build_refund(self, rf, maker_sec, locktime, refund_spk):
        node = self.nodes[0]
        btc = self.fresh_segwit_utxo(1)
        btc_amt = int(satoshi_round(btc["amount"]) * COIN)
        tx = CTransaction()
        tx.nVersion = 2
        tx.nLockTime = locktime
        tx.vin.append(CTxIn(COutPoint(int(rf[0], 16), rf[1]), nSequence=0xfffffffe))
        tx.vin.append(CTxIn(COutPoint(int(btc["txid"], 16), btc["vout"]), nSequence=0xfffffffe))
        tx.vout.append(self.ctxout(N, refund_spk, self.A_OUT))          # A back to the maker
        tx.vout.append(self.ctxout(btc_amt - FEE, self.wallet_spk(), self.BTC_OUT))
        tx.vout.append(CTxOut(CTxOutValue(FEE)))
        partial = node.signrawtransactionwithwallet(tx.serialize().hex())
        tx = tx_from_hex(partial["hex"])
        spent = [self.ctxout(N, bytes(self.order_tap.scriptPubKey), self.A_OUT),
                 self.ctxout(btc_amt, bytes.fromhex(btc["scriptPubKey"]),
                             b"\x01" + bytes.fromhex(btc["asset"])[::-1])]
        msg = TaprootSignatureHash(tx, spent, 0, self.genesis, 0, scriptpath=True, script=self.refund)
        sig = sign_schnorr(maker_sec, msg)
        while len(tx.wit.vtxinwit) < len(tx.vin):
            tx.wit.vtxinwit.append(CTxInWitness())
        tx.wit.vtxinwit[0].scriptWitness.stack = cov.refund_witness(self.order_tap, self.refund, sig)
        return tx

    def refund_tests(self, rf, maker_sec):
        node = self.nodes[0]
        refund_spk = self.wallet_spk()   # a stable destination for the reclaimed order
        # before expiry: CLTV rejects (tx locktime below the baked expiry height)
        self.log.info("REJECT: REFUND before expiry")
        tx = self.build_refund(rf, maker_sec, node.getblockcount(), refund_spk)
        self.assert_rejected(tx, "script-verify-flag-failed")
        # after expiry: the maker reclaims the order
        self.log.info("PASS: REFUND after expiry")
        self.generate(node, self.expiry - node.getblockcount() + 1)
        tx = self.build_refund(rf, maker_sec, self.expiry, refund_spk)
        rtxid = node.sendrawtransaction(tx.serialize().hex())
        self.generate(node, 1)
        assert_equal(node.gettxout(rtxid, 0)["scriptPubKey"]["hex"], refund_spk.hex())

    def reject_blinded_credit(self, ff2, maker_spk):
        """A full fill whose maker-credit output is confidential (blinded) is
        rejected by the covenant: the FILL leaf asserts every introspected
        output asset/value prefix == 0x01 (explicit). The credit here carries a
        REAL, balanced Pedersen commitment + rangeproof (built with
        rawblindrawtransaction), so it passes the amount layer and it is the
        script interpreter that refuses it."""
        node = self.nodes[0]
        self.log.info("REJECT: confidential/blinded credit output")
        b_in = self.fresh_segwit_utxo(100, self.B_display)
        b_amt = int(satoshi_round(b_in["amount"]) * COIN)
        btc = self.fresh_segwit_utxo(1)
        btc_amt = int(satoshi_round(btc["amount"]) * COIN)
        req_full = ceil_price(N)
        # Confidential destinations for the B-leg (credit + change). Blinding two
        # outputs against all-explicit inputs balances (last blinder is forced).
        maker_conf = node.getnewaddress("", "blech32")
        change_conf = node.getnewaddress("", "blech32")
        maker_ck = bytes.fromhex(node.getaddressinfo(maker_conf)["confidential_key"])
        change_ck = bytes.fromhex(node.getaddressinfo(change_conf)["confidential_key"])

        tx = CTransaction()
        tx.nVersion = 2
        tx.vin.append(CTxIn(COutPoint(int(ff2[0], 16), ff2[1])))         # covenant (A, explicit)
        tx.vin.append(CTxIn(COutPoint(int(b_in["txid"], 16), b_in["vout"])))
        tx.vin.append(CTxIn(COutPoint(int(btc["txid"], 16), btc["vout"])))
        o0 = self.ctxout(req_full, self.addr_spk(maker_conf), self.B_OUT)      # credit -> BLINDED
        o0.nNonce = CTxOutNonce(maker_ck)
        o1 = self.ctxout(b_amt - req_full, self.addr_spk(change_conf), self.B_OUT)
        o1.nNonce = CTxOutNonce(change_ck)                                     # B change -> BLINDED
        tx.vout.append(o0)
        tx.vout.append(o1)
        tx.vout.append(self.ctxout(N, self.wallet_spk(), self.A_OUT))          # taker A receipt (explicit)
        tx.vout.append(self.ctxout(btc_amt - FEE, self.wallet_spk(), self.BTC_OUT))
        tx.vout.append(CTxOut(CTxOutValue(FEE)))
        z = "00" * 32   # all three inputs are explicit -> zero blinders
        blinded = node.rawblindrawtransaction(
            tx.serialize().hex(), [z, z, z],
            [Decimal(90), satoshi_round(b_in["amount"]), satoshi_round(btc["amount"])],
            [self.A_display, self.B_display, BITCOIN_ASSET], [z, z, z], "", False)
        tx = tx_from_hex(blinded)
        # confirm the credit output is genuinely a commitment (prefix != 0x01)
        assert tx.vout[0].nAsset.vchCommitment[0] in (0x0a, 0x0b), tx.vout[0].nAsset.vchCommitment[0]
        partial = node.signrawtransactionwithwallet(tx.serialize().hex())
        tx = tx_from_hex(partial["hex"])
        while len(tx.wit.vtxinwit) < len(tx.vin):
            tx.wit.vtxinwit.append(CTxInWitness())
        tx.wit.vtxinwit[0].scriptWitness.stack = cov.fill_witness(self.order_tap, self.fill)
        self.assert_rejected(tx, "script-verify-flag-failed")


if __name__ == "__main__":
    SeqObCovenantFillTest().main()
