package api

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/offer"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/offerstore"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/session"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/validator"
)

// wsConn wraps a websocket connection with a single-writer lock and its
// per-connection relay state (subscriptions, joined sessions, registered pubkey).
type wsConn struct {
	conn    *websocket.Conn
	writeMu sync.Mutex

	mu          sync.Mutex
	subs        map[string]func() // pairKey -> stop forwarder
	roles       map[string]session.Role
	makerPubkey string

	// closed is closed exactly once when this connection is torn down, so its session
	// pumps and ping loop stop promptly (a lingering pump would steal a re-attached
	// session's courier frames, P3.8).
	closed    chan struct{}
	closeOnce sync.Once
}

// stop signals every goroutine bound to this connection (session pumps, ping loop)
// to exit. Idempotent.
func (c *wsConn) stop() { c.closeOnce.Do(func() { close(c.closed) }) }

// writeWait bounds a single WS write. Without it, a maker whose OS receive buffer is full
// blocks WriteMessage forever while holding writeMu — and because routeMatches/notifyMaker
// call another maker's send() synchronously from the SUBMITTER's read loop, one slow maker
// could wedge a submitter (head-of-line blocking). A deadline turns that into a fast error
// the caller cleans up instead of a silent hang.
const writeWait = 10 * time.Second

// P3.8 keepalive: the relay pings every pingPeriod and expects a pong (or any read)
// within pongWait, else the read deadline fires and the dead connection is reaped.
// Without this a half-open TCP connection (peer vanished, no FIN) would pin the
// maker's offers as unliftable ghosts until their TTL. pingPeriod < pongWait.
const (
	pongWait   = 60 * time.Second
	pingPeriod = 50 * time.Second
)

func (c *wsConn) send(from *seqobv1.From) error {
	b, err := jsonMarshal.Marshal(from)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_ = c.conn.SetWriteDeadline(time.Now().Add(writeWait))
	return c.conn.WriteMessage(websocket.TextMessage, b)
}

func (c *wsConn) sendErr(code uint32, msg string) {
	_ = c.send(&seqobv1.From{Msg: &seqobv1.From_Error{Error: &seqobv1.GenericError{Code: code, Message: msg}}})
}

// connRegistry maps a maker_pubkey to its (single, latest) live WS connection so
// a lift can be routed to an online maker. Best-effort (Phase-1).
type connRegistry struct {
	mu sync.Mutex
	m  map[string]*wsConn
}

func newConnRegistry() *connRegistry { return &connRegistry{m: make(map[string]*wsConn)} }

func (r *connRegistry) set(pubkey string, c *wsConn) {
	r.mu.Lock()
	r.m[pubkey] = c
	r.mu.Unlock()
}

func (r *connRegistry) get(pubkey string) (*wsConn, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.m[pubkey]
	return c, ok
}

// drop unregisters the connection and returns the maker pubkey(s) it was serving, so
// the caller can evict their now-unreachable offers.
func (r *connRegistry) drop(c *wsConn) []string {
	r.mu.Lock()
	dropped := make([]string, 0)
	for k, v := range r.m {
		if v == c {
			delete(r.m, k)
			dropped = append(dropped, k)
		}
	}
	r.mu.Unlock()
	return dropped
}

