#!/usr/bin/env python3
# Copyright (c) 2026 The Sequentia developers
# Distributed under the MIT software license.
"""Emit a DETERMINISTIC golden vector for the SeqOB covenant REFUND spend.

This pins the lwk_wasm `buildCovenantRefundTx` helper to the SAME taproot
tapscript sighash + witness the regtest-proven Python refund spend
(feature_seqob_covenant_fill.py, build_refund) produces, WITHOUT a node: all the
pieces (leaf bytes, leaf hash, control block, TaprootSignatureHash, sign_schnorr)
are pure Python. Fixed inputs -> a reproducible vector the Rust golden test
(lwk_wollet/src/seqob_covenant.rs) asserts byte-for-byte.

Run:  python3 test/functional/gen_refund_golden.py
"""
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import seqob_env  # noqa: E402,F401  (puts the node's test_framework on sys.path)

import seqob_covenant as cov
from test_framework.key import compute_xonly_pubkey, sign_schnorr
from test_framework.messages import (
    COutPoint, CTransaction, CTxIn, CTxInWitness, CTxOut, CTxOutAsset,
    CTxOutValue, CTxOutWitness, uint256_from_str,
)
from test_framework.script import CScript, OP_1, TaprootSignatureHash


def h2b(s):
    return bytes.fromhex(s)


def asset_out(display_hex):
    # explicit-asset prefix 0x01 + 32-byte internal-order id
    return b"\x01" + h2b(display_hex)[::-1]


def ctxout(amount, spk, aout):
    return CTxOut(nValue=CTxOutValue(amount), scriptPubKey=spk, nAsset=CTxOutAsset(aout))


def main():
    # ---- FIXED order parameters (a real maker keypair; not the 0x22*32 dummy) ----
    maker_sec = h2b("2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b")
    maker_x = compute_xonly_pubkey(maker_sec)[0]

    asset_a_display = "a1" * 32           # the resting (locked) asset A
    fee_asset_display = "fe" * 32         # a DISTINCT fee asset the maker funds
    asset_a = h2b(asset_a_display)[::-1]  # internal order
    rate_num, rate_den, min_lot = 3, 7, 500_000_000
    maker_prog = maker_x                  # v1 taproot payout program (unused by REFUND)
    expiry = 400                          # absolute-locktime CLTV height

    order_tap, fill, refund = cov.order_taptree(
        asset_a, h2b("b2" * 32), rate_num, rate_den, maker_prog, min_lot, expiry, maker_x)

    order_spk = bytes(order_tap.scriptPubKey)
    refund_leaf = bytes(refund)
    refund_cb = cov.control_block(order_tap, "refund")

    # ---- FIXED refund transaction ----
    N = 1_000_000_000                     # locked atoms of asset A
    fee = 5000
    fee_in_val = 40000                    # a fee-asset funding input (change = 35000)
    genesis_display = "00" * 31 + "42"    # a fixed 32-byte genesis block hash (display order)
    genesis = uint256_from_str(h2b(genesis_display)[::-1])

    cov_txid = "11" * 32
    fee_txid = "22" * 32
    # All wallet-controlled scriptPubKeys are p2wpkh (BIP84) — the covenant fee
    # input is signed key-path, and the reclaim/change addresses are the maker's.
    reclaim_spk = h2b("0014" + "33" * 20)      # where reclaimed A goes (maker addr)
    change_spk = h2b("0014" + "44" * 20)       # fee-asset change back to the maker
    fee_prevout_spk = h2b("0014" + "14" * 20)  # the maker's fee-funding p2wpkh coin

    tx = CTransaction()
    tx.nVersion = 2
    tx.nLockTime = expiry
    tx.vin.append(CTxIn(COutPoint(int(cov_txid, 16), 0), nSequence=0xfffffffe))
    tx.vin.append(CTxIn(COutPoint(int(fee_txid, 16), 1), nSequence=0xfffffffe))
    tx.vout.append(ctxout(N, reclaim_spk, asset_out(asset_a_display)))          # 0: A back to maker
    tx.vout.append(ctxout(fee_in_val - fee, change_spk, asset_out(fee_asset_display)))  # 1: fee-asset change
    tx.vout.append(CTxOut(CTxOutValue(fee), nAsset=CTxOutAsset(asset_out(fee_asset_display))))  # 2: explicit fee

    spent = [
        ctxout(N, order_spk, asset_out(asset_a_display)),                 # covenant prevout (input 0)
        ctxout(fee_in_val, fee_prevout_spk, asset_out(fee_asset_display)),  # fee prevout (input 1)
    ]

    # The Elements taproot sighash commits to one (empty) output witness per
    # output; a manually-built tx must carry them (the node/consensus does).
    while len(tx.wit.vtxinwit) < len(tx.vin):
        tx.wit.vtxinwit.append(CTxInWitness())
    while len(tx.wit.vtxoutwit) < len(tx.vout):
        tx.wit.vtxoutwit.append(CTxOutWitness())

    # covenant input (index 0) tapscript sighash, SIGHASH_DEFAULT (hash_type 0)
    msg = TaprootSignatureHash(tx, spent, 0, genesis, 0, scriptpath=True, script=refund)
    sig = sign_schnorr(maker_sec, msg)   # aux = zeros -> deterministic (== rust no_aux_rand)

    while len(tx.wit.vtxinwit) < len(tx.vin):
        tx.wit.vtxinwit.append(CTxInWitness())
    tx.wit.vtxinwit[0].scriptWitness.stack = cov.refund_witness(order_tap, refund, sig)

    print("// ---- SeqOB covenant REFUND golden vector (gen_refund_golden.py) ----")
    print('maker_sec        = "%s"' % maker_sec.hex())
    print('maker_x          = "%s"' % maker_x.hex())
    print('asset_a_display  = "%s"' % asset_a_display)
    print('fee_asset_display= "%s"' % fee_asset_display)
    print('genesis_display  = "%s"' % genesis_display)
    print('order_spk        = "%s"' % order_spk.hex())
    print('refund_leaf      = "%s"' % refund_leaf.hex())
    print('refund_leaf_hash = "%s"' % order_tap.leaves["refund"].leaf_hash.hex())
    print('refund_cb        = "%s"' % refund_cb.hex())
    print('fee_prevout_spk  = "%s"' % fee_prevout_spk.hex())
    print('sighash          = "%s"' % msg.hex())
    print('signature        = "%s"' % sig.hex())
    print("N                =", N)
    print("fee              =", fee)
    print("fee_in_val       =", fee_in_val)
    print("expiry           =", expiry)
    print("reclaim_spk      =", reclaim_spk.hex())
    print("change_spk       =", change_spk.hex())


if __name__ == "__main__":
    main()
