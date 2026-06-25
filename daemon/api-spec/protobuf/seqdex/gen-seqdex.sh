#!/usr/bin/env bash
# Regenerate the Go bindings for the SeqDEX seqdex.v1 API surface into
# ../gen/seqdex/v1. This covers BOTH:
#
#   * the public same-chain Trade/Swap/Transport API (trade.proto, swap.proto,
#     transport.proto, types.proto) — Phase 6a, the seqdex.v1 successor to the
#     upstream tdex.v2 TradeService; AND
#   * the cross-chain swap API (xchain.proto) — Phase 5 m2 / Phase 6b.
#
# All live in package seqdex.v1 and are generated together so their shared
# google.api annotations and the grpc-gateway REST bindings are produced in one
# pass.
#
# The daemon's main buf.gen.yaml uses LOCAL protoc-gen-go* plugins; this script
# instead uses buf REMOTE plugins pinned to the exact versions the daemon module
# depends on (protobuf v1.30.0 / grpc-go v1.3.0 / grpc-gateway v2.15.2), so no
# local protoc toolchain is required. buf is expected at ~/.local/bin/buf.
#
# Usage:  bash gen-seqdex.sh
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"          # .../protobuf/seqdex
PROTOBUF_DIR="$(cd "$HERE/.." && pwd)"                         # .../protobuf
BUF="${BUF:-$HOME/.local/bin/buf}"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
mkdir -p "$WORK/seqdex/v1"
cp "$HERE/v1/"*.proto "$WORK/seqdex/v1/"

cat > "$WORK/buf.yaml" <<'YAML'
version: v2
modules:
  - path: .
deps:
  - buf.build/googleapis/googleapis
lint:
  use:
    - STANDARD
  except:
    - PACKAGE_VERSION_SUFFIX
    - ENUM_ZERO_VALUE_SUFFIX
    - RPC_REQUEST_STANDARD_NAME
    - RPC_RESPONSE_STANDARD_NAME
    - RPC_REQUEST_RESPONSE_UNIQUE
YAML

cat > "$WORK/buf.gen.yaml" <<'YAML'
version: v2
managed:
  enabled: true
  override:
    - file_option: go_package_prefix
      value: github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen
  disable:
    - module: buf.build/googleapis/googleapis
plugins:
  - remote: buf.build/protocolbuffers/go:v1.30.0
    out: gen
    opt: paths=source_relative
  - remote: buf.build/grpc/go:v1.3.0
    out: gen
    opt:
      - paths=source_relative
      - require_unimplemented_servers=false
  - remote: buf.build/grpc-ecosystem/gateway:v2.15.2
    out: gen
    opt:
      - paths=source_relative
YAML

( cd "$WORK" && "$BUF" dep update && "$BUF" lint && "$BUF" generate )

for f in trade swap transport types xchain; do
  for suffix in pb.go _grpc.pb.go pb.gw.go; do
    src="$WORK/gen/seqdex/v1/${f}${suffix}"
    if [ -f "$src" ]; then
      cp "$src" "$PROTOBUF_DIR/gen/seqdex/v1/${f}${suffix}"
    fi
  done
done
echo "regenerated $PROTOBUF_DIR/gen/seqdex/v1/{trade,swap,transport,types,xchain}.{pb,_grpc.pb,pb.gw}.go"
ls -1 "$PROTOBUF_DIR/gen/seqdex/v1/"
