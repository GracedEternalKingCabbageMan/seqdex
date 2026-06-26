package client

import (
	"fmt"

	"google.golang.org/protobuf/proto"

	seqdexv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqdex/v1"
	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
)

// Taker drives the proposer side of a lift. It builds the SwapRequest, seals it
// for the relay courier, and finalizes the SwapAccept it gets back. The relay
// only ever moves the sealed bytes (SwapMsg.ciphertext), never the plaintext.
type Taker struct {
	Wallet Wallet
}

// Propose builds and seals the SwapRequest for an offer lift. It returns both the
// sealed bytes to courier and the plaintext SwapRequest (for the caller's logs).
func (t *Taker) Propose(o *seqobv1.Offer, takeBase uint64, feeAsset string, c *Crypter) (sealed []byte, req *seqdexv1.SwapRequest, err error) {
	req, err = t.Wallet.ProposerBuildRequest(o, takeBase, feeAsset)
	if err != nil {
		return nil, nil, fmt.Errorf("build request: %w", err)
	}
	pt, err := proto.Marshal(req)
	if err != nil {
		return nil, nil, err
	}
	sealed, err = c.Seal(pt)
	if err != nil {
		return nil, nil, err
	}
	return sealed, req, nil
}

// Finalize opens the maker's sealed SwapAccept, blinds+signs+broadcasts via the
// wallet, and returns the sealed SwapComplete plus the broadcast txid.
func (t *Taker) Finalize(sealedAccept []byte, c *Crypter) (sealedComplete []byte, txid string, err error) {
	pt, err := c.Open(sealedAccept)
	if err != nil {
		return nil, "", fmt.Errorf("open accept: %w", err)
	}
	var acc seqdexv1.SwapAccept
	if err := proto.Unmarshal(pt, &acc); err != nil {
		return nil, "", fmt.Errorf("unmarshal accept: %w", err)
	}
	complete, txid, err := t.Wallet.ProposerFinalize(&acc)
	if err != nil {
		return nil, "", fmt.Errorf("finalize: %w", err)
	}
	ct, err := proto.Marshal(complete)
	if err != nil {
		return nil, "", err
	}
	sealedComplete, err = c.Seal(ct)
	if err != nil {
		return nil, "", err
	}
	return sealedComplete, txid, nil
}

// Maker drives the responder side: it opens the taker's sealed SwapRequest, runs
// the existing CompleteSwap path, and seals the SwapAccept back.
type Maker struct {
	Wallet Wallet
}

// HandleRequest opens a sealed SwapRequest, runs ResponderComplete, and returns
// the sealed SwapAccept.
func (m *Maker) HandleRequest(sealedReq []byte, c *Crypter) (sealedAccept []byte, err error) {
	pt, err := c.Open(sealedReq)
	if err != nil {
		return nil, fmt.Errorf("open request: %w", err)
	}
	var req seqdexv1.SwapRequest
	if err := proto.Unmarshal(pt, &req); err != nil {
		return nil, fmt.Errorf("unmarshal request: %w", err)
	}
	acc, err := m.Wallet.ResponderComplete(&req)
	if err != nil {
		return nil, fmt.Errorf("responder complete: %w", err)
	}
	at, err := proto.Marshal(acc)
	if err != nil {
		return nil, err
	}
	sealedAccept, err = c.Seal(at)
	if err != nil {
		return nil, err
	}
	return sealedAccept, nil
}
