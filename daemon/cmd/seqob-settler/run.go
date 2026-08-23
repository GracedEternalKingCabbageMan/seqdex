package main

import (
	"flag"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/matcher"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/settler"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/watcher"
	"github.com/aejkcs50/seqdex/daemon/pkg/covenant"
	"github.com/aejkcs50/seqdex/daemon/pkg/xchain"
	"github.com/vulpemventures/go-elements/elementsutil"
)

var runUnmarshal = protojson.UnmarshalOptions{DiscardUnknown: true}

// runSettler is the always-online loop. Because the relay does NOT route
// both-covenant matches to a settler (api/matched.go only advertises the resting
// side), the settler DISCOVERS crosses locally: it polls each covenant market's
// public orderbook, pairs opposite-side covenant offers whose integer prices
// cross (mirroring the relay matcher), reads both covenant UTXOs from the chain,
// and settles each pair in one joint FILL tx it assembles + broadcasts. It holds
// no user funds: the covenants' FILL leaves enforce every maker credit, so a
// wrong assembly is simply rejected by the chain.
func runSettler(args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	relay := fs.String("relay", env("SEQOB_RELAY", "http://127.0.0.1:9955"), "relay base URL (env SEQOB_RELAY)")
	nodeRPC := fs.String("node-rpc", env("SEQOB_NODE_RPC", ""), "Sequentia node RPC host:port (env SEQOB_NODE_RPC)")
	nodeUser := fs.String("node-rpc-user", env("SEQOB_NODE_RPC_USER", ""), "node JSON-RPC user (env SEQOB_NODE_RPC_USER)")
	nodePass := fs.String("node-rpc-pass", env("SEQOB_NODE_RPC_PASS", ""), "node JSON-RPC pass (env SEQOB_NODE_RPC_PASS)")
	feeWallet := fs.String("fee-wallet", env("SEQOB_SETTLER_WALLET", ""), "node wallet holding the settler's bitcoin for fee+gap (env SEQOB_SETTLER_WALLET)")
	gapValue := fs.Uint64("gap-value", 10000, "atoms for each full-fill bitcoin gap output (settler-owned, returned to the settler)")
	feeAtoms := fs.Uint64("fee", 5000, "explicit network fee per joint tx, in atoms of the fee asset (raise if the node rejects for low fee)")
	feeAsset := fs.String("fee-asset", env("SEQOB_SETTLER_FEE_ASSET", ""), "asset the settler pays fees and gap outputs in (display hex; default: the chain's pegged bitcoin asset). Any asset the node accepts for fees; the native asset is not privileged")
	pollInterval := fs.Duration("poll-interval", 3*time.Second, "orderbook poll interval")
	markets := fs.String("markets", env("SEQOB_SETTLER_MARKETS", ""), "comma-separated base/quote pairs to scope, e.g. gold/usdx,silvr/usdx (default: all relay markets)")
	_ = fs.Parse(args)

	if *nodeRPC == "" {
		die("run: -node-rpc host:port is required")
	}
	if *feeWallet == "" {
		die("run: -fee-wallet is required (a node wallet holding the settler's bitcoin for fee+gap)")
	}
	host, port, err := splitHostPort(*nodeRPC)
	if err != nil {
		die("run: bad -node-rpc %q: %v", *nodeRPC, err)
	}

	// Two node views: a wallet-scoped Chain for funding/signing/broadcast, and a
	// keyless read-only ChainView (the relay watcher's client) for covenant reads.
	rpc := xchain.NewRPC(host, port, *nodeUser, *nodePass)
	chain := xchain.NewChain(rpc, *feeWallet)
	view := watcher.NewRPCChain(host, port, *nodeUser, *nodePass)

	pegged, err := chain.PeggedAsset()
	if err != nil {
		die("run: getsidechaininfo (pegged bitcoin asset): %v", err)
	}
	if *feeAsset != "" {
		pegged = strings.ToLower(*feeAsset)
	}
	peggedInternal, err := internalAsset(pegged)
	if err != nil {
		die("run: pegged asset %q: %v", pegged, err)
	}
	funder, err := settler.NewNodeFunder(chain, pegged, *feeAtoms)
	if err != nil {
		die("run: fee funder: %v", err)
	}

	loop := &settlerLoop{
		relay:   strings.TrimRight(*relay, "/"),
		view:    view,
		asm:     &settler.NodeAssembler{Bitcoin: peggedInternal, GapValue: *gapValue, Funder: funder},
		bc:      settler.ChainBroadcaster{Chain: chain},
		http:    &http.Client{Timeout: 15 * time.Second},
		markets: parseMarkets(*markets),
		settled: map[string]time.Time{},
	}

	fmt.Printf("seqob-settler run: relay=%s node=%s wallet=%s gap=%d fee=%d poll=%s\n",
		loop.relay, *nodeRPC, *feeWallet, *gapValue, *feeAtoms, *pollInterval)
	if len(loop.markets) > 0 {
		fmt.Printf("  scoped to %d market(s)\n", len(loop.markets))
	} else {
		fmt.Printf("  auto-discovering markets from %s/v1/markets\n", loop.relay)
	}
	loop.run(*pollInterval)
}