// handleWS upgrades to a websocket and serves the To/From envelope.
func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	c := &wsConn{
		conn:   conn,
		subs:   make(map[string]func()),
		roles:  make(map[string]session.Role),
		closed: make(chan struct{}),
	}
	defer s.closeConn(c)

	// P3.8 keepalive: bound reads by a deadline the pong handler (and any inbound
	// frame) extends, and ping on a ticker so a half-open peer is reaped instead of
	// pinning its offers forever.
	_ = conn.SetReadDeadline(time.Now().Add(pongWait))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(pongWait))
	})
	go s.pingLoop(c)

	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			// Log the concrete close/read error (previously swallowed). A WS-drop storm is
			// undiagnosable without this — it distinguishes a stale-binary protocol EOF from a
			// write-deadline timeout, a peer close, or a genuine network error.
			if s.log != nil {
				c.mu.Lock()
				mp := c.makerPubkey
				c.mu.Unlock()
				s.log.Printf("ws closed (maker %.16s): %v", mp, err)
			}
			return
		}
		// Any inbound frame proves the peer is alive: push the read deadline out (a
		// chatty client that never pongs stays connected).
		_ = conn.SetReadDeadline(time.Now().Add(pongWait))
		var to seqobv1.To
		if err := jsonUnmarshal.Unmarshal(data, &to); err != nil {
			c.sendErr(400, "decode To: "+err.Error())
			continue
		}
		s.dispatch(c, &to, clientIP(r))
	}
}

// pingLoop pings the peer every pingPeriod until the connection is torn down, so a
// half-open TCP connection (peer gone with no FIN) trips the read deadline and is
// reaped. WriteControl is safe to call concurrently with the single-writer send().
func (s *Server) pingLoop(c *wsConn) {
	t := time.NewTicker(pingPeriod)
	defer t.Stop()
	for {
		select {
		case <-c.closed:
			return
		case <-t.C:
			if err := c.conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(writeWait)); err != nil {
				return
			}
		}
	}
}

func (s *Server) closeConn(c *wsConn) {
	c.stop() // stop session pumps + ping loop first so they don't steal re-attach frames
	c.mu.Lock()
	for _, stop := range c.subs {
		stop()
	}
	c.subs = map[string]func(){}
	c.mu.Unlock()
	// Evict this maker's resting offers: in an interactive book the maker must be online
	// to co-sign a lift, so once its connection drops its offers are unliftable and would
	// otherwise linger as un-fillable "ghosts" until their TTL.
	for _, pubkey := range s.makerConns.drop(c) {
		s.store.RemoveByMaker(pubkey)
	}
	s.releaseAbandonedLifts(c)
	c.conn.Close()
}

// takerReattachGrace is how long an abandoned lift session is held open for the taker
// to come back (P3.8 re-attach) before it is aborted and the offer's lift slot freed.
var takerReattachGrace = 45 * time.Second

// releaseAbandonedLifts aborts the lift sessions whose TAKER just disconnected and did
// not come back.
//
// An offer has ONE lift slot, held for as long as its session is live, and a cross
// session's co-sign deadline is measured in HOURS because it spans real parent-chain
// confirmations. Nothing released that slot when the taker simply went away — a failed
// take, a closed tab, a page reload — so one abandoned attempt made the offer answer
// every subsequent taker with "offer has a lift in progress; retry when it frees" for
// the rest of that deadline. In a book being probed by real users that poisons offers
// faster than the makers re-quote them, and the book looks full while being unusable.
//
// Router.Abort already exists for exactly this ("maker refused / taker bailed") and
// re-opens the order; it was simply never called on a disconnect.
//
// The grace period is what keeps this compatible with re-attach: a taker mid-swap with
// funds committed MUST be able to reconnect, and attach() overwrites the recorded
// connection when it does, so a returning taker is never aborted out from under.
func (s *Server) releaseAbandonedLifts(c *wsConn) {
	c.mu.Lock()
	ids := make([]string, 0, len(c.roles))
	for sid, role := range c.roles {
		if role == session.RoleTaker {
			ids = append(ids, sid)
		}
	}
	c.mu.Unlock()
	for _, sid := range ids {
		go func(sid string) {
			time.Sleep(takerReattachGrace)
			s.takerMu.Lock()
			cur, ok := s.takerConns[sid]
			if ok && cur == c {
				delete(s.takerConns, sid)
			}
			s.takerMu.Unlock()
			if !ok || cur != c {
				return // the taker re-attached on a new connection; leave it alone
			}
			if _, live := s.sessions.Get(sid); !live {
				return
			}
			s.sessions.Abort(sid)
		}(sid)
	}
}

