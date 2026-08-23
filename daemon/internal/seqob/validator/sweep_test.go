package validator

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// Rate-limit keys are pruned only when hit again, so keys never seen twice (a fresh
// pubkey per submission is free to mint) must be swept once their window empties.
func TestRateMapsSweepEmptyKeys(t *testing.T) {
	clock := time.Unix(1750000000, 0)
	c := DefaultConfig()
	c.Now = func() time.Time { return clock }
	v := New(c, nil)
	for i := 0; i < 200; i++ {
		o := signed(t, key(t), nil)
		if err := v.ValidateOffer(context.Background(), o, fmt.Sprintf("10.0.0.%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	if len(v.pubHits) != 200 || len(v.ipHits) != 200 {
		t.Fatalf("expected 200 keys each, got pub=%d ip=%d", len(v.pubHits), len(v.ipHits))
	}
	clock = clock.Add(2 * time.Minute)
	if err := v.ValidateOffer(context.Background(), signed(t, key(t), nil), "10.1.1.1"); err != nil {
		t.Fatal(err)
	}
	if len(v.pubHits) != 1 || len(v.ipHits) != 1 {
		t.Fatalf("stale keys not swept: pub=%d ip=%d", len(v.pubHits), len(v.ipHits))
	}
}
