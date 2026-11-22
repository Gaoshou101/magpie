package library

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/gateway"
	"github.com/yetone/magpie/internal/mcpauth"
	"github.com/yetone/magpie/internal/mcpauth/mcpauthtest"
)

// TestHub429CooldownThroughRouter exercises the full #7451 scenario:
//  1. Server A (canva) uses OAuth whose token is due for renewal, and /token returns 429 + Retry-After: 60.
//  2. Server B (srvb) is healthy.
//  3. Through the MCP router, tools/list triggers one renewal on A (429) while still serving B.
//  4. Subsequent router calls to B during the 60s cooldown succeed without re-hitting A's /token.
//  5. CheckServers reports A as rate-limited (State="error", Why="ratelimited", SignIn.RetryAt > 0)
//     rather than "auth" ("needs sign-in"), and caches during cooldown.
//  6. After the 60s cooldown expires, concurrent calls needing A perform only one renewal and succeed.
func TestHub429CooldownThroughRouter(t *testing.T) {
	hubSandbox(t)
	checked.Lock()
	checked.m = map[string]checkedServer{}
	checked.Unlock()

	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	var clkMu sync.Mutex
	t.Cleanup(mcpauth.SetClockForTest(func() time.Time {
		clkMu.Lock()
		defer clkMu.Unlock()
		return now
	}))
	advance := func(d time.Duration) {
		clkMu.Lock()
		now = now.Add(d)
		clkMu.Unlock()
	}

	var refreshCalls atomic.Int32
	var force429 atomic.Bool
	force429.Store(true)

	f := mcpauthtest.New(t)
	f.ExpiresIn = 60 // inside mcpauth renewLead, so Token() immediately tries to renew

	var proxyURL string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bodyBytes, _ := io.ReadAll(r.Body)
		if strings.HasSuffix(r.URL.Path, "/token") && strings.Contains(string(bodyBytes), "grant_type=refresh_token") {
			refreshCalls.Add(1)
			if force429.Load() {
				w.Header().Set("Retry-After", "60")
				http.Error(w, `{"error":"rate_limited"}`, http.StatusTooManyRequests)
				return
			}
		}
		req, err := http.NewRequestWithContext(r.Context(), r.Method, f.Server.URL+r.URL.RequestURI(), bytes.NewReader(bodyBytes))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		req.Header = r.Header.Clone()
		resp, err := (&http.Client{
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}).Do(req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		respBytes, _ := io.ReadAll(resp.Body)
		respBytes = bytes.ReplaceAll(respBytes, []byte(f.Server.URL), []byte(proxyURL))
		if r.URL.Path == "/mcp" && resp.StatusCode == http.StatusOK {
			respBytes = bytes.ReplaceAll(respBytes, []byte(`{"method":"initialize"}`), []byte(`{"protocolVersion":"2025-03-26","capabilities":{}}`))
			respBytes = bytes.ReplaceAll(respBytes, []byte(`{"method":"tools/list"}`), []byte(`{"tools":[{"name":"design"}]}`))
		}
		for k, vv := range resp.Header {
			if strings.EqualFold(k, "Content-Length") {
				continue
			}
			for _, v := range vv {
				w.Header().Add(k, strings.ReplaceAll(v, f.Server.URL, proxyURL))
			}
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(respBytes)
	}))
	proxyURL = proxy.URL
	t.Cleanup(proxy.Close)

	var seenB http.Header
	srvB := httptest.NewServer(mcpHandler(t, false, &seenB))
	t.Cleanup(srvB.Close)

	ok(t)(SaveServer("", Server{Name: "canva", Transport: "http", URL: proxyURL + "/mcp", Agents: []string{"codex"}}))
	ok(t)(SaveServer("", Server{Name: "srvb", Transport: "http", URL: srvB.URL, Agents: []string{"codex"}}))
	mcpauthtest.SignIn(t, "canva", proxyURL+"/mcp")
	ok(t)(SetMCPHub(true))

	gw := httptest.NewServer(gateway.New().Handler())
	t.Cleanup(gw.Close)

	callRouter := func(method string, params any) map[string]any {
		t.Helper()
		payload, _ := json.Marshal(map[string]any{
			"jsonrpc": "2.0",
			"id":      1,
			"method":  method,
			"params":  params,
		})
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, gw.URL+"/mcp/magpie/hub/codex", bytes.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s status %d: %s", method, resp.StatusCode, b)
		}
		var out map[string]any
		if err := json.Unmarshal(b, &out); err != nil {
			t.Fatalf("%s json %s: %v", method, b, err)
		}
		return out
	}

	// Step 1: tools/list lists srvB tools while triggering one renewal on canva (429).
	resList := callRouter("tools/list", map[string]any{})
	if resList["error"] != nil {
		t.Fatalf("tools/list failed: %v", resList["error"])
	}
	if got := refreshCalls.Load(); got != 1 {
		t.Fatalf("after tools/list: canva refreshCalls = %d, want 1", got)
	}

	// Step 2: calling tools on srvB during the 60s cooldown must succeed and never re-hit canva's /token.
	advance(10 * time.Second)
	for i := range 3 {
		resCall := callRouter("tools/call", map[string]any{
			"name":      "srvb__a",
			"arguments": map[string]any{},
		})
		if resCall["error"] != nil {
			t.Fatalf("call %d to srvb failed: %v", i, resCall["error"])
		}
	}
	if got := refreshCalls.Load(); got != 1 {
		t.Fatalf("after 3 tools/call to srvB during cooldown: canva refreshCalls = %d, want 1", got)
	}

	// Step 3: CheckServers must report rate-limited rather than "auth" ("needs sign-in").
	m, err := CheckServers(context.Background(), []string{"canva", "srvb"}, false)
	if err != nil {
		t.Fatal(err)
	}
	hc := m["canva"]
	if hc.State != "error" || hc.Why != "ratelimited" || hc.SignIn == nil || !hc.SignIn.SignedIn || hc.SignIn.Dead || hc.SignIn.RetryAt <= now.UnixMilli() {
		t.Fatalf("CheckServers(canva) = %+v, want error/ratelimited with active SignIn.RetryAt", hc)
	}
	if m["srvb"].State != "ok" {
		t.Fatalf("CheckServers(srvb) = %+v, want ok", m["srvb"])
	}
	if got := refreshCalls.Load(); got != 1 {
		t.Fatalf("after CheckServers during cooldown: canva refreshCalls = %d, want 1", got)
	}

	// Step 4: after the 60s cooldown ends, concurrent requests needing canva renew only once and succeed.
	advance(51 * time.Second)
	f.ExpiresIn = 3600
	force429.Store(false)

	var wg sync.WaitGroup
	wg.Add(2)
	var listA, callA map[string]any
	go func() {
		defer wg.Done()
		listA = callRouter("tools/list", map[string]any{})
	}()
	go func() {
		defer wg.Done()
		callA = callRouter("tools/call", map[string]any{
			"name":      "canva__design",
			"arguments": map[string]any{},
		})
	}()
	wg.Wait()

	if listA["error"] != nil {
		t.Fatalf("post-cooldown tools/list failed: %v", listA["error"])
	}
	if callA["error"] != nil {
		t.Fatalf("post-cooldown canva__design call failed: %v", callA["error"])
	}
	if got := refreshCalls.Load(); got != 2 {
		t.Fatalf("post-cooldown concurrent calls: canva refreshCalls = %d, want 2", got)
	}

	// CheckServers (fresh=false) sees RetryAt cleared in healthKey, so it re-checks and sees ok.
	m, err = CheckServers(context.Background(), []string{"canva"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if m["canva"].State != "ok" || m["canva"].SignIn == nil || m["canva"].SignIn.RetryAt != 0 {
		t.Fatalf("post-recovery CheckServers(canva) = %+v, want ok with RetryAt=0", m["canva"])
	}
}

// TestCheckServer401Then429ReportsRateLimited verifies that when an access token is
// rejected with 401 and the resulting Renew() receives 429, CheckServer reports
// rate-limited instead of "needs sign-in", and keeps the sign-in alive.
func TestCheckServer401Then429ReportsRateLimited(t *testing.T) {
	sandbox(t)
	checked.Lock()
	checked.m = map[string]checkedServer{}
	checked.Unlock()

	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	t.Cleanup(mcpauth.SetClockForTest(func() time.Time { return now }))

	var refreshCalls atomic.Int32
	f := mcpauthtest.New(t)
	var proxyURL string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bodyBytes, _ := io.ReadAll(r.Body)
		if strings.HasSuffix(r.URL.Path, "/token") && strings.Contains(string(bodyBytes), "grant_type=refresh_token") {
			refreshCalls.Add(1)
			w.Header().Set("Retry-After", "60")
			http.Error(w, `{"error":"rate_limited"}`, http.StatusTooManyRequests)
			return
		}
		req, err := http.NewRequestWithContext(r.Context(), r.Method, f.Server.URL+r.URL.RequestURI(), bytes.NewReader(bodyBytes))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		req.Header = r.Header.Clone()
		resp, err := (&http.Client{
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}).Do(req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		respBytes, _ := io.ReadAll(resp.Body)
		respBytes = bytes.ReplaceAll(respBytes, []byte(f.Server.URL), []byte(proxyURL))
		for k, vv := range resp.Header {
			if strings.EqualFold(k, "Content-Length") {
				continue
			}
			for _, v := range vv {
				w.Header().Add(k, strings.ReplaceAll(v, f.Server.URL, proxyURL))
			}
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(respBytes)
	}))
	proxyURL = proxy.URL
	t.Cleanup(proxy.Close)

	ok(t)(SaveServer("", Server{Name: "canva", Transport: "http", URL: proxyURL + "/mcp"}))
	mcpauthtest.SignIn(t, "canva", proxyURL+"/mcp")
	f.Revoke()

	h := CheckServer(context.Background(), &Server{Name: "canva", Transport: "http", URL: proxyURL + "/mcp"})
	if h.State != "error" || h.Why != "ratelimited" || h.SignIn == nil || h.SignIn.Dead || h.SignIn.RetryAt == 0 {
		t.Fatalf("CheckServer after 401->429 = %+v", h)
	}
	// A forced re-check during cooldown does not hit /token again.
	m, err := CheckServers(context.Background(), []string{"canva"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if m["canva"].Why != "ratelimited" || refreshCalls.Load() != 1 {
		t.Fatalf("forced re-check during cooldown: health=%+v refreshCalls=%d", m["canva"], refreshCalls.Load())
	}
}