func (s *Server) dispatch(c *wsConn, to *seqobv1.To, ip string) {
	switch m := to.GetMsg().(type) {
	case *seqobv1.To_MarketSubscribe:
		s.wsSubscribe(c, m.MarketSubscribe)
	case *seqobv1.To_MarketUnsubscribe:
		s.wsUnsubscribe(c, m.MarketUnsubscribe)
	case *seqobv1.To_OfferSubmit:
		s.wsOfferSubmit(c, m.OfferSubmit, ip)
	case *seqobv1.To_OfferEdit:
		s.wsOfferEdit(c, m.OfferEdit, ip)
	case *seqobv1.To_OfferCancel:
		s.wsOfferCancel(c, m.OfferCancel)
	case *seqobv1.To_StartLift:
		s.wsStartLift(c, m.StartLift)
	case *seqobv1.To_SessionReattach:
		s.wsSessionReattach(c, m.SessionReattach)
	case *seqobv1.To_SettleAck:
		s.wsSettleAck(c, m.SettleAck)
	case *seqobv1.To_SettledTrade:
		s.wsSettledTrade(c, m.SettledTrade)
	case *seqobv1.To_SwapMsg:
		s.wsSwapMsg(c, m.SwapMsg)
	case *seqobv1.To_ListMarkets:
		_ = c.send(&seqobv1.From{Msg: &seqobv1.From_MarketList{MarketList: &seqobv1.MarketList{Markets: s.store.Markets()}}})
	default:
		c.sendErr(400, "unknown or empty To.msg")
	}
}

// subKey is the per-connection subscription key. It includes the book NAMESPACE
// (transparent vs confidential) so a client may hold BOTH a transparent and a
// confidential subscription to the same pair without one replacing the other.
func subKey(pair *seqobv1.AssetPair) string {
	k := pair.GetBaseAsset() + "/" + pair.GetQuoteAsset()
	if pair.GetConfidential() {
		k += "|conf"
	}
	return k
}

func (s *Server) wsSubscribe(c *wsConn, pair *seqobv1.AssetPair) {
	if pair == nil {
		c.sendErr(400, "market_subscribe missing pair")
		return
	}
	// The subscribe request carries the book namespace (AssetPair.confidential): false
	// (default) is the transparent book, true the separate blinded book. The snapshot and
	// every delta are filtered to it, matching REST (?confidential=) and the matcher, so a
	// transparent-book UI never renders a confidential offer and vice-versa.
	confidential := pair.GetConfidential()
	pk := subKey(pair)
	snap, id, ch := s.store.Subscribe(pair, confidential)
	stop := make(chan struct{})

	c.mu.Lock()
	if prev, ok := c.subs[pk]; ok {
		prev()
	}
	c.subs[pk] = func() {
		close(stop)
		s.store.Unsubscribe(id)
	}
	c.mu.Unlock()

	// Snapshot first, then stream deltas.
	_ = c.send(&seqobv1.From{Msg: &seqobv1.From_PublicBook{PublicBook: &seqobv1.PublicBook{Pair: pair, Offers: snap}}})
	go s.forwardDeltas(c, ch, stop)
}

