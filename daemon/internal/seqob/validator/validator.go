// Package validator validates SeqOB offers before they enter the book.
//
// Checks: signature, schema, amounts > 0, known assets, trade-direction/asset
// consistency, a mandatory non-absurd expiry (short self-expiry so a suppressed
// cancel self-heals — review M3), and per-maker_pubkey + per-IP rate limits.
//
// It does NOT enforce a min_anchor_depth floor: per the project policy override
// the DEX is 0-conf tolerant and min_anchor_depth is a maker-only dial that
// defaults to 0. The liveness probe is an interface with a no-op default and is
// explicitly a Phase-1 stub (confidential balances make a precise probe
// impossible; atomic co-signing is the real safety net).
package validator

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/offer"
)

// LivenessProbe checks (best-effort, read-only) that a maker plausibly holds the
// asset it is offering. PHASE-1 STUB: the default NoopLivenessProbe always
// passes. A later implementation would query a read-only Sequentia node RPC.
type LivenessProbe interface {
	CheckOffer(ctx context.Context, o *seqobv1.Offer) error
}

// NoopLivenessProbe is the Phase-1 default: it accepts every offer.
type NoopLivenessProbe struct{}

// CheckOffer always returns nil (Phase-1 stub).
func (NoopLivenessProbe) CheckOffer(context.Context, *seqobv1.Offer) error { return nil }

// Config tunes validation.
type Config struct {
	// SchemaVersion the relay accepts (default 1).
	SchemaVersion uint32
	// KnownAssets, if non-empty, restricts offers to these asset ids (hex). Empty
	// = accept any asset (registry wiring is deferred).
	KnownAssets map[string]bool
	// MinExpiry / MaxExpiry bound how soon / far an offer may expire from now.
	// Expiry is mandatory: an offer with no future expiry is rejected.
	MinExpiry time.Duration
	MaxExpiry time.Duration
	// Rate limits (sliding 1-minute window).
	MaxOffersPerMinPerPubkey int
	MaxOffersPerMinPerIP     int
	// Now is the clock (default time.Now).
	Now func() time.Time
}

// DefaultConfig returns sane Phase-1 defaults.
func DefaultConfig() Config {
	return Config{
		SchemaVersion:            1,
		MinExpiry:                30 * time.Second,
		MaxExpiry:                7 * 24 * time.Hour,
		MaxOffersPerMinPerPubkey: 60,
		MaxOffersPerMinPerIP:     120,
		Now:                      time.Now,
	}
}

// Validator validates offers and enforces rate limits.
type Validator struct {
	cfg     Config
	probe   LivenessProbe
	mu      sync.Mutex
	pubHits map[string][]time.Time
	ipHits  map[string][]time.Time
}

// New returns a Validator. If probe is nil, NoopLivenessProbe is used.
func New(cfg Config, probe LivenessProbe) *Validator {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.SchemaVersion == 0 {
		cfg.SchemaVersion = 1
	}
	if probe == nil {
		probe = NoopLivenessProbe{}
	}
	return &Validator{
		cfg:     cfg,
		probe:   probe,
		pubHits: make(map[string][]time.Time),
		ipHits:  make(map[string][]time.Time),
	}
}

// ValidateOffer runs all structural/semantic checks and rate limits. ip may be
// empty (e.g. for a local CLI); IP rate limiting is then skipped.
func (v *Validator) ValidateOffer(ctx context.Context, o *seqobv1.Offer, ip string) error {
	if err := offer.VerifyOffer(o); err != nil {
		return fmt.Errorf("signature: %w", err)
	}
	if o.GetSchemaVersion() != v.cfg.SchemaVersion {
		return fmt.Errorf("unsupported schema_version %d (want %d)", o.GetSchemaVersion(), v.cfg.SchemaVersion)
	}
	if err := v.checkTerms(o); err != nil {
		return err
	}
	if err := v.checkExpiry(o); err != nil {
		return err
	}
	if err := v.checkRate(o, ip); err != nil {
		return err
	}
	if err := v.probe.CheckOffer(ctx, o); err != nil {
		return fmt.Errorf("liveness: %w", err)
	}
	return nil
}

