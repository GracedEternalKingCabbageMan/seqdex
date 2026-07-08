package api

import (
	"crypto/rand"
	"encoding/hex"

	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/matcher"
)

// matchedProto builds the From.matched payload for one recipient role.
func matchedProto(mt matcher.Match, role, sessionID string) *seqobv1.Matched {
	m := &seqobv1.Matched{
		SessionId:       sessionID,
		OfferId:         mt.RestingKey.OfferID,
		MakerPubkey:     mt.RestingKey.MakerPubkey,
		Role:            role,
		FillBaseAmount:  mt.FillBase,
		FillQuoteAmount: mt.FillQuote,
	}
	if mt.RestingCovenant != nil {
		m.RestingIsCovenant = true
		m.Covenant = mt.RestingCovenant
		m.CovenantLocked = mt.RestingLocked
	}
	return m
}

// routeMatches delivers From.matched to the counterparties of each match. The
// incoming submitter (taker connection `tc`, may be nil for the REST path) is
// told about every fill it triggered. For an INTERACTIVE resting order with an
// online maker, the maker is also notified so it can co-sign; a COVENANT resting
// order needs no maker round-trip (the taker settles the FILL spend itself), so
// its offline maker is not contacted.
func (s *Server) routeMatches(tc *wsConn, matches []matcher.Match) {
	for _, mt := range matches {
		sid := newSessionID()
		if tc != nil {
			_ = tc.send(&seqobv1.From{Msg: &seqobv1.From_Matched{Matched: matchedProto(mt, "taker", sid)}})
		}
		if mt.RestingCovenant == nil {
			if mc, ok := s.makerConns.get(mt.RestingKey.MakerPubkey); ok && mc != tc {
				_ = mc.send(&seqobv1.From{Msg: &seqobv1.From_Matched{Matched: matchedProto(mt, "maker", sid)}})
			}
		}
	}
}

func newSessionID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