// pair is a base/quote market to poll.
type pair struct{ base, quote string }

// candidate is one discovered both-covenant cross, keyed by its ordered outpoint
// pair for dedup.
type candidate struct {
	key   string
	match matcher.Match
}

type settlerLoop struct {
	relay   string
	view    settler.Inspector
	asm     settler.Assembler
	bc      settler.Broadcaster
	http    *http.Client
	markets []pair
	settled map[string]time.Time
}

// settledTTL is how long a pair stays skipped after a joint fill was broadcast:
// long enough for gettxout to show both covenants spent, short enough that a
// Bitcoin-driven reorg that un-happens the fill (both covenants unspent again,
// re-opened by the relay) is picked up on a later pass rather than never.
const settledTTL = 30 * time.Minute

// run polls forever, escalating a backoff on relay/node I/O errors and resetting
// it after a clean pass.
func (l *settlerLoop) run(interval time.Duration) {
	backoff := interval
	if backoff <= 0 {
		backoff = time.Second
	}
	for {
		n, err := l.tick()
		if err != nil {
			if backoff < 60*time.Second {
				backoff *= 2
			}
			fmt.Printf("settler: pass error: %v (backing off %s)\n", err, backoff)
			time.Sleep(backoff)
			continue
		}
		backoff = interval
		if n > 0 {
			fmt.Printf("settler: settled %d joint cross(es) this pass\n", n)
		}
		time.Sleep(interval)
	}
}

// tick runs one discovery+settle pass over all scoped markets.
func (l *settlerLoop) tick() (int, error) {
	pairs := l.markets
	if len(pairs) == 0 {
		discovered, err := l.discoverMarkets()
		if err != nil {
			return 0, fmt.Errorf("discover markets: %w", err)
		}
		pairs = discovered
	}
	settledCount := 0
	for _, p := range pairs {
		cands, err := l.crossesForPair(p)
		if err != nil {
			// An I/O failure here (relay down) will recur for every pair; surface it
			// so the caller backs off rather than hot-looping.
			return settledCount, fmt.Errorf("pair %s/%s: %w", p.base, p.quote, err)
		}
		for _, c := range cands {
			if at, ok := l.settled[c.key]; ok && time.Since(at) < settledTTL {
				continue
			}
			if l.handle(c) {
				settledCount++
			}
		}
	}
	return settledCount, nil
}

// crossesForPair polls a market's public orderbook and returns every pair of
// opposite-side covenant offers whose integer prices cross (a both-covenant
// candidate). It mirrors the relay matcher's asset line-up + crosses() test.
func (l *settlerLoop) crossesForPair(p pair) ([]candidate, error) {
	book, err := l.orderbook(p)
	if err != nil {
		return nil, err
	}
	var covs []*seqobv1.Offer
	for _, o := range book.GetOffers() {
		if o.GetCovenant() != nil {
			covs = append(covs, o)
		}
	}
	var out []candidate
	seen := map[string]bool{}
	for i := 0; i < len(covs); i++ {
		for j := i + 1; j < len(covs); j++ {
			a, b := covs[i], covs[j]
			// Opposite sides only.
			if (a.GetTradeDir() == seqobv1.TradeDir_TRADE_DIR_BUY) == (b.GetTradeDir() == seqobv1.TradeDir_TRADE_DIR_BUY) {
				continue
			}
			// Assets line up (well-formed swap): a gives what b wants and vice versa.
			if a.GetOfferAsset() != b.GetWantAsset() || a.GetWantAsset() != b.GetOfferAsset() {
				continue
			}
			// Integer price cross (symmetric; mirrors matcher.crosses).
			if !crossesOffers(a, b) {
				continue
			}
			ca, cb := a.GetCovenant(), b.GetCovenant()
			key := outpointPairKey(ca, cb)
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, candidate{
				key:   key,
				match: matcher.Match{RestingCovenant: ca, IncomingCovenant: cb},
			})
		}
	}
	return out, nil
}

