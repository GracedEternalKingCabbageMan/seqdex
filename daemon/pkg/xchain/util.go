package xchain

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
)

func toHex(b []byte) string { return hex.EncodeToString(b) }

func fromHex(s string) ([]byte, error) { return hex.DecodeString(s) }

// hexEq reports whether the hex string decodes to exactly b. A malformed hex
// string is treated as "not equal" (never a match).
func hexEq(hexStr string, b []byte) bool {
	got, err := hex.DecodeString(hexStr)
	if err != nil {
		return false
	}
	return bytes.Equal(got, b)
}

// hashEqualsPreimage reports whether SHA256(preimage) == hash. This is the
// submarine-swap binding check: the same H gates both the LN HTLC and the
// Sequentia on-chain HTLC.
func hashEqualsPreimage(hash, preimage []byte) bool {
	h := sha256.Sum256(preimage)
	return bytes.Equal(h[:], hash)
}