func (s *Server) forwardDeltas(c *wsConn, ch <-chan offerstore.Event, stop <-chan struct{}) {
	var lastSeq uint64
	for {
		select {
		case <-stop:
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			var from *seqobv1.From
			switch ev.Type {
			case offerstore.EventCreated:
				from = &seqobv1.From{Msg: &seqobv1.From_PublicOrderCreated{PublicOrderCreated: ev.Offer}}
			case offerstore.EventStatus:
				from = &seqobv1.From{Msg: &seqobv1.From_OrderStatus{OrderStatus: ev.Status}}
			case offerstore.EventRemoved:
				from = &seqobv1.From{Msg: &seqobv1.From_PublicOrderRemoved{PublicOrderRemoved: ev.Ref}}
			default:
				continue
			}
			// P3.8 delta sequencing: the store stamps a monotonic per-subscription Seq and
			// increments it for every routed event (delivered OR dropped), so a hole here means
			// the relay dropped a delta on a buffer overflow and this subscriber's view is
			// stale. Tell it to re-snapshot (Resync) BEFORE the out-of-order delta, then carry
			// the seq on the delta so the client can also self-detect.
			if lastSeq != 0 && ev.Seq != lastSeq+1 {
				_ = c.send(&seqobv1.From{Msg: &seqobv1.From_Resync{Resync: &seqobv1.Resync{Pair: ev.Pair, NextSeq: ev.Seq}}})
			}
			lastSeq = ev.Seq
			from.Seq = ev.Seq
			if err := c.send(from); err != nil {
				return
			}
		}
	}
}

func (s *Server) wsUnsubscribe(c *wsConn, pair *seqobv1.AssetPair) {
	if pair == nil {
		return
	}
	pk := subKey(pair) // namespace-scoped, matching wsSubscribe
	c.mu.Lock()
	if stop, ok := c.subs[pk]; ok {
		stop()
		delete(c.subs, pk)
	}
	c.mu.Unlock()
}

func (s *Server) wsOfferSubmit(c *wsConn, o *seqobv1.Offer, ip string) {
	if err := s.validator.ValidateOffer(context.Background(), o, ip); err != nil {
		if errors.Is(err, validator.ErrReplay) {
			// ITEM A: byte-identical replay of an already-resting offer. It cost the
			// maker no rate budget; do not re-submit (the store would reject the
			// duplicate key). Re-register reachability and re-ack the live status.
			s.registerMaker(c, o.GetMakerPubkey())
			_ = c.send(&seqobv1.From{Msg: &seqobv1.From_OrderStatus{OrderStatus: s.restingStatus(o)}})
			return
		}
		c.sendErr(400, "invalid offer: "+err.Error())
		return
	}
	if _, err := s.store.Submit(o); err != nil {
		// A DURABLE offer (cross-chain / lightning / covenant) survives its maker's WS drop, so a
		// reconnecting settlement agent re-posts a REFRESHED (re-signed, new expiry) copy against the
		// still-resting key. Treat that as an EDIT + re-register (so lifts route to the reconnected
		// agent) rather than a 409 — this is what keeps the durable book populated across WS churn.
		if errors.Is(err, offerstore.ErrExists) {
			if eerr := s.store.Edit(o); eerr != nil {
				c.sendErr(409, "submit(refresh): "+eerr.Error())
				return
			}
			s.registerMaker(c, o.GetMakerPubkey())
			_ = c.send(&seqobv1.From{Msg: &seqobv1.From_OrderStatus{OrderStatus: s.restingStatus(o)}})
			return
		}
		c.sendErr(409, "submit: "+err.Error())
		return
	}
	// Register this connection as the maker's reachable endpoint for live lifts.
	s.registerMaker(c, o.GetMakerPubkey())

	// Continuous matching under the order's time-in-force: cross the freshly-rested
	// order against the resting opposite side, routing From.matched to the
	// counterparties. For a covenant resting order the offline maker needs no
	// round-trip; the taker (this connection) is told the covenant terms and settles
	// the FILL spend itself. The disposition retires the incoming per IOC/FOK/POST_ONLY.
	res := s.matcher.CrossOrder(o)
	s.routeMatches(c, res.Matches)
	if s.applyDisposition(o, res) {
		_ = c.send(&seqobv1.From{Msg: &seqobv1.From_OrderStatus{OrderStatus: cancelledStatus(o)}})
		return
	}

	_ = c.send(&seqobv1.From{Msg: &seqobv1.From_OrderStatus{OrderStatus: s.restingStatus(o)}})
}