func (v *Validator) checkTerms(o *seqobv1.Offer) error {
	p := o.GetPair()
	if p == nil || p.GetBaseAsset() == "" || p.GetQuoteAsset() == "" {
		return fmt.Errorf("offer missing pair assets")
	}
	if p.GetBaseAsset() == p.GetQuoteAsset() {
		return fmt.Errorf("base and quote asset are identical")
	}
	if o.GetBaseAmount() == 0 || o.GetOfferAmount() == 0 || o.GetWantAmount() == 0 {
		return fmt.Errorf("amounts must be > 0")
	}
	if o.GetOfferAsset() == "" || o.GetWantAsset() == "" {
		return fmt.Errorf("offer/want asset missing")
	}
	// Direction/asset consistency: a SELL gives base and wants quote (and
	// offer_amount is the base side); a BUY gives quote and wants base (and
	// want_amount is the base side).
	switch o.GetTradeDir() {
	case seqobv1.TradeDir_TRADE_DIR_SELL:
		if o.GetOfferAsset() != p.GetBaseAsset() || o.GetWantAsset() != p.GetQuoteAsset() {
			return fmt.Errorf("SELL must offer base for quote")
		}
		if o.GetOfferAmount() != o.GetBaseAmount() {
			return fmt.Errorf("SELL offer_amount must equal base_amount")
		}
	case seqobv1.TradeDir_TRADE_DIR_BUY:
		if o.GetOfferAsset() != p.GetQuoteAsset() || o.GetWantAsset() != p.GetBaseAsset() {
			return fmt.Errorf("BUY must offer quote for base")
		}
		if o.GetWantAmount() != o.GetBaseAmount() {
			return fmt.Errorf("BUY want_amount must equal base_amount")
		}
	default:
		return fmt.Errorf("unspecified trade_dir")
	}
	if o.GetMinFill() > o.GetBaseAmount() {
		return fmt.Errorf("min_fill exceeds base_amount")
	}
	if len(v.cfg.KnownAssets) > 0 {
		for _, a := range []string{p.GetBaseAsset(), p.GetQuoteAsset(), o.GetOfferAsset(), o.GetWantAsset()} {
			if !v.cfg.KnownAssets[strings.ToLower(a)] && !v.cfg.KnownAssets[a] {
				return fmt.Errorf("unknown asset %q", a)
			}
		}
	}
	return nil
}

func (v *Validator) checkExpiry(o *seqobv1.Offer) error {
	now := v.cfg.Now()
	exp := o.GetExpiresAtUnix()
	if exp == 0 {
		return fmt.Errorf("expires_at_unix is mandatory")
	}
	t := time.Unix(int64(exp), 0)
	if !t.After(now) {
		return fmt.Errorf("offer already expired")
	}
	if v.cfg.MinExpiry > 0 && t.Before(now.Add(v.cfg.MinExpiry)) {
		return fmt.Errorf("expiry too soon (min %s)", v.cfg.MinExpiry)
	}
	if v.cfg.MaxExpiry > 0 && t.After(now.Add(v.cfg.MaxExpiry)) {
		return fmt.Errorf("expiry too far in the future (max %s)", v.cfg.MaxExpiry)
	}
	return nil
}

func (v *Validator) checkRate(o *seqobv1.Offer, ip string) error {
	now := v.cfg.Now()
	cutoff := now.Add(-time.Minute)
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.cfg.MaxOffersPerMinPerPubkey > 0 {
		hits := prune(v.pubHits[o.GetMakerPubkey()], cutoff)
		if len(hits) >= v.cfg.MaxOffersPerMinPerPubkey {
			v.pubHits[o.GetMakerPubkey()] = hits
			return fmt.Errorf("rate limit: too many offers for this maker_pubkey")
		}
		v.pubHits[o.GetMakerPubkey()] = append(hits, now)
	}
	if ip != "" && v.cfg.MaxOffersPerMinPerIP > 0 {
		hits := prune(v.ipHits[ip], cutoff)
		if len(hits) >= v.cfg.MaxOffersPerMinPerIP {
			v.ipHits[ip] = hits
			return fmt.Errorf("rate limit: too many offers from this IP")
		}
		v.ipHits[ip] = append(hits, now)
	}
	return nil
}

// ValidateCancel verifies a signed cancel's signature. (Nonce/replay enforcement
// is the offerstore's job, since it holds the per-key nonce high-water mark.)
func (v *Validator) ValidateCancel(c *seqobv1.OfferCancel) error {
	return offer.VerifyCancel(c)
}

func prune(hits []time.Time, cutoff time.Time) []time.Time {
	out := hits[:0]
	for _, h := range hits {
		if h.After(cutoff) {
			out = append(out, h)
		}
	}
	return out
}
