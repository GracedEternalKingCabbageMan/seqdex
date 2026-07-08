// Command seqob-relaycli is a thin scripting client for the SeqOB relay, used by
// the regtest matcher+covenant proof (feature_seqob_matcher_covenant.py). It
// signs an Offer with the production offer package and drives the relay two ways:
//
//	post  --http URL  --priv HEX  [--offer-file F | stdin]
//	    Sign the offer and POST it to /v1/offers (the MAKER path: stateless, so
//	    the maker is inherently OFFLINE afterwards). Prints the OrderStatus JSON.
//
//	take  --ws URL    --priv HEX  [--offer-file F | stdin]  [--timeout D]
//	    Sign the offer, open the WS, subscribe to its pair, submit it, and wait
//	    for the matcher's From.matched. Prints the matched JSON, or exits non-zero
//	    on timeout. This is the TAKER path that proves auto-crossing.
//
// The offer is protojson of seqob.v1.Offer WITHOUT maker_pubkey/maker_sig; this
// tool fills both from --priv so the canonical signed bytes are the production
// ones. Everything it emits is protojson (proto field names).
package main

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/gorilla/websocket"
	"google.golang.org/protobuf/encoding/protojson"

	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/offer"
)

func die(f string, a ...interface{}) {
	fmt.Fprintf(os.Stderr, "seqob-relaycli: "+f+"\n", a...)
	os.Exit(1)
}

var jsonU = protojson.UnmarshalOptions{DiscardUnknown: true}
var jsonM = protojson.MarshalOptions{UseProtoNames: true, EmitUnpopulated: true}

func loadOffer(path string) *seqobv1.Offer {
	var raw []byte
	var err error
	if path == "" || path == "-" {
		raw, err = io.ReadAll(os.Stdin)
	} else {
		raw, err = os.ReadFile(path)
	}
	if err != nil {
		die("read offer: %v", err)
	}
	var o seqobv1.Offer
	if err := jsonU.Unmarshal(raw, &o); err != nil {
		die("parse offer json: %v", err)
	}
	return &o
}

func priv(hexKey string) *btcec.PrivateKey {
	b, err := hex.DecodeString(hexKey)
	if err != nil || len(b) != 32 {
		die("--priv must be 32-byte hex")
	}
	k, _ := btcec.PrivKeyFromBytes(b)
	return k
}

func signOffer(o *seqobv1.Offer, k *btcec.PrivateKey) {
	o.MakerPubkey = ""
	o.MakerSig = nil
	if err := offer.SignOffer(o, k); err != nil {
		die("sign offer: %v", err)
	}
}

func main() {
	if len(os.Args) < 2 {
		die("usage: seqob-relaycli <post|take> [flags]")
	}
	switch os.Args[1] {
	case "post":
		fs := flag.NewFlagSet("post", flag.ExitOnError)
		httpURL := fs.String("http", "http://127.0.0.1:9955", "relay HTTP base URL")
		p := fs.String("priv", "", "maker private key (32-byte hex)")
		of := fs.String("offer-file", "", "offer protojson file (default stdin)")
		fs.Parse(os.Args[2:])
		o := loadOffer(*of)
		signOffer(o, priv(*p))
		body, _ := jsonM.Marshal(o)
		resp, err := http.Post(*httpURL+"/v1/offers", "application/json", bytes.NewReader(body))
		if err != nil {
			die("post: %v", err)
		}
		defer resp.Body.Close()
		out, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			die("relay %d: %s", resp.StatusCode, string(out))
		}
		fmt.Println(string(out))

	case "take":
		fs := flag.NewFlagSet("take", flag.ExitOnError)
		wsURL := fs.String("ws", "ws://127.0.0.1:9955/v1/ws", "relay WS URL")
		p := fs.String("priv", "", "taker private key (32-byte hex)")
		of := fs.String("offer-file", "", "offer protojson file (default stdin)")
		timeout := fs.Duration("timeout", 15*time.Second, "how long to wait for a match")
		fs.Parse(os.Args[2:])
		o := loadOffer(*of)
		signOffer(o, priv(*p))

		c, _, err := websocket.DefaultDialer.Dial(*wsURL, nil)
		if err != nil {
			die("dial ws: %v", err)
		}
		defer c.Close()
		c.SetReadDeadline(time.Now().Add(*timeout))

		send := func(to *seqobv1.To) {
			b, _ := jsonM.Marshal(to)
			if err := c.WriteMessage(websocket.TextMessage, b); err != nil {
				die("ws write: %v", err)
			}
		}
		// Subscribe first so the submit is processed on this connection and the
		// matched frame is pushed back here.
		send(&seqobv1.To{Msg: &seqobv1.To_MarketSubscribe{MarketSubscribe: o.GetPair()}})
		send(&seqobv1.To{Msg: &seqobv1.To_OfferSubmit{OfferSubmit: o}})

		for {
			_, data, err := c.ReadMessage()
			if err != nil {
				die("no match within %s: %v", *timeout, err)
			}
			var from seqobv1.From
			if err := jsonU.Unmarshal(data, &from); err != nil {
				continue
			}
			if m := from.GetMatched(); m != nil {
				b, _ := json.Marshal(map[string]interface{}{
					"session_id":          m.GetSessionId(),
					"offer_id":            m.GetOfferId(),
					"maker_pubkey":        m.GetMakerPubkey(),
					"role":                m.GetRole(),
					"fill_base_amount":    m.GetFillBaseAmount(),
					"fill_quote_amount":   m.GetFillQuoteAmount(),
					"resting_is_covenant": m.GetRestingIsCovenant(),
					"covenant_locked":     m.GetCovenantLocked(),
					"covenant":            covenantJSON(m.GetCovenant()),
				})
				fmt.Println(string(b))
				return
			}
			if e := from.GetError(); e != nil {
				die("relay error %d: %s", e.GetCode(), e.GetMessage())
			}
		}
	default:
		die("unknown subcommand %q (want post|take)", os.Args[1])
	}
}

func covenantJSON(c *seqobv1.CovenantTerms) map[string]interface{} {
	if c == nil {
		return nil
	}
	return map[string]interface{}{
		"covenant_txid":   c.GetCovenantTxid(),
		"covenant_vout":   c.GetCovenantVout(),
		"asset_a":         c.GetAssetA(),
		"asset_b":         c.GetAssetB(),
		"rate_num":        c.GetRateNum(),
		"rate_den":        c.GetRateDen(),
		"maker_prog":      hex.EncodeToString(c.GetMakerProg()),
		"maker_prog_ver":  c.GetMakerProgVer(),
		"min_lot":         c.GetMinLot(),
		"expiry_locktime": c.GetExpiryLocktime(),
	}
}