func (s *Server) wsOfferEdit(c *wsConn, o *seqobv1.Offer, ip string) {
	if err := s.validator.ValidateOffer(context.Background(), o, ip); err != nil {
		if errors.Is(err, validator.ErrReplay) {
			// No-op edit identical to the resting offer (ITEM A): no budget charged,
			// nothing to change; re-ack the live status.
			_ = c.send(&seqobv1.From{Msg: &seqobv1.From_OrderStatus{OrderStatus: s.restingStatus(o)}})
			return
		}
		c.sendErr(400, "invalid offer: "+err.Error())
		return
	}
	if err := s.store.Edit(o); err != nil {
		c.sendErr(409, "edit: "+err.Error())
		return
	}
	_ = c.send(&seqobv1.From{Msg: &seqobv1.From_OrderStatus{OrderStatus: &seqobv1.OrderStatus{
		OfferId: o.GetOfferId(), MakerPubkey: o.GetMakerPubkey(),
		Status: seqobv1.OfferStatus_OFFER_STATUS_OPEN, ActiveAmount: o.GetBaseAmount(),
	}}})
}

// registerMaker binds this connection as the reachable endpoint for makerPubkey
// so a lift can be routed to it.
func (s *Server) registerMaker(c *wsConn, makerPubkey string) {
	c.mu.Lock()
	c.makerPubkey = makerPubkey
	c.mu.Unlock()
	s.makerConns.set(makerPubkey, c)
}

// restingStatus returns the live OrderStatus for o's key, falling back to a fresh
// OPEN status if the offer raced out of the book between validate and lookup.
func (s *Server) restingStatus(o *seqobv1.Offer) *seqobv1.OrderStatus {
	if st, ok := s.store.OrderStatusOf(offerstore.Key{MakerPubkey: o.GetMakerPubkey(), OfferID: o.GetOfferId()}); ok {
		return st
	}
	return &seqobv1.OrderStatus{
		OfferId: o.GetOfferId(), MakerPubkey: o.GetMakerPubkey(),
		Status: seqobv1.OfferStatus_OFFER_STATUS_OPEN, ActiveAmount: o.GetBaseAmount(),
	}
}

func (s *Server) wsOfferCancel(c *wsConn, cancel *seqobv1.OfferCancel) {
	if err := s.validator.ValidateCancel(cancel); err != nil {
		c.sendErr(400, "invalid cancel: "+err.Error())
		return
	}
	if err := s.store.Cancel(cancel); err != nil {
		c.sendErr(409, "cancel: "+err.Error())
		return
	}
	_ = c.send(&seqobv1.From{Msg: &seqobv1.From_OrderStatus{OrderStatus: &seqobv1.OrderStatus{
		OfferId: cancel.GetOfferId(), MakerPubkey: cancel.GetMakerPubkey(),
		Status: seqobv1.OfferStatus_OFFER_STATUS_CANCELLED,
	}}})
}

// liftGuardExempts reports whether an offer may be lifted with NO maker connection
// present. Only a covenant offer qualifies: the taker settles the fill spend itself, so
// there is nobody to wait for. Every other settlement needs the maker live to send
// terms. A nil offer is exempt so the guard never pre-empts openLift's own reporting of
// an unknown/closed offer.
func liftGuardExempts(o *seqobv1.Offer) bool {
	return o == nil || o.GetCovenant() != nil
}

