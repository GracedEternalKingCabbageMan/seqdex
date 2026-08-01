// Package registry is a minimal, read-only client for the Sequentia Asset
// Registry's "minimal index" endpoint. It exists so market creation can source
// an asset's on-chain precision (nDenomination / decimal places) from the
// registry instead of relying on a manually supplied CLI flag.
//
// The minimal index is a JSON object keyed by the (display) asset id, whose
// value is the array [domain, ticker, name, precision, verified] - precision is
// field index 3. See the registry server's minimalIndex(). Everything here is
// fail-soft: any network/parse error yields "not found", never a hard error, so
// an unreachable registry can never block market creation.
package registry

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// precisionIndex is field index 3 of a minimal-index entry
// [domain, ticker, name, precision, verified].
const precisionIndex = 3

// Client fetches asset precision from a registry minimal-index URL.
type Client struct {
	url  string
	http *http.Client
}

// NewClient returns a registry client for the given minimal-index URL (for
// example http://localhost:3005/index.minimal.json). It returns nil when url is
// empty, so callers can treat "no registry configured" as a nil client and skip
// lookups entirely.
func NewClient(url string) *Client {
	if url == "" {
		return nil
	}
	return &Client{
		url:  url,
		http: &http.Client{Timeout: 5 * time.Second},
	}
}

// PrecisionFor returns the registry precision for the given (display) asset id
// and true when the asset is present in the index. It returns (0, false) on a
// nil client, an unreachable/invalid registry, or an unknown asset - callers
// must fall back to their own default in that case. It never returns an error:
// registry unavailability must not break market creation.
func (c *Client) PrecisionFor(assetID string) (uint, bool) {
	if c == nil {
		return 0, false
	}
	idx, err := c.fetch()
	if err != nil {
		return 0, false
	}
	entry, ok := idx[assetID]
	if !ok || len(entry) <= precisionIndex {
		return 0, false
	}
	// precision arrives as a JSON number -> float64.
	p, ok := entry[precisionIndex].(float64)
	if !ok || p < 0 {
		return 0, false
	}
	return uint(p), true
}

// fetch downloads and decodes the minimal index (id -> [..] array).
func (c *Client) fetch() (map[string][]interface{}, error) {
	resp, err := c.http.Get(c.url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("registry returned status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	var idx map[string][]interface{}
	if err := json.Unmarshal(body, &idx); err != nil {
		return nil, err
	}
	return idx, nil
}
