#!/usr/bin/env python3
# Copyright (c) 2026 The Sequentia developers
# Distributed under the MIT software license.
"""Passive CLOB end-to-end: the SeqOB relay's matching engine crosses a
covenant-funded resting order against an incoming order and it settles with the
resting maker OFFLINE.

This proves the whole passive limit-order-book loop on regtest, exercising the
PRODUCTION Go code (seqdex): the seqobd relay + its continuous matcher, and the
Go covenant builder (daemon/pkg/covenant, the byte-for-byte port of the proven
seqob_covenant.py that feature_seqob_covenant_fill.py validated with 11
consensus scenarios).

Flow:
  1. Maker funds a covenant SELL order (N of asset A at rate num/den) in one
     taproot UTXO and POSTs it to the relay over REST, then is OFFLINE (REST is
     stateless; no maker process/connection remains).
  2. A NON-crossing taker order is left resting by the matcher (no match).
  3. A crossing taker order is auto-matched by the relay (From.matched, no manual
     lift), carrying the covenant terms + fill size/price.
  4. The taker builds the covenant FILL spend from the Go builder and broadcasts
     it; it confirms; the OFFLINE maker received >= its price in B and the taker
     received A (verified on-chain — the maker never came back online).
  5. A partial fill re-rests the remainder as a still-valid covenant order, and
     that remainder is filled again.

Requires the Go toolchain and a configured, built Sequentia node checkout
(env GO_BIN / SEQUENTIA_DIR override the $HOME/dev-tools/go/bin/go and
$HOME/Sequentia defaults); see seqob_env.py. The relay
binaries (seqobd, seqob-covenant, seqob-relaycli) are built once per run.
"""

import base64
import json
import os
import socket
import subprocess
import time
from decimal import Decimal

import seqob_env  # noqa: F401  (puts the node's test_framework on sys.path)
from test_framework.test_framework import BitcoinTestFramework, SkipTest
from test_framework.util import assert_equal, satoshi_round, BITCOIN_ASSET
from test_framework.key import compute_xonly_pubkey, generate_privkey
from test_framework.messages import (
    COIN, COutPoint, CTransaction, CTxIn, CTxInWitness, CTxOut, CTxOutAsset,
    CTxOutValue, tx_from_hex,
)
from test_framework.script import CScript, OP_1

import seqob_covenant as cov

FEE = 5000
N = 90 * COIN
RATE_NUM, RATE_DEN = 1, 3
MIN_LOT = 5 * COIN


def ceil_price(filled):
    return (filled * RATE_NUM + RATE_DEN - 1) // RATE_DEN


def free_port():
    s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    s.bind(("127.0.0.1", 0))
    port = s.getsockname()[1]
    s.close()
    return port


def go_bin():
    return seqob_env.go_bin()


def seqdex_dir():
    return seqob_env.seqdex_dir()