func (s *Server) wsStartLift(c *wsConn, sl *seqobv1.StartLift) {
	// openLift -> Router.StartLift fires the notifyMaker hook, which binds the
	// maker connection and delivers From.lift_requested (with the taker's session
	// pubkey) BEFORE we reply to the taker. Bind the taker here so it can receive
	// the maker's couriered reply.
	// REFUSE A LIFT AGAINST A MAKER THAT IS NOT HERE.
	//
	// A cross-chain or Lightning offer survives its maker's WS drop on purpose: the
	// settlement agent reconnects after a blip, and evicting on every drop emptied the
	// cross book (see TestCrossSurvivesMakerDisconnect). But surviving a reconnect is not
	// the same as being liftable while the maker is GONE. Those rails need the maker live
	// to send terms, so a lift against a departed maker used to be ACCEPTED and then
	// simply go quiet — the taker waited out its terms timeout, and the trade sat there
	// while blocks went by, with nothing to retry against because no error was ever sent.
	//
	// A covenant offer is genuinely offline-liftable (the taker settles the fill spend
	// itself), so it is exempt. For everything else, say so immediately: an instant,
	// explicit refusal is retryable down the book, where silence is not.
	if o, ok := s.store.Get(offerstore.Key{MakerPubkey: sl.GetMakerPubkey(), OfferID: sl.GetOfferId()}); ok && !liftGuardExempts(o.Offer) {
		if _, live := s.makerConns.get(sl.GetMakerPubkey()); !live {
			c.sendErr(409, "maker is not connected; this offer cannot be lifted right now")
			return
		}
	}
	sess, err := s.openLift(sl)
	if err != nil {
		c.sendErr(409, err.Error())
		return
	}
	s.attach(c, sess, session.RoleTaker)
	_ = c.send(&seqobv1.From{Msg: &seqobv1.From_LiftAccepted{LiftAccepted: &seqobv1.LiftAccepted{
		SessionId: sess.ID, MakerSessionPubkey: makerPubkeyBytes(sl.GetMakerPubkey()),
	}}})
}

// wsSessionReattach re-binds a RECONNECTED websocket to a still-live lift session
// after a WS drop (P3.8). The role was bound to the OLD socket, so without this the
// fresh WS 403s on the first swap_msg. The participant re-proves control of the
// session key the relay already holds for the claimed role (the maker's offer key,
// or the taker's ephemeral session pubkey), so no new trust is introduced.
func (s *Server) wsSessionReattach(c *wsConn, ra *seqobv1.SessionReattach) {
	if ra == nil || ra.GetSessionId() == "" {
		c.sendErr(400, "session_reattach missing session_id")
		return
	}
	sid, role := ra.GetSessionId(), ra.GetRole()
	sess, ok := s.sessions.Get(sid)
	if !ok {
		c.sendErr(409, "unknown or closed session")
		return
	}
	var sessRole session.Role
	var keyBytes []byte
	switch role {
	case "maker":
		sessRole = session.RoleMaker
		keyBytes = makerPubkeyBytes(sess.MakerPubkey) // the maker's offer key doubles as its session key
	case "taker":
		sessRole = session.RoleTaker
		keyBytes = sess.TakerSessionPubkey
	default:
		c.sendErr(400, "session_reattach role must be \"taker\" or \"maker\"")
		return
	}
	if len(keyBytes) == 0 {
		c.sendErr(409, "no bound session key for role "+role)
		return
	}
	if err := offer.VerifyReattach(sid, role, keyBytes, ra.GetSig()); err != nil {
		c.sendErr(403, "reattach auth: "+err.Error())
		return
	}
	// Re-bind: attach the role so this connection again receives couriered frames, and
	// (for a maker) re-register it as the maker's reachable endpoint for further lifts.
	s.attach(c, sess, sessRole)
	if sessRole == session.RoleMaker {
		s.registerMaker(c, sess.MakerPubkey)
	}
	_ = c.send(&seqobv1.From{Msg: &seqobv1.From_SessionReattached{SessionReattached: &seqobv1.SessionReattached{
		SessionId: sid, Role: role,
	}}})
}

