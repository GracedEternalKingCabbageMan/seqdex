package api

import (
	"context"
	"errors"
	"net/http"
	"sync"

	"github.com/gorilla/websocket"

	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
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
}

func (c *wsConn) send(from *seqobv1.From) error {
	b, err := jsonMarshal.Marshal(from)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
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
		conn:  conn,
		subs:  make(map[string]func()),
		roles: make(map[string]session.Role),
	}
	defer s.closeConn(c)

	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		var to seqobv1.To
		if err := jsonUnmarshal.Unmarshal(data, &to); err != nil {
			c.sendErr(400, "decode To: "+err.Error())
			continue
		}
		s.dispatch(c, &to, clientIP(r))
	}
}

func (s *Server) closeConn(c *wsConn) {
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
	c.conn.Close()
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
	case *seqobv1.To_SwapMsg:
		s.wsSwapMsg(c, m.SwapMsg)
	case *seqobv1.To_ListMarkets:
		_ = c.send(&seqobv1.From{Msg: &seqobv1.From_MarketList{MarketList: &seqobv1.MarketList{Markets: s.store.Markets()}}})
	default:
		c.sendErr(400, "unknown or empty To.msg")
	}
}

func (s *Server) wsSubscribe(c *wsConn, pair *seqobv1.AssetPair) {
	if pair == nil {
		c.sendErr(400, "market_subscribe missing pair")
		return
	}
	pk := pair.GetBaseAsset() + "/" + pair.GetQuoteAsset()
	snap, id, ch := s.store.Subscribe(pair)
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
			case offerstore.EventRemoved:
				from = &seqobv1.From{Msg: &seqobv1.From_PublicOrderRemoved{PublicOrderRemoved: ev.Ref}}
			case offerstore.EventStatus:
				from = &seqobv1.From{Msg: &seqobv1.From_OrderStatus{OrderStatus: ev.Status}}
			default:
				continue
			}
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
	pk := pair.GetBaseAsset() + "/" + pair.GetQuoteAsset()
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
	k, err := s.store.Submit(o)
	if err != nil {
		c.sendErr(409, "submit: "+err.Error())
		return
	}
	// Register this connection as the maker's reachable endpoint for live lifts.
	s.registerMaker(c, o.GetMakerPubkey())

	_ = c.send(&seqobv1.From{Msg: &seqobv1.From_OrderStatus{OrderStatus: &seqobv1.OrderStatus{
		OfferId: k.OfferID, MakerPubkey: k.MakerPubkey,
		Status: seqobv1.OfferStatus_OFFER_STATUS_OPEN, ActiveAmount: o.GetBaseAmount(),
	}}})
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

func (s *Server) wsStartLift(c *wsConn, sl *seqobv1.StartLift) {
	// openLift -> Router.StartLift fires the notifyMaker hook, which binds the
	// maker connection and delivers From.lift_requested (with the taker's session
	// pubkey) BEFORE we reply to the taker. Bind the taker here so it can receive
	// the maker's couriered reply.
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
	go func() {
		inbox := sess.Inbox(role)
		for {
			select {
			case <-sess.Done():
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
