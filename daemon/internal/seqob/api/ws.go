package api

import (
	"context"
	"net/http"
	"sync"

	"github.com/gorilla/websocket"

	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/offerstore"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/session"
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

func (r *connRegistry) drop(c *wsConn) {
	r.mu.Lock()
	for k, v := range r.m {
		if v == c {
			delete(r.m, k)
		}
	}
	r.mu.Unlock()
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
	s.makerConns.drop(c)
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
		c.sendErr(400, "invalid offer: "+err.Error())
		return
	}
	k, err := s.store.Submit(o)
	if err != nil {
		c.sendErr(409, "submit: "+err.Error())
		return
	}
	// Register this connection as the maker's reachable endpoint for live lifts.
	c.mu.Lock()
	c.makerPubkey = o.GetMakerPubkey()
	c.mu.Unlock()
	s.makerConns.set(o.GetMakerPubkey(), c)

	_ = c.send(&seqobv1.From{Msg: &seqobv1.From_OrderStatus{OrderStatus: &seqobv1.OrderStatus{
		OfferId: k.OfferID, MakerPubkey: k.MakerPubkey,
		Status: seqobv1.OfferStatus_OFFER_STATUS_OPEN, ActiveAmount: o.GetBaseAmount(),
	}}})
}

func (s *Server) wsOfferEdit(c *wsConn, o *seqobv1.Offer, ip string) {
	if err := s.validator.ValidateOffer(context.Background(), o, ip); err != nil {
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
	sess, err := s.openLift(sl)
	if err != nil {
		c.sendErr(409, err.Error())
		return
	}
	// Bind the taker (this connection) and pump frames destined for it.
	s.attach(c, sess, session.RoleTaker)
	_ = c.send(&seqobv1.From{Msg: &seqobv1.From_LiftAccepted{LiftAccepted: &seqobv1.LiftAccepted{
		SessionId: sess.ID, MakerSessionPubkey: sess.MakerSessionPubkey,
	}}})

	// If the maker is online, bind it too and notify it of the new session.
	// PHASE-1 LIMITATION: the From envelope has no dedicated "lift requested"
	// variant carrying taker_session_pubkey, so a fully live maker handshake needs
	// that added field; here we notify the maker via lift_accepted(session_id) and
	// courier the (opaque) frames once both peers are attached.
	if mc, ok := s.makerConns.get(sl.GetMakerPubkey()); ok && mc != c {
		s.attach(mc, sess, session.RoleMaker)
		_ = mc.send(&seqobv1.From{Msg: &seqobv1.From_LiftAccepted{LiftAccepted: &seqobv1.LiftAccepted{SessionId: sess.ID}}})
	}
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