// notifyMaker is the session.Router hook: when a taker lifts an offer, deliver a
// From.lift_requested to the maker's live connection (if any) carrying the
// taker's session pubkey, and bind the maker so it receives couriered frames.
// The relay never decrypts; it only routes.
func (s *Server) notifyMaker(sess *session.Session) {
	mc, ok := s.makerConns.get(sess.MakerPubkey)
	if !ok {
		return
	}
	s.attach(mc, sess, session.RoleMaker)
	_ = mc.send(&seqobv1.From{Msg: &seqobv1.From_LiftRequested{LiftRequested: &seqobv1.LiftRequested{
		SessionId:          sess.ID,
		OfferId:            sess.OfferID,
		MakerPubkey:        sess.MakerPubkey,
		TakeAmount:         sess.TakeAmount,
		TakerSessionPubkey: sess.TakerSessionPubkey,
	}}})
}

// attach records the connection's role in a session and starts a pump that
// forwards inbound courier frames for that role to the socket.
func (s *Server) attach(c *wsConn, sess *session.Session, role session.Role) {
	c.mu.Lock()
	c.roles[sess.ID] = role
	c.mu.Unlock()
	if role == session.RoleTaker {
		// Remember who holds the taker side, so closeConn can tell an ABANDONED lift
		// from a taker that merely reconnected (P3.8 re-attach overwrites this).
		s.takerMu.Lock()
		s.takerConns[sess.ID] = c
		s.takerMu.Unlock()
	}
	go func() {
		inbox := sess.Inbox(role)
		for {
			select {
			case <-sess.Done():
				return
			case <-c.closed:
				// This connection dropped: stop pumping so a reconnected+re-attached
				// connection (P3.8) receives the session's frames instead of this dead pump
				// silently consuming them.
				return
			case msg, ok := <-inbox:
				if !ok {
					return
				}
				if err := c.send(&seqobv1.From{Msg: &seqobv1.From_SwapMsg{SwapMsg: msg}}); err != nil {
					return
				}
			}
		}
	}()
}

func (s *Server) wsSwapMsg(c *wsConn, msg *seqobv1.SwapMsg) {
	if msg == nil || msg.GetSessionId() == "" {
		c.sendErr(400, "swap_msg missing session_id")
		return
	}
	c.mu.Lock()
	role, ok := c.roles[msg.GetSessionId()]
	c.mu.Unlock()
	if !ok {
		c.sendErr(403, "not a participant in this session")
		return
	}
	// OPAQUE courier: route ciphertext only, never decrypt or parse it.
	if err := s.sessions.Send(msg.GetSessionId(), role, msg.GetCiphertext()); err != nil {
		c.sendErr(409, "courier: "+err.Error())
	}
}

