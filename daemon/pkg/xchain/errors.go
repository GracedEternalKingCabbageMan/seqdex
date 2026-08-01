package xchain

import (
	"errors"
	"fmt"
)

var (
	// errNoSecret is returned when a redeem spend is requested but the
	// HashLock holds only the hash, not the preimage.
	errNoSecret = errors.New("xchain: hashlock has no secret/preimage to build a redeem spend")

	// ErrAnchorOrdering is returned by the orchestrator when the Sequentia leg
	// does NOT satisfy the anchor-shortened ordering rule (its block's
	// anchorheight is below the BTC-leg's confirmation height, or the anchor
	// status is not "ok"). Proceeding would forfeit the reorg-safety that
	// anchoring provides, so the orchestrator refuses.
	ErrAnchorOrdering = errors.New("xchain: SEQ leg violates anchor-shortened ordering (anchorheight < BTC-leg height, or anchor not ok)")

	// ErrAnchorOrderingTerminal is the subset of ErrAnchorOrdering that can NEVER
	// clear by waiting: the SEQ leg's OWN confirming block commits an anchorheight
	// BELOW the BTC-leg height. anchorheight is a committed CBlockHeader field
	// (primitives/block.h m_anchor_height), so for a given block hash it is
	// immutable — re-reading it every poll is futile by construction.
	//
	// The VERDICT is unchanged (still a refusal, and it still wraps
	// ErrAnchorOrdering so every errors.Is(err, ErrAnchorOrdering) caller keeps
	// working). The sentinel exists only so a caller stops burning its timelock
	// window on a hopeless retry loop while the two conjuncts that DO vary
	// (anchorstatus flap, not-yet-certified) keep being retried.
	//
	// It must NOT be "cleared" by re-reading the anchor of a LATER (burying) block
	// or of the chain tip. InvalidateBlock disconnects the offending block and
	// everything ABOVE it while lower blocks stay CONNECTED, so a burying block's
	// anchor says nothing about whether the FUNDING output survives a Bitcoin
	// reorg — only the funding block's own anchor does.
	ErrAnchorOrderingTerminal = fmt.Errorf("%w [TERMINAL: the leg's own block anchors below the BTC leg; a committed header field that can never rise]", ErrAnchorOrdering)

	// ErrBTCLegInvalid is returned by the maker when the taker's claimed BTC leg
	// does not match the quote (wrong H, wrong redeemScript, wrong amount/asset,
	// or it does not pay the expected HTLC P2SH). The maker MUST refuse to lock
	// its SEQ leg in this case.
	ErrBTCLegInvalid = errors.New("xchain: taker BTC leg invalid (does not match quote)")

	// ErrBTCLegUnconfirmed is returned when the taker's BTC leg is not yet
	// confirmed to the required depth. The taker must confirm it before the
	// maker will lock the SEQ leg (the BTC-leg-first ordering rule).
	ErrBTCLegUnconfirmed = errors.New("xchain: taker BTC leg not confirmed to required depth")

	// ErrSEQLegInvalid is the reverse (asset->BTC) mirror of ErrBTCLegInvalid:
	// returned when the taker's funded SEQ asset leg does not match the quote
	// (wrong H, wrong redeemScript, wrong amount/asset, or it does not pay the
	// expected HTLC P2SH). The maker MUST refuse to reveal the secret (claim the
	// SEQ leg) in this case.
	ErrSEQLegInvalid = errors.New("xchain: taker SEQ leg invalid (does not match quote)")

	// ErrSEQLegUnconfirmed is the reverse mirror of ErrBTCLegUnconfirmed: the
	// taker's SEQ asset leg is not yet confirmed to the required depth.
	ErrSEQLegUnconfirmed = errors.New("xchain: taker SEQ leg not confirmed to required depth")

	// ErrLNLegInvalid means the Lightning leg of a submarine swap does not match
	// the swap (invoice hash != H, wrong amount, or a revealed preimage that does
	// not hash to H). The maker MUST NOT proceed.
	ErrLNLegInvalid = errors.New("xchain: submarine LN leg invalid (does not match swap)")

	// ErrLNLegTimeout means a Lightning-leg wait (e.g. a hold invoice never
	// reaching "accepted") exceeded its deadline; fall back to the refund path.
	ErrLNLegTimeout = errors.New("xchain: submarine LN leg timed out")
)