class SeqObMatcherCovenantTest(BitcoinTestFramework):

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

    # --- build + run the Go relay binaries ---------------------------------

    def build_binaries(self):
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
        bins = {}
        for name, pkg in [
            ("seqobd", "./cmd/seqobd"),
            ("seqob-covenant", "./cmd/seqob-covenant"),
            ("seqob-relaycli", "./cmd/seqob-relaycli"),
        ]:
            out = os.path.join(outdir, name)
            self.log.info("building %s ...", name)
            subprocess.run([go, "build", "-o", out, pkg], cwd=sdir, env=env, check=True)
            bins[name] = out
        return bins

    def start_relay(self):
        self.relay_port = free_port()
        self.relay_http = "http://127.0.0.1:%d" % self.relay_port
        self.relay_ws = "ws://127.0.0.1:%d/v1/ws" % self.relay_port
        env = dict(os.environ)
        env["SEQOB_LISTEN"] = ":%d" % self.relay_port
        self.relay_log = open(os.path.join(self.options.tmpdir, "seqobd.log"), "w")
        self.relay_proc = subprocess.Popen(
            [self.bins["seqobd"], "-min-expiry", "5s"],
            env=env, stdout=self.relay_log, stderr=subprocess.STDOUT)
        # Wait for it to accept connections.
        for _ in range(100):
            try:
                s = socket.create_connection(("127.0.0.1", self.relay_port), timeout=0.2)
                s.close()
                return
            except OSError:
                time.sleep(0.1)
        raise RuntimeError("seqobd did not come up on port %d" % self.relay_port)

    def stop_relay(self):
        if getattr(self, "relay_proc", None):
            self.relay_proc.terminate()
            try:
                self.relay_proc.wait(timeout=5)
            except subprocess.TimeoutExpired:
                self.relay_proc.kill()
        if getattr(self, "relay_log", None):
            self.relay_log.close()

    def cov_derive(self):
        """Run the PRODUCTION Go covenant builder to derive the order's taproot
        scriptPubKey + control-block pieces."""
        out = subprocess.run(
            [self.bins["seqob-covenant"], "derive",
             "-a", self.asset_a.hex(), "-b", self.asset_b.hex(),
             "-num", str(RATE_NUM), "-den", str(RATE_DEN), "-minlot", str(MIN_LOT),
             "-prog", self.maker_x.hex(), "-makerver", "1",
             "-expiry", str(self.expiry), "-makerx", self.maker_x.hex()],
            capture_output=True, text=True, check=True)
        return json.loads(out.stdout)

    def cov_fill(self, locked, filled, k):
        out = subprocess.run(
            [self.bins["seqob-covenant"], "fill",
             "-a", self.asset_a.hex(), "-b", self.asset_b.hex(),
             "-num", str(RATE_NUM), "-den", str(RATE_DEN), "-minlot", str(MIN_LOT),
             "-prog", self.maker_x.hex(), "-makerver", "1",
             "-expiry", str(self.expiry), "-makerx", self.maker_x.hex(),
             "-locked", str(locked), "-filled", str(filled), "-k", str(k)],
            capture_output=True, text=True, check=True)
        return json.loads(out.stdout)

    def relay_post(self, priv, offer):
        p = subprocess.run(
            [self.bins["seqob-relaycli"], "post", "--http", self.relay_http,
             "--priv", priv.hex()],
            input=json.dumps(offer), capture_output=True, text=True, check=True)
        return json.loads(p.stdout)

    def relay_take(self, priv, offer, timeout="12s", expect_match=True):
        p = subprocess.run(
            [self.bins["seqob-relaycli"], "take", "--ws", self.relay_ws,
             "--priv", priv.hex(), "--timeout", timeout],
            input=json.dumps(offer), capture_output=True, text=True)
        if expect_match:
            assert p.returncode == 0, "relay take failed: %s" % p.stderr
            return json.loads(p.stdout)
        assert p.returncode != 0, "expected NO match, but take succeeded: %s" % p.stdout
        return None

    def orderbook(self):
        import urllib.request
        url = "%s/v1/market/%s/%s/orderbook" % (self.relay_http, self.A_display, self.B_display)
        with urllib.request.urlopen(url, timeout=5) as r:
            return json.loads(r.read().decode())

    # --- offer builders ----------------------------------------------------

    def now(self):
        return int(time.time())

    def covenant_offer(self, offer_id, txid, vout):
        req_full = ceil_price(N)
        b64 = lambda b: base64.b64encode(b).decode()
        return {
            "offer_id": offer_id, "schema_version": 1,
            "pair": {"base_asset": self.A_display, "quote_asset": self.B_display},
            "trade_dir": "TRADE_DIR_SELL",
            "base_amount": str(N), "offer_amount": str(N), "offer_asset": self.A_display,
            "want_amount": str(req_full), "want_asset": self.B_display,
            "allow_partial": True, "min_fill": str(MIN_LOT),
            "created_at_unix": str(self.now()), "expires_at_unix": str(self.now() + 3600),
            "covenant": {
                "covenant_txid": txid, "covenant_vout": vout,
                "asset_a": self.asset_a.hex(), "asset_b": self.asset_b.hex(),
                "rate_num": str(RATE_NUM), "rate_den": str(RATE_DEN),
                "maker_prog": b64(self.maker_x), "maker_prog_ver": 1,
                "min_lot": str(MIN_LOT), "expiry_locktime": self.expiry,
                "maker_x": b64(self.maker_x), "internal_key": b64(cov.NUMS),
                "merkle_path": [b64(self.order_tap.leaves["refund"].leaf_hash)],
            },
        }

    def buy_offer(self, offer_id, base, offer_b):
        return {
            "offer_id": offer_id, "schema_version": 1,
            "pair": {"base_asset": self.A_display, "quote_asset": self.B_display},
            "trade_dir": "TRADE_DIR_BUY",
            "base_amount": str(base), "want_amount": str(base), "want_asset": self.A_display,
            "offer_amount": str(offer_b), "offer_asset": self.B_display,
            "allow_partial": True,
            "created_at_unix": str(self.now()), "expires_at_unix": str(self.now() + 3600),
            "same_chain": {"maker_recv_address": "taker"},
        }

    # --- on-chain helpers (mirror feature_seqob_covenant_fill.py) -----------

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

    def fund_covenant(self):
        """Fund ONE covenant order UTXO (N of asset A paying order_spk). Returns
        (txid, vout, N)."""
        node = self.nodes[0]
        a_utxo = self.find_asset_utxo(self.A_display, 91)
        a_in = int(satoshi_round(a_utxo["amount"]) * COIN)
        btc = self.fresh_segwit_utxo(1)
        btc_in = int(satoshi_round(btc["amount"]) * COIN)

        tx = CTransaction()
        tx.nVersion = 2
        tx.vin.append(CTxIn(COutPoint(int(a_utxo["txid"], 16), a_utxo["vout"])))
        tx.vin.append(CTxIn(COutPoint(int(btc["txid"], 16), btc["vout"])))
        order_spk = bytes(self.order_tap.scriptPubKey)
        tx.vout.append(self.ctxout(N, order_spk, self.A_OUT))
        tx.vout.append(self.ctxout(a_in - N, self.wallet_spk(), self.A_OUT))
        tx.vout.append(self.ctxout(btc_in - FEE, self.wallet_spk(), self.BTC_OUT))
        tx.vout.append(CTxOut(CTxOutValue(FEE)))
        signed = node.signrawtransactionwithwallet(tx.serialize().hex())
        assert signed["complete"], signed
        txid = node.sendrawtransaction(signed["hex"])
        self.generate(node, 1)
        return txid, 0, N

    def assemble_fill(self, cov_in, wallet_ins, outs, witness):
        """Build a FILL spend: covenant input 0, then wallet inputs; outs =
        [(amount, spk, asset_out or None-for-fee)]; witness = [leaf, control] the
        Go builder emitted (proving production == the proven artifact)."""
        node = self.nodes[0]
        tx = CTransaction()
        tx.nVersion = 2
        tx.vin.append(CTxIn(COutPoint(int(cov_in[0], 16), cov_in[1])))
        for u in wallet_ins:
            tx.vin.append(CTxIn(COutPoint(int(u["txid"], 16), u["vout"])))
        for (amt, spk, aout) in outs:
            if aout is None:
                tx.vout.append(CTxOut(CTxOutValue(amt)))
            else:
                tx.vout.append(self.ctxout(amt, spk, aout))
        partial = node.signrawtransactionwithwallet(tx.serialize().hex())
        tx = tx_from_hex(partial["hex"])
        while len(tx.wit.vtxinwit) < len(tx.vin):
            tx.wit.vtxinwit.append(CTxInWitness())
        tx.wit.vtxinwit[0].scriptWitness.stack = witness
        return tx

    # --- the test ----------------------------------------------------------

    def run_test(self):
        node = self.nodes[0]
        self.bins = self.build_binaries()
        self.start_relay()
        try:
            self._run(node)
        finally:
            self.stop_relay()

    def _run(self, node):
        self.generate(node, 101)
        node.sendtoaddress(address=node.getnewaddress(), amount=1000000,
                           fee_asset_label=BITCOIN_ASSET)
        self.generate(node, 1)

        # Two ordinary explicit assets: A rests in covenant orders, B pays.
        self.A_display = node.issueasset(assetamount=100000, tokenamount=0,
                                          blind=False, fee_asset=BITCOIN_ASSET)["asset"]
        self.generate(node, 1)
        self.B_display = node.issueasset(assetamount=100000, tokenamount=0,
                                          blind=False, fee_asset=BITCOIN_ASSET)["asset"]
        self.generate(node, 1)
        self.A_OUT = self.asset_out(self.A_display)
        self.B_OUT = self.asset_out(self.B_display)
        self.BTC_OUT = b"\x01" + bytes.fromhex(BITCOIN_ASSET)[::-1]
        self.asset_a = bytes.fromhex(self.A_display)[::-1]
        self.asset_b = bytes.fromhex(self.B_display)[::-1]

        maker_sec = generate_privkey()
        self.maker_x = compute_xonly_pubkey(maker_sec)[0]
        maker_spk = bytes(CScript([OP_1, self.maker_x]))
        self.expiry = node.getblockcount() + 300

        self.order_tap, self.fill, self.refund = cov.order_taptree(
            self.asset_a, self.asset_b, RATE_NUM, RATE_DEN, self.maker_x, MIN_LOT,
            self.expiry, self.maker_x)

        # -- byte-equality: Go builder == the proven Python artifact ---------
        self.log.info("verify the Go covenant builder is byte-identical to the proven Python")
        d = self.cov_derive()
        assert_equal(d["scriptpubkey"], bytes(self.order_tap.scriptPubKey).hex())
        assert_equal(d["fill_leaf"], bytes(self.fill).hex())
        assert_equal(d["control_block"], cov.control_block(self.order_tap, "fill").hex())
        assert_equal(d["output_key"], self.order_tap.output_pubkey.hex())

        maker_priv = bytes.fromhex("11" * 32)
        taker_priv = bytes.fromhex("22" * 32)

        # -- fund the covenant + post it (maker then OFFLINE) ----------------
        txid, vout, locked = self.fund_covenant()
        self.log.info("funded covenant UTXO %s:%d (%d units of A)", txid, vout, locked // COIN)
        st = self.relay_post(maker_priv, self.covenant_offer("cov-main", txid, vout))
        assert_equal(st["status"], "OFFER_STATUS_OPEN")
        maker_pubkey = st["maker_pubkey"]
        # No maker process/connection exists past this REST call: the maker is offline.

        # -- SCENARIO: a NON-crossing order is left resting (no match) --------
        self.log.info("matcher leaves a non-crossing order resting (no match)")
        # Bid far below the ask: pays only ceil_price/3 of B for N of A.
        self.relay_take(taker_priv, self.buy_offer("nocross", N, ceil_price(N) // 3),
                        timeout="4s", expect_match=False)
        book = self.orderbook()
        assert any(o["offer_id"] == "cov-main" for o in book["offers"]), \
            "covenant order must still rest after a non-crossing take"

        # -- SCENARIO: the matcher auto-crosses; taker settles, maker OFFLINE -
        self.log.info("matcher auto-crosses a covenant order; taker settles it while the maker is offline")
        m = self.relay_take(taker_priv, self.buy_offer("take-full", N, ceil_price(N)))
        assert_equal(m["resting_is_covenant"], True)
        assert_equal(m["offer_id"], "cov-main")
        assert_equal(m["maker_pubkey"], maker_pubkey)
        assert_equal(int(m["fill_base_amount"]), N)
        assert_equal(int(m["fill_quote_amount"]), ceil_price(N))
        assert_equal(int(m["covenant_locked"]), N)
        assert_equal(m["covenant"]["rate_num"], RATE_NUM)

        # Build the FILL spend from the PRODUCTION Go builder.
        plan = self.cov_fill(locked=N, filled=N, k=0)
        req_full = ceil_price(N)
        assert_equal(int(plan["required_b"]), req_full)
        assert_equal(int(plan["required_b"]), int(m["fill_quote_amount"]))
        assert_equal(plan["fill_leaf"], bytes(self.fill).hex())
        assert_equal(plan["control_block"], cov.control_block(self.order_tap, "fill").hex())
        assert_equal(int(plan["credit_index"]), 0)
        witness = [bytes.fromhex(plan["fill_leaf"]), bytes.fromhex(plan["control_block"])]

        b_in = self.fresh_segwit_utxo(100, self.B_display)
        b_amt = int(satoshi_round(b_in["amount"]) * COIN)
        btc = self.fresh_segwit_utxo(1)
        btc_amt = int(satoshi_round(btc["amount"]) * COIN)
        taker_a_spk = self.wallet_spk()
        tx = self.assemble_fill((txid, vout), [b_in, btc], [
            (req_full, maker_spk, self.B_OUT),                  # 0 credit -> maker (offline)
            (b_amt - req_full, self.wallet_spk(), self.B_OUT),  # 1 taker B change (not A -> full fill)
            (N, taker_a_spk, self.A_OUT),                       # 2 taker A receipt
            (btc_amt - FEE, self.wallet_spk(), self.BTC_OUT),   # 3 btc change
            (FEE, b"", None),                                   # 4 fee
        ], witness)
        ftxid = node.sendrawtransaction(tx.serialize().hex())
        self.generate(node, 1)

        # The OFFLINE maker was paid >= its price in B; the taker received A.
        credit = node.gettxout(ftxid, 0)
        assert_equal(credit["scriptPubKey"]["hex"], maker_spk.hex())
        assert_equal(credit["asset"], self.B_display)
        assert satoshi_round(credit["value"]) * COIN >= Decimal(req_full)
        a_receipt = node.gettxout(ftxid, 2)
        assert_equal(a_receipt["scriptPubKey"]["hex"], taker_a_spk.hex())
        assert_equal(a_receipt["asset"], self.A_display)
        assert_equal(satoshi_round(a_receipt["value"]) * COIN, Decimal(N))
        self.log.info("  maker (offline) credited %s B; taker received %s A",
                      credit["value"], a_receipt["value"])

        # -- SCENARIO: partial fill re-rests the remainder as a valid covenant -
        self.log.info("partial fill re-rests the remainder as a still-valid covenant order")
        txid2, vout2, _ = self.fund_covenant()
        self.relay_post(maker_priv, self.covenant_offer("cov-part", txid2, vout2))

        f1 = 30 * COIN
        mp = self.relay_take(taker_priv, self.buy_offer("take-part", f1, ceil_price(f1)))
        assert_equal(int(mp["fill_base_amount"]), f1)
        assert_equal(int(mp["fill_quote_amount"]), ceil_price(f1))

        plan1 = self.cov_fill(locked=N, filled=f1, k=0)
        assert_equal(plan1["partial"], True)
        assert_equal(int(plan1["remainder_index"]), 1)
        req1 = ceil_price(f1)
        rem1 = N - f1
        assert_equal(int(plan1["required_b"]), req1)
        assert_equal(int(plan1["remainder"]), rem1)
        wit1 = [bytes.fromhex(plan1["fill_leaf"]), bytes.fromhex(plan1["control_block"])]
        order_spk = bytes(self.order_tap.scriptPubKey)

        b_in = self.fresh_segwit_utxo(100, self.B_display)
        b_amt = int(satoshi_round(b_in["amount"]) * COIN)
        btc = self.fresh_segwit_utxo(1)
        btc_amt = int(satoshi_round(btc["amount"]) * COIN)
        tx = self.assemble_fill((txid2, vout2), [b_in, btc], [
            (req1, maker_spk, self.B_OUT),                     # 0 credit
            (rem1, order_spk, self.A_OUT),                     # 1 remainder -> SAME covenant
            (f1, self.wallet_spk(), self.A_OUT),               # 2 taker A receipt
            (b_amt - req1, self.wallet_spk(), self.B_OUT),     # 3 taker B change
            (btc_amt - FEE, self.wallet_spk(), self.BTC_OUT),  # 4 btc change
            (FEE, b"", None),                                  # 5 fee
        ], wit1)
        ptxid = node.sendrawtransaction(tx.serialize().hex())
        self.generate(node, 1)
        rem_out = node.gettxout(ptxid, 1)
        assert_equal(rem_out["scriptPubKey"]["hex"], order_spk.hex())
        assert_equal(rem_out["asset"], self.A_display)
        assert_equal(satoshi_round(rem_out["value"]) * COIN, Decimal(rem1))

        # The remainder is a fresh covenant UTXO: fill it again (proves re-rest).
        self.log.info("  re-filling the %d-unit remainder proves it is a valid covenant order", rem1 // COIN)
        f2 = 20 * COIN
        req2 = ceil_price(f2)
        rem2 = rem1 - f2
        plan2 = self.cov_fill(locked=rem1, filled=f2, k=0)
        assert_equal(int(plan2["required_b"]), req2)
        assert_equal(int(plan2["remainder"]), rem2)
        wit2 = [bytes.fromhex(plan2["fill_leaf"]), bytes.fromhex(plan2["control_block"])]
        b_in = self.fresh_segwit_utxo(100, self.B_display)
        b_amt = int(satoshi_round(b_in["amount"]) * COIN)
        btc = self.fresh_segwit_utxo(1)
        btc_amt = int(satoshi_round(btc["amount"]) * COIN)
        tx = self.assemble_fill((ptxid, 1), [b_in, btc], [
            (req2, maker_spk, self.B_OUT),
            (rem2, order_spk, self.A_OUT),
            (f2, self.wallet_spk(), self.A_OUT),
            (b_amt - req2, self.wallet_spk(), self.B_OUT),
            (btc_amt - FEE, self.wallet_spk(), self.BTC_OUT),
            (FEE, b"", None),
        ], wit2)
        p2txid = node.sendrawtransaction(tx.serialize().hex())
        self.generate(node, 1)
        assert_equal(satoshi_round(node.gettxout(p2txid, 1)["value"]) * COIN, Decimal(rem2))

        self.log.info("SeqOB passive CLOB proven: a covenant order rests, the relay "
                      "auto-matches it, and it settles with the maker OFFLINE.")


if __name__ == "__main__":
    SeqObMatcherCovenantTest().main()
