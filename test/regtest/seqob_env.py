#!/usr/bin/env python3
# Copyright (c) 2026 The Sequentia developers
# Distributed under the MIT software license.
"""Locate the two checkouts these regtest proofs straddle, and put the Sequentia
node's functional-test framework on sys.path.

Import this FIRST, before any `test_framework.*` import:

    import seqob_env  # noqa: F401  (must precede the test_framework imports)
    from test_framework.test_framework import BitcoinTestFramework

The proofs live here because they test THIS repo's code (the covenant builder,
the seqobd relay and its matcher, the settler, the watcher, the bridge). They
drive it against a real Sequentia node, so they also need the node repo's
`test_framework` package and its built binary. `test_framework` finds its own
`test/config.ini` relative to its source file, so pointing sys.path at a
CONFIGURED, BUILT node checkout is the only wiring needed.

  SEQUENTIA_DIR  the Sequentia node checkout   (default ~/Sequentia)
  SEQDEX_DIR     this repo                     (default: derived from __file__)
  GO_BIN         the Go toolchain              (default ~/dev-tools/go/bin/go)
"""
import os
import sys


def seqdex_dir():
    """This repo's root. Derived from our own location; env overrides."""
    here = os.path.dirname(os.path.abspath(__file__))
    return os.environ.get("SEQDEX_DIR", os.path.dirname(os.path.dirname(here)))


def node_dir():
    """A configured, built Sequentia node checkout."""
    return os.environ.get("SEQUENTIA_DIR", os.path.expanduser("~/Sequentia"))


def go_bin():
    return os.environ.get("GO_BIN", os.path.expanduser("~/dev-tools/go/bin/go"))


_functional = os.path.join(node_dir(), "test", "functional")
if not os.path.isdir(_functional):
    raise SystemExit(
        "Sequentia node functional tests not found at %s.\n"
        "Set SEQUENTIA_DIR to a configured, built node checkout "
        "(https://github.com/ConcatenaLabs/Sequentia)." % _functional)

# Our own directory first, so `import seqob_covenant` resolves here and not to a
# same-named module in the node checkout.
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
sys.path.insert(1, _functional)
