package usage

import (
	"testing"
	"time"

	"github.com/yetone/magpie/internal/sessions"
)

// #5723: A ZCode request carries its session id without the sess_ prefix
// stored in its database. When a ZCode local call's session is stored as
// sess_<uuid> while the gateway logged <uuid>, fallback matching (by session,
// token counts, and end time within 2s) should normalize the sess_ prefix so
// the local call matches the gateway record and is not counted twice.
func TestZCodeSessionThroughMagpieCountedOnce(t *testing.T) {
	const rawUUID = "578bd3d1-834b-43fb-b40c-ed9d17ad7731"
	began := time.Date(2026, 11, 8, 16, 58, 28, 0, time.UTC)
	rec := Record{
		Time:      began,
		Agent:     "zcode",
		Provider:  "relay",
		Model:     "deepseek-v4.1-flash",
		Input:     307,
		CacheRead: 86656,
		Output:    280,
		Millis:    3000,
		Status:    200,
		Session:   rawUUID,
	}
	// ZCode database stores the session ID with the "sess_" prefix.
	call := sessions.Call{
		Time:    began.Add(3 * time.Second),
		Agent:   "zcode",
		Session: "sess_" + rawUUID,
		Tokens: sessions.Tokens{
			Input:     307,
			CacheRead: 86656,
			Output:    280,
		},
	}
	direct := sessions.Call{
		Time:    began.Add(10 * time.Minute),
		Agent:   "zcode",
		Session: "sess_different-session-uuid",
		Tokens: sessions.Tokens{
			Input:     307,
			CacheRead: 86656,
			Output:    280,
		},
	}
	m := gatewayMatches([]Record{rec}, []sessions.Call{call, direct})
	if !m[0] {
		t.Fatal("a ZCode local call with sess_ prefix was not matched to the gateway record")
	}
	if m[1] {
		t.Fatal("a distinct ZCode session was unexpectedly matched to the gateway record")
	}
}

// TestMatchOpensZCodeChunksWithSessPrefix verifies that chunkHints in
// match_chunks.go correctly hashes normalized session names, so a local chunk
// with sess_<uuid> is not prematurely pruned when the gateway has <uuid>.
func TestMatchOpensZCodeChunksWithSessPrefix(t *testing.T) {
	const rawUUID = "498cf3a2-1111-42ab-8899-abcdef012345"
	now := time.Now().Truncate(time.Second)
	gateway := &rowChunk{}
	gateway.add(Row{Record: Record{
		Time:     now,
		Agent:    "zcode",
		Provider: "relay",
		Session:  rawUUID,
		Input:    120,
		Output:   40,
		Millis:   1000,
		Status:   200,
	}}, "", 0, false)

	zcodeChunk := &rowChunk{Source: sessions.CallSource{Path: "/local/zcode-db"}}
	zcodeChunk.add(Row{Record: Record{
		Time:    now.Add(1 * time.Second),
		Agent:   "zcode",
		Session: "sess_" + rawUUID,
		Input:   120,
		Output:  40,
	}, Source: "log"}, "", 0, false)
	zcodeChunk.serial = nextChunkSerial()

	var opened []*rowChunk
	matchOpening = func(c *rowChunk) { opened = append(opened, c) }
	t.Cleanup(func() { matchOpening = nil })

	got := refMap(matchedBlocks([]*rowChunk{gateway}, []*rowChunk{zcodeChunk}, nil, nil, time.Time{}, false))
	if !got[rowRef{zcodeChunk, 0}] || len(got) != 1 {
		t.Fatalf("matched %v; want the zcode local call matched", got)
	}
	if len(opened) != 1 || opened[0] != zcodeChunk {
		t.Fatalf("decoded %d chunks; want zcodeChunk opened, got %v", len(opened), opened)
	}
}