// handle resolves both covenants on-chain, computes the cross fills, and settles.
// It returns true iff a joint tx was broadcast. Non-settleable candidates
// (spent/ghost covenants, dust remainders, floors not cleared) are skipped
// quietly; a covenant.PlanJointSettlement / broadcast error is logged.
func (l *settlerLoop) handle(c candidate) bool {
	lockedA, lockedB, err := settler.Resolve(c.match, l.view)
	if err != nil {
		// The advertised cross is stale or unverifiable (a covenant was spent, is a
		// ghost, or does not match its terms). Not fatal — just not settleable now —
		// but say so once per pass, or an operator cannot tell why nothing settles.
		fmt.Printf("settler: %s not resolvable now: %v\n", c.key, err)
		return false
	}
	fillA, fillB, err := computeFills(c.match, lockedA, lockedB)
	if err != nil {
		fmt.Printf("settler: %s: %v\n", c.key, err)
		return false
	}
	txid, err := settler.Handle(c.match, lockedA, fillA, lockedB, fillB, l.asm, l.bc)
	if err != nil {
		fmt.Printf("settler: %s not settled: %v\n", c.key, err)
		return false
	}
	l.settled[c.key] = time.Now()
	fmt.Printf("settler: SETTLED joint cross %s -> tx %s (A %d/%d, B %d/%d)\n",
		c.key, txid, fillA, lockedA, fillB, lockedB)
	return true
}

// computeFills picks the cross fills for two crossing covenants from their locked
// amounts and on-chain rates. Both leaves round in their own maker's favour, so
// with reciprocal rates that do not divide evenly (A at 1/3, B at 3/1, 10 each:
// B owes 4 for all of A's 10, but A then owes ceil(4*3)=12 > 10) the obvious
// pick misses a floor by a rounding atom and the most common cross of all, equal
// prices, was rejected on every pass. The fills are found by a bounded search
// from the largest candidate down, each candidate checked against BOTH leaves'
// ceil floors and the min-lot rule on both remainders. The smaller side fills in
// full when it can; otherwise its remainder re-rests.
// covenant.PlanJointSettlement re-validates every floor and min-lot, so this is
// advisory — a bad pick is rejected there, never broadcast.
func computeFills(m matcher.Match, lockedA, lockedB uint64) (fillA, fillB uint64, err error) {
	numA, denA := m.RestingCovenant.GetRateNum(), m.RestingCovenant.GetRateDen()
	numB, denB := m.IncomingCovenant.GetRateNum(), m.IncomingCovenant.GetRateDen()
	minLotA, minLotB := m.RestingCovenant.GetMinLot(), m.IncomingCovenant.GetMinLot()
	if denA == 0 || denB == 0 {
		return 0, 0, fmt.Errorf("zero rate denominator")
	}
	valid := func(fa, fb uint64) bool {
		if fa == 0 || fb == 0 || fa > lockedA || fb > lockedB || fa < minLotA || fb < minLotB {
			return false
		}
		if fb < covenant.CeilPrice(fa, numA, denA) || fa < covenant.CeilPrice(fb, numB, denB) {
			return false
		}
		if r := lockedA - fa; r != 0 && r < minLotA {
			return false
		}
		if r := lockedB - fb; r != 0 && r < minLotB {
			return false
		}
		return true
	}
	// Candidates, largest first: A's full lock and what B's lock can pay for, then
	// stepping down; for each A candidate, B pays A's floor or its whole lock.
	startA := lockedA
	if c := largestFillCoveredBy(lockedB, numA, denA, lockedA); c < startA {
		startA = c
	}
	for fa, tries := startA, 0; fa > 0 && tries < 256; fa, tries = fa-1, tries+1 {
		for _, fb := range []uint64{covenant.CeilPrice(fa, numA, denA), lockedB} {
			if valid(fa, fb) {
				return fa, fb, nil
			}
		}
	}
	// Symmetric: B's side leads (B fills in full or steps down; A pays B's floor or all).
	startB := lockedB
	if c := largestFillCoveredBy(lockedA, numB, denB, lockedB); c < startB {
		startB = c
	}
	for fb, tries := startB, 0; fb > 0 && tries < 256; fb, tries = fb-1, tries+1 {
		for _, fa := range []uint64{covenant.CeilPrice(fb, numB, denB), lockedA} {
			if valid(fa, fb) {
				return fa, fb, nil
			}
		}
	}
	return 0, 0, fmt.Errorf("no valid cross for locked A=%d B=%d at rates %d/%d and %d/%d (min lots %d/%d)", lockedA, lockedB, numA, denA, numB, denB, minLotA, minLotB)
}