// wsSettleAck is the interactive-lift settlement commit point. A same-chain lift settles
// via an opaque SwapMsg courier the relay never parses, so it otherwise never learns the
// lift settled — leaving the resting order a ghost at full size and the trades feed empty
// by construction. The MAKER, having co-signed the exact settling PSET, sends this ack
// after broadcast; on it the relay records the trade at the resting order's price,
// decrements/finalizes the resting order, arms reorg re-open with the settling txid, and
// closes the session. MAKER-authenticated: only the session's maker may send it, so a
// taker cannot grief a live resting order by forging a decrement. The decrement size is
// the relay's own authoritative TakeAmount (from StartLift), not the maker-reported
// fill_base, clamped to what is actually resting.
func (s *Server) wsSettleAck(c *wsConn, sa *seqobv1.SettleAck) {
	if sa == nil || sa.GetSessionId() == "" {
		c.sendErr(400, "settle_ack missing session_id")
		return
	}
	sid := sa.GetSessionId()
	c.mu.Lock()
	role, ok := c.roles[sid]
	c.mu.Unlock()
	if !ok || role != session.RoleMaker {
		c.sendErr(403, "only the session maker may ack settlement")
		return
	}
	sess, ok := s.sessions.Get(sid)
	if !ok {
		c.sendErr(409, "unknown or closed session")
		return
	}
	k := offerstore.Key{MakerPubkey: sess.MakerPubkey, OfferID: sess.OfferID}
	// Record + decrement only if the resting order is still present. If a prior fill already
	// removed it, there is nothing to record/decrement — but still close the session so it does
	// not linger and get deadline-aborted (which would try to re-open an already-settled order).
	if e, ok := s.store.Get(k); ok {
		fill := sess.TakeAmount
		if fill > e.ActiveAmount {
			fill = e.ActiveAmount // never decrement below the resting size (it shrank via another fill)
		}
		if fill > 0 {
			// Record BEFORE ApplyPartialFill: the latter may remove the entry at full fill, after
			// which e.Offer (needed for the trade's pair + execution price) is no longer fetchable.
			// Record WITH the settle txid so a reorg can retract this exact print (P3.9).
			s.store.RecordTradeTx(e.Offer, fill, sa.GetSettleTxid())
			// anchor_confs the maker declares for the settling tx (0 keeps the 0-conf-tolerant
			// default). A fill with anchor_confs >= min_anchor_depth reaches FILLED; previously
			// this was hard-coded 0, so a min_anchor_depth>0 offer could never finalize.
			if err := s.store.ApplyPartialFill(k, fill, sa.GetSettleTxid(), sa.GetAnchorConfs()); err != nil {
				c.sendErr(409, "apply fill: "+err.Error())
				return
			}
			// Arm reorg re-open with the settled offer + fill: if this tx's Bitcoin anchor is
			// later orphaned, onReopen re-inserts the exact order (restoring `fill` atoms) and
			// retracts the phantom trade (Principle 1 / P3.9). Cache the offer here because a full
			// fill REMOVED it from the store.
			_ = s.sessions.ArmReorgReopen(sid, sa.GetSettleTxid(), e.Offer, fill)
			s.sessions.Close(sid)
			return
		}
	}
	_ = s.sessions.SetSettleTxid(sid, sa.GetSettleTxid()) // arms reorg re-open (Principle 1)
	s.sessions.Close(sid)
}

// wsSettledTrade is the durable-settlement commit point for CROSS-CHAIN and LIGHTNING
// lifts. Unlike a same-chain lift (fast, its co-sign session still live at settlement),
// a cross/LN swap can finish long after the courier session was swept (its on-chain /
// Lightning leg outlives the co-sign deadline) or after the maker reconnected (dropping
// the in-memory session role) — so a session-scoped SettleAck is unreachable. Instead the
// maker sends this MAKER-SIGNED, offer-keyed record: the relay verifies the signature
// against the resting offer's maker_pubkey, records the trade at the resting order's price,
// and decrements/finalizes the order (advancing anchor_confs). Without it a BTC/LN-only pair
// reports last_price 0 with an empty chart — every cross/LN fill went unrecorded. It leaks
// nothing the chain does not (settle_txid is public), and the maker has no incentive to
// falsely decrement its own resting order. Reorg retraction is a separate concern.
func (s *Server) wsSettledTrade(c *wsConn, st *seqobv1.SettledTrade) {
	if st == nil || st.GetOfferId() == "" || st.GetMakerPubkey() == "" {
		c.sendErr(400, "settled_trade missing offer_id/maker_pubkey")
		return
	}
	// Maker-signed and self-standing: the signature over {offer_id, maker_pubkey, fill_base,
	// settle_txid, anchor_confs, nonce} authenticates the report without a live session, and
	// keying the store op to (maker_pubkey, offer_id) means only the offer's own maker can
	// record/decrement its fill (a forged report for another maker's offer needs that maker's key).
	if err := offer.VerifySettledTrade(st); err != nil {
		c.sendErr(400, "invalid settled_trade: "+err.Error())
		return
	}
	k := offerstore.Key{MakerPubkey: st.GetMakerPubkey(), OfferID: st.GetOfferId()}
	if err := s.store.RecordSettledFill(k, st.GetFillBase(), st.GetSettleTxid(), st.GetAnchorConfs(), st.GetNonce()); err != nil {
		c.sendErr(409, "record settled trade: "+err.Error())
		return
	}
}
