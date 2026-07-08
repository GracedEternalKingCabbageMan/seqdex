// Command seqobd is the SeqOB order-book relay daemon.
//
// It is NON-CUSTODIAL: it stores signed offers, serves the per-pair book
// (snapshot + deltas), and couriers OPAQUE end-to-end-encrypted swap-session
// messages between two peers. It holds NO wallet, NO keys, and NO funds, and it
// never decrypts the courier payload.
//
// Phase 1 wires: offerstore + validator (no-op liveness probe) + session router
// (no-op reorg watcher) + the REST/WS API.
//
// Chain watcher: when a read-only node RPC is configured (-node-host/-node-port
// with cookie creds, or -node-rpc host:port), seqobd starts the covenant
// chain-watcher (internal/seqob/watcher) as a goroutine holding the same book.
// It reconciles resting COVENANT orders to the node's CURRENT tip — removing
// ghosts whose funding was undone by a Bitcoin-driven reorg (anchoring
// supremacy), re-resting partial-fill remainders, holding unconfirmed spends,
// and re-opening fills that never confirmed. The relay itself stays keyless and
// fundless; the watcher only READS the node and drives the covenant-aware book
// ops.
package main

import (
	"context"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/aejkcs50/seqdex/daemon/internal/seqob/api"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/offerstore"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/session"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/validator"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/watcher"
)

// splitHostPort parses "host:port" for the node RPC endpoint.
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

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	var (
		listen        = flag.String("listen", env("SEQOB_LISTEN", ":9955"), "HTTP listen address (env SEQOB_LISTEN)")
		nodeRPC       = flag.String("node-rpc", env("SEQOB_NODE_RPC", ""), "read-only Sequentia node RPC host:port for the covenant chain-watcher (env SEQOB_NODE_RPC; empty disables the watcher)")
		nodeUser      = flag.String("node-rpc-user", env("SEQOB_NODE_RPC_USER", ""), "node JSON-RPC username / cookie user (env SEQOB_NODE_RPC_USER)")
		nodePass      = flag.String("node-rpc-pass", env("SEQOB_NODE_RPC_PASS", ""), "node JSON-RPC password / cookie pass (env SEQOB_NODE_RPC_PASS)")
		watchInterval = flag.Duration("watch-interval", 5*time.Second, "covenant chain-watcher reconcile interval")
		sessionTTL    = flag.Duration("session-deadline", 2*time.Minute, "lift session co-sign deadline")
		xsessionTTL   = flag.Duration("xsession-deadline", 3*time.Hour, "courier deadline for CROSS-CHAIN lift sessions (they span a real parent-chain confirmation; 0 = use -session-deadline)")
		expirySweep   = flag.Duration("expiry-sweep", 15*time.Second, "offer expiry sweep interval")
		sessionSweep  = flag.Duration("session-sweep", 10*time.Second, "lift-session deadline sweep interval")
		minExpiry     = flag.Duration("min-expiry", 30*time.Second, "minimum offer expiry horizon")
		maxExpiry     = flag.Duration("max-expiry", 7*24*time.Hour, "maximum offer expiry horizon")
		offersPerMin  = flag.Int("offers-per-min", 60, "max offers/min per maker_pubkey")
		offersPerMinI = flag.Int("offers-per-min-ip", 120, "max offers/min per IP")
	)
	flag.Parse()

	logger := log.New(os.Stdout, "seqobd ", log.LstdFlags|log.Lmsgprefix)

	store := offerstore.New(nil)

	vcfg := validator.DefaultConfig()
	vcfg.MinExpiry = *minExpiry
	vcfg.MaxExpiry = *maxExpiry
	vcfg.MaxOffersPerMinPerPubkey = *offersPerMin
	vcfg.MaxOffersPerMinPerIP = *offersPerMinI
	// Phase-1 stub liveness probe (no-op). A later build wires this to nodeRPC.
	v := validator.New(vcfg, validator.NoopLivenessProbe{})

	// onReopen returns an order to the book when a lift aborts/times out or a
	// settled tx's Bitcoin anchor is orphaned (Principle 1). In Phase 1 the reorg
	// watcher is a no-op and aborts leave the (un-removed) order OPEN, so this is a
	// logging seam; it becomes load-bearing once the anchor watch lands.
	onReopen := func(s *session.Session) {
		k := offerstore.Key{MakerPubkey: s.MakerPubkey, OfferID: s.OfferID}
		if _, ok := store.Get(k); ok {
			logger.Printf("session %s ended; order %s/%s still resting", s.ID, s.MakerPubkey, s.OfferID)
			return
		}
		logger.Printf("session %s ended; order %s/%s would need re-open (cache the offer when the anchor watch lands)", s.ID, s.MakerPubkey, s.OfferID)
	}

	router := session.NewRouter(session.Options{
		Deadline: *sessionTTL,
		Reorg:    session.NoopReorgWatcher{}, // Phase-1 stub
		OnReopen: onReopen,
	})

	srv := api.New(store, v, router, logger)
	srv.SetCrossSessionDeadline(*xsessionTTL)

	stop := make(chan struct{})
	go store.RunExpirySweeper(*expirySweep, stop)
	go router.RunDeadlineSweeper(*sessionSweep, stop)

	// Covenant chain-watcher: reconcile the resting covenant book to the node's
	// current tip. Enabled only when a node RPC is configured.
	if *nodeRPC != "" {
		host, port, err := splitHostPort(*nodeRPC)
		if err != nil {
			logger.Fatalf("bad -node-rpc %q: %v", *nodeRPC, err)
		}
		chain := watcher.NewRPCChain(host, port, *nodeUser, *nodePass)
		w := watcher.New(chain, watcher.NewStoreBook(store), logger)
		go w.Run(*watchInterval, stop)
		logger.Printf("covenant chain-watcher enabled (node %s, every %s)", *nodeRPC, *watchInterval)
	} else {
		logger.Printf("covenant chain-watcher DISABLED (no -node-rpc): resting covenant orders are not reconciled to chain state")
	}

	httpSrv := &http.Server{Addr: *listen, Handler: srv.Handler()}

	go func() {
		logger.Printf("listening on %s (non-custodial relay; no wallet, no keys)", *listen)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Fatalf("http server: %v", err)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	logger.Printf("shutting down...")
	close(stop)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(ctx); err != nil {
		logger.Printf("graceful shutdown error: %v", err)
	}
}
