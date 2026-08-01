package wallet

import (
	"testing"

	"github.com/aejkcs50/seqdex/daemon/internal/core/ports"
)

// staticInfoStub is the minimum WalletInfo feeExchangeRate needs: the native
// asset id. Everything else is unused on this path.
type staticInfoStub string

func (s staticInfoStub) GetNetwork() string                 { return "regtest" }
func (s staticInfoStub) GetNativeAsset() string             { return string(s) }
func (s staticInfoStub) GetRootPath() string                { return "" }
func (s staticInfoStub) GetBirthdayBlockHash() string       { return "" }
func (s staticInfoStub) GetBirthdayBlockHeight() uint32     { return 0 }
func (s staticInfoStub) GetAccounts() []ports.WalletAccount { return nil }

// fakeRates is a stand-in fee-market view.
type fakeRates struct{ m map[string]uint64 }

func (f fakeRates) FeeExchangeRate(asset string) (uint64, bool) {
	r, ok := f.m[asset]
	return r, ok
}

// THE NATIVE ASSET IS NOT 1:1.
//
// feeExchangeRate used to return exchange_rate_scale for the native asset
// unconditionally. That was a hardcoded privilege, and it under-paid fees by
// exactly the factor between par and the asset's real published rate.
//
// On the live testnet the policy asset publishes at ~3.76e7, because one unit of
// it is worth about 0.376 reference units. A caller sizing through a hardcoded
// 1:1 therefore believed each atom was worth 2.66x what the node credits. Since
// the node's relay test is fee_atoms * rate / 1e8 >= minRelayTxFee, the fee it
// built cleared the floor by only a hair — 1.1x measured on the fleet — and any
// further drift would have put fixed-rate payers under it fleet-wide.
func TestNativeAssetUsesThePublishedRateNotPar(t *testing.T) {
	const native = "policyassethex"
	const livePolicyRate = 37_624_824 // the real published rate, not 1e8

	s := &Service{
		staticInfo: staticInfoStub(native),
		rates:      fakeRates{m: map[string]uint64{native: livePolicyRate}},
	}

	rate, ok := s.feeExchangeRate(native)
	if !ok {
		t.Fatal("the native asset must stay fee-eligible when the node prices it")
	}
	if rate == exchangeRateScale {
		t.Fatalf("native asset was valued at par (%d); it must use the PUBLISHED rate, "+
			"or every fee sized through it is over-valued by 1e8/rate", exchangeRateScale)
	}
	if rate != livePolicyRate {
		t.Fatalf("rate = %d, want the published %d", rate, livePolicyRate)
	}
}

// A non-native asset is unchanged: the published rate, or ineligible.
func TestNonNativeAssetIsRateGated(t *testing.T) {
	const native = "policyassethex"
	const gold = "goldhex"

	s := &Service{
		staticInfo: staticInfoStub(native),
		rates:      fakeRates{m: map[string]uint64{gold: 414_440_000_000}},
	}
	if rate, ok := s.feeExchangeRate(gold); !ok || rate != 414_440_000_000 {
		t.Fatalf("gold: rate=%d ok=%v", rate, ok)
	}
	if _, ok := s.feeExchangeRate("unlistedhex"); ok {
		t.Fatal("an unlisted asset must not be fee-eligible")
	}
}

// The 1:1 assumption survives ONLY where there is no table to consult.
func TestParIsTheNoNodeRpcFallbackOnly(t *testing.T) {
	const native = "policyassethex"

	s := &Service{staticInfo: staticInfoStub(native), rates: nil}
	if rate, ok := s.feeExchangeRate(native); !ok || rate != exchangeRateScale {
		t.Fatalf("with no fee-market view the native asset must size at par: rate=%d ok=%v", rate, ok)
	}
	if _, ok := s.feeExchangeRate("goldhex"); ok {
		t.Fatal("with no fee-market view nothing but the native asset is eligible")
	}

	// A configured table that simply does not list the native asset keeps par as
	// a last resort, so a node whose sidecar is briefly down can still size a fee
	// — but that is a fallback, never a privilege extended to other assets.
	s2 := &Service{staticInfo: staticInfoStub(native), rates: fakeRates{m: map[string]uint64{}}}
	if rate, ok := s2.feeExchangeRate(native); !ok || rate != exchangeRateScale {
		t.Fatalf("unlisted native: rate=%d ok=%v", rate, ok)
	}
	if _, ok := s2.feeExchangeRate("goldhex"); ok {
		t.Fatal("an unlisted non-native asset gets no such fallback")
	}
}