// largestFillCoveredBy is the largest fill f <= cur with CeilPrice(f, num, den) <= budget.
func largestFillCoveredBy(budget, num, den, cur uint64) uint64 {
	if num == 0 {
		return cur
	}
	f := (budget * den) / num // floor(budget*den/num): ceil(f*num/den) <= budget
	for f > 0 && covenant.CeilPrice(f, num, den) > budget {
		f--
	}
	if f > cur {
		f = cur
	}
	return f
}

// --- relay REST -------------------------------------------------------------

func (l *settlerLoop) orderbook(p pair) (*seqobv1.PublicBook, error) {
	url := fmt.Sprintf("%s/v1/market/%s/%s/orderbook", l.relay, p.base, p.quote)
	var book seqobv1.PublicBook
	if err := l.getProto(url, &book); err != nil {
		return nil, err
	}
	return &book, nil
}

func (l *settlerLoop) discoverMarkets() ([]pair, error) {
	var ml seqobv1.MarketList
	if err := l.getProto(l.relay+"/v1/markets", &ml); err != nil {
		return nil, err
	}
	var out []pair
	for _, m := range ml.GetMarkets() {
		out = append(out, pair{base: m.GetPair().GetBaseAsset(), quote: m.GetPair().GetQuoteAsset()})
	}
	return out, nil
}

func (l *settlerLoop) getProto(url string, m proto.Message) error {
	resp, err := l.http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: http %d: %s", url, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return runUnmarshal.Unmarshal(body, m)
}

// --- helpers ----------------------------------------------------------------

// crossesOffers reports whether two offers' integer prices cross. It is the
// symmetric form of the relay matcher's crosses(): offerA*offerB >= wantB*wantA.
func crossesOffers(x, y *seqobv1.Offer) bool {
	lhs := new(big.Int).Mul(u64(x.GetOfferAmount()), u64(y.GetOfferAmount()))
	rhs := new(big.Int).Mul(u64(y.GetWantAmount()), u64(x.GetWantAmount()))
	return lhs.Cmp(rhs) >= 0
}

func u64(x uint64) *big.Int { return new(big.Int).SetUint64(x) }

// outpointPairKey is the order-independent key for a covenant outpoint pair.
func outpointPairKey(a, b *seqobv1.CovenantTerms) string {
	x := fmt.Sprintf("%s:%d", a.GetCovenantTxid(), a.GetCovenantVout())
	y := fmt.Sprintf("%s:%d", b.GetCovenantTxid(), b.GetCovenantVout())
	if x > y {
		x, y = y, x
	}
	return x + "|" + y
}

func parseMarkets(s string) []pair {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	var out []pair
	for _, tok := range strings.Split(s, ",") {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		parts := strings.SplitN(tok, "/", 2)
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			continue
		}
		out = append(out, pair{base: strings.TrimSpace(parts[0]), quote: strings.TrimSpace(parts[1])})
	}
	return out
}

// internalAsset converts a display-hex asset id to its 32-byte internal order.
func internalAsset(displayHex string) ([32]byte, error) {
	var out [32]byte
	commit, err := elementsutil.AssetHashToBytes(displayHex) // 0x01 || internal
	if err != nil {
		return out, err
	}
	if len(commit) != 33 {
		return out, fmt.Errorf("asset %q is not 32 bytes", displayHex)
	}
	copy(out[:], commit[1:])
	return out, nil
}

// env returns the environment variable value or a default.
func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// splitHostPort parses "host:port" into (host, port).
func splitHostPort(hp string) (string, int, error) {
	host, portStr, err := net.SplitHostPort(hp)
	if err != nil {
		return "", 0, err
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return "", 0, err
	}
	return host, port, nil
}
