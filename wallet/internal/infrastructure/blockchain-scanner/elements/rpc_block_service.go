package elements_scanner

import (
	"fmt"
	"strings"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/vulpemventures/go-elements/block"
	"github.com/vulpemventures/go-elements/transaction"
	"github.com/vulpemventures/neutrino-elements/pkg/blockservice"
)

// rpcBlockService implements blockservice.BlockService backed by an Elements/
// Sequentia node JSON-RPC connection. It is the node-RPC-only alternative to
// the Esplora-backed block service, removing the dependency on an external
// Esplora HTTP endpoint.
//
// SEQUENTIA: it does NOT use go-elements' raw block deserializer
// (block.NewFromBuffer). That deserializer cannot parse anchored Sequentia
// block headers (the 36-byte m_anchor_height/m_anchor_hash fields plus the PoS
// committee signblock witness are unknown to it; see header_repo.go and
// src/primitives/block.h). Instead we let the node parse its own format via
// `getblock <hash> 2` (verbosity 2, JSON containing each transaction's raw
// hex), and reconstruct the *block.Block the scanner consumes:
//   - Header.Height / Header.Timestamp from the JSON block fields, and
//   - TransactionsData from go-elements transaction.NewTxFromHex on each tx hex
//     (Sequentia txs are standard Elements and parse fine).
// The scanner only reads block.TransactionsData.Transactions and
// block.Header.Height (see neutrino-elements scanner.extractBlockMatches), so a
// header carrying those fields is sufficient.
type rpcBlockService struct {
	rpcClient *rpcClient
}

var _ blockservice.BlockService = (*rpcBlockService)(nil)

// NewRpcBlockService returns a blockservice.BlockService that fetches blocks
// from the node via `getblock <hash> 2` (JSON incl. each tx's raw hex).
func NewRpcBlockService(rpcClient *rpcClient) blockservice.BlockService {
	return &rpcBlockService{rpcClient: rpcClient}
}

func (b *rpcBlockService) GetBlock(
	hash *chainhash.Hash,
) (*block.Block, error) {
	// verbosity 2 -> JSON block with full (hex-serialized) transactions.
	resp, err := b.rpcClient.call(
		"getblock", []interface{}{hash.String(), 2},
	)
	if err != nil {
		if isNotFoundErr(err) {
			return nil, blockservice.ErrorBlockNotFound
		}
		return nil, err
	}

	m, ok := resp.(map[string]interface{})
	if !ok {
		return nil, blockservice.ErrorBlockNotFound
	}

	var height uint32
	if v, ok := m["height"].(float64); ok {
		height = uint32(v)
	}
	var timestamp uint32
	if v, ok := m["time"].(float64); ok {
		timestamp = uint32(v)
	}

	rawTxs, ok := m["tx"].([]interface{})
	if !ok {
		return nil, fmt.Errorf("getblock response missing tx array")
	}

	txs := make([]*transaction.Transaction, 0, len(rawTxs))
	for _, raw := range rawTxs {
		txMap, ok := raw.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("unexpected tx entry type %T in getblock", raw)
		}
		txHex, ok := txMap["hex"].(string)
		if !ok {
			return nil, fmt.Errorf("getblock tx entry missing hex (need verbosity 2)")
		}
		tx, err := transaction.NewTxFromHex(txHex)
		if err != nil {
			return nil, fmt.Errorf("failed to parse block tx: %w", err)
		}
		txs = append(txs, tx)
	}

	return &block.Block{
		Header: &block.Header{
			Height:    height,
			Timestamp: timestamp,
		},
		TransactionsData: &block.Transactions{
			Transactions: txs,
		},
	}, nil
}

func isNotFoundErr(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "not found") ||
		strings.Contains(msg, "block not found")
}
