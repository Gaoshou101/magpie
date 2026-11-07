package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/usage"
)

// A Claude Code subagent's call one magpie passes on to another (a Remote
// magpie provider) keeps the subagent Claude Code named it by
// (x-claude-code-agent-id, x-claude-code-parent-agent-id), so the other
// magpie's Context splits it from the main session as this one does; they
// were dropped, and the remote counted the subagent's calls as the main
// session's (#5608). The conversation's own turns, which name no subagent,
// go on naming none, and the vendor behind the other magpie is sent neither.
func TestRemoteMagpieKeepsTheSubagent(t *testing.T) {
	var mu sync.Mutex
	var vendor []http.Header // as the vendor saw each request
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		mu.Lock()
		vendor = append(vendor, r.Header.Clone())
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"chatcmpl-1","object":"chat.completion","model":"m1","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`)
	}))
	t.Cleanup(up.Close)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	if err := provider.Save(provider.Provider{ID: "fake", Name: "Fake", Key: "k", Models: []string{"m1"}, Chat: up.URL}); err != nil {
		t.Fatal(err)
	}
	var arrived []http.Header // as the remote magpie was asked
	h := New().Handler()
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		arrived = append(arrived, r.Header.Clone())
		mu.Unlock()
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(remote.Close)
	if err := provider.Save(provider.Provider{ID: "office", Name: "Office", Preset: provider.RemoteMagpiePreset, Key: "rk", Models: []string{"fake/m1"},
		Chat: remote.URL + "/v1", Responses: remote.URL + "/v1", Anthropic: remote.URL}); err != nil {
		t.Fatal(err)
	}

	there := func(n int) usage.Record {
		t.Helper()
		for range 100 {
			var rs []usage.Record
			for _, r := range usage.Load(time.Time{}) {
				if r.Provider == "fake" {
					rs = append(rs, r)
				}
			}
			if len(rs) >= n {
				return rs[n-1]
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("no record %d of the remote's", n)
		return usage.Record{}
	}
	for i, tc := range []struct{ name, sub, parent string }{
		{"subagent", "agent-sub-1", "agent-lead-1"},
		{"subagent of the lead", "agent-sub-2", ""},
		{"main session", "", ""},
	} {
		req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"office/fake/m1","messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("User-Agent", "claude-cli/2.1.0 (external, cli)")
		req.Header.Set("Authorization", "Bearer "+TokenFor("claude-code"))
		req.Header.Set("x-claude-code-session-id", "cc-session-A")
		if tc.sub != "" {
			req.Header.Set("x-claude-code-agent-id", tc.sub)
		}
		if tc.parent != "" {
			req.Header.Set("x-claude-code-parent-agent-id", tc.parent)
		}
		rec := httptest.NewRecorder()
		New().Handler().ServeHTTP(rec, req)
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), "hello") {
			t.Fatalf("%s: %d %s", tc.name, rec.Code, rec.Body.String())
		}
		mu.Lock()
		got := arrived[len(arrived)-1]
		mu.Unlock()
		if got.Get("x-claude-code-agent-id") != tc.sub || got.Get("x-claude-code-parent-agent-id") != tc.parent {
			t.Errorf("%s: the remote magpie was sent agent-id %q parent %q; want %q, %q", tc.name, got.Get("x-claude-code-agent-id"), got.Get("x-claude-code-parent-agent-id"), tc.sub, tc.parent)
		}
		if r := there(i + 1); r.Subagent != tc.sub || r.ParentAgent != tc.parent || r.NativeSession != "cc-session-A" {
			t.Errorf("%s: the remote recorded subagent %q parent %q native %q; want %q, %q, cc-session-A", tc.name, r.Subagent, r.ParentAgent, r.NativeSession, tc.sub, tc.parent)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	for _, v := range vendor {
		for k := range v {
			if lk := strings.ToLower(k); lk == "x-claude-code-agent-id" || lk == "x-claude-code-parent-agent-id" {
				t.Errorf("the vendor was sent %s", k)
			}
		}
	}
}
