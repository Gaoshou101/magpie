package mcpauth_test

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/mcpauth"
	"github.com/yetone/magpie/internal/mcpauth/mcpauthtest"
)

func wrapTokenEndpoint(t *testing.T, f *mcpauthtest.Fake, onRefresh func(w http.ResponseWriter, r *http.Request) bool) string {
	t.Helper()
	var proxyURL string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bodyBytes, _ := io.ReadAll(r.Body)
		if strings.HasSuffix(r.URL.Path, "/token") && strings.Contains(string(bodyBytes), "grant_type=refresh_token") {
			if onRefresh(w, r) {
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
	return proxyURL + "/mcp"
}

func TestRenew429RetryAfterCooldown(t *testing.T) {
	home(t)
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	t.Cleanup(mcpauth.SetClockForTest(func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}))
	advance := func(d time.Duration) {
		mu.Lock()
		now = now.Add(d)
		mu.Unlock()
	}

	f := mcpauthtest.New(t)
	f.ExpiresIn = 60 // inside renewLead so Token() immediately tries to renew
	var refreshCalls atomic.Int32
	var force429 atomic.Bool
	force429.Store(true)
	var retryHeader atomic.Value
	retryHeader.Store("60")

	u := wrapTokenEndpoint(t, f, func(w http.ResponseWriter, r *http.Request) bool {
		refreshCalls.Add(1)
		if force429.Load() {
			if h, _ := retryHeader.Load().(string); h != "" {
				w.Header().Set("Retry-After", h)
			}
			http.Error(w, `{"error":"rate_limited"}`, http.StatusTooManyRequests)
			return true
		}
		return false
	})

	mcpauthtest.SignIn(t, "canva", u)
	oldRec, ok := mcpauth.Get("canva")
	if !ok {
		t.Fatal("missing sign-in record")
	}

	// First renewal hits 429 with Retry-After: 60.
	_, err := mcpauth.Token(t.Context(), "canva")
	if !errors.Is(err, mcpauth.ErrRateLimited) {
		t.Fatalf("first Token() err = %v, want ErrRateLimited", err)
	}
	var rle *mcpauth.RateLimitError
	if !errors.As(err, &rle) || !rle.RetryAt.Equal(now.Add(60*time.Second)) {
		t.Fatalf("RateLimitError = %+v, want RetryAt=%v", rle, now.Add(60*time.Second))
	}
	if refreshCalls.Load() != 1 {
		t.Fatalf("refreshCalls = %d, want 1", refreshCalls.Load())
	}
	st := mcpauth.StatusOf("canva", u)
	if !st.SignedIn || st.Dead || st.RetryAt != now.Add(60*time.Second).UnixMilli() {
		t.Fatalf("StatusOf during cooldown = %+v", st)
	}

	// During the 60s cooldown, repeated Token and Renew calls do not hit /token.
	advance(59 * time.Second)
	for range 3 {
		if _, err := mcpauth.Token(t.Context(), "canva"); !errors.Is(err, mcpauth.ErrRateLimited) {
			t.Fatalf("Token during cooldown err = %v", err)
		}
		if _, err := mcpauth.Renew(t.Context(), "canva", oldRec.Access); !errors.Is(err, mcpauth.ErrRateLimited) {
			t.Fatalf("Renew during cooldown err = %v", err)
		}
	}
	if refreshCalls.Load() != 1 {
		t.Fatalf("refreshCalls during cooldown = %d, want 1", refreshCalls.Load())
	}

	// Once cooldown elapses, StatusOf clears RetryAt and concurrent Token calls
	// perform only a single renewal.
	advance(2 * time.Second)
	if st := mcpauth.StatusOf("canva", u); st.RetryAt != 0 {
		t.Fatalf("StatusOf after cooldown = %+v, want RetryAt=0", st)
	}
	f.ExpiresIn = 3600
	force429.Store(false)

	const workers = 4
	toks := make([]string, workers)
	errs := make([]error, workers)
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := range workers {
		go func(idx int) {
			defer wg.Done()
			toks[idx], errs[idx] = mcpauth.Token(t.Context(), "canva")
		}(i)
	}
	wg.Wait()

	for i := range workers {
		if errs[i] != nil || toks[i] == "" || toks[i] == oldRec.Access {
			t.Fatalf("worker %d: tok=%q err=%v", i, toks[i], errs[i])
		}
		if toks[i] != toks[0] {
			t.Fatalf("worker %d got different token %q != %q", i, toks[i], toks[0])
		}
	}
	if refreshCalls.Load() != 2 {
		t.Fatalf("refreshCalls after concurrent post-cooldown recovery = %d, want 2", refreshCalls.Load())
	}
	recAfter, _ := mcpauth.Get("canva")
	if recAfter.RetryAt != 0 || recAfter.RateLimits != 0 || recAfter.Dead {
		t.Fatalf("record after recovery = %+v, want cleared cooldown", recAfter)
	}

	// Also verify HTTP-date Retry-After when token becomes due again.
	advance(3550 * time.Second) // f.ExpiresIn=3600, so now within renewLead (120s)
	force429.Store(true)
	retryHeader.Store(now.Add(45 * time.Second).UTC().Format(http.TimeFormat))
	_, err = mcpauth.Token(t.Context(), "canva")
	if !errors.As(err, &rle) || rle.RetryAt.Sub(now) != 45*time.Second {
		t.Fatalf("HTTP-date Retry-After: err=%v rle=%+v", err, rle)
	}
}

func TestRenew429BackoffWithoutRetryAfterAndSwitchAccount(t *testing.T) {
	home(t)
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	t.Cleanup(mcpauth.SetClockForTest(func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}))
	advance := func(d time.Duration) {
		mu.Lock()
		now = now.Add(d)
		mu.Unlock()
	}

	f := mcpauthtest.New(t)
	f.ExpiresIn = 60
	var refreshCalls atomic.Int32
	var force429 atomic.Bool
	force429.Store(true)

	u := wrapTokenEndpoint(t, f, func(w http.ResponseWriter, r *http.Request) bool {
		refreshCalls.Add(1)
		if force429.Load() {
			// No Retry-After header -> exponential backoff: 30s, 60s, 120s... up to 15m.
			http.Error(w, `{"error":"rate_limited"}`, http.StatusTooManyRequests)
			return true
		}
		return false
	})

	mcpauthtest.SignInAccount(t, "canva", u, "acct-1")
	mcpauthtest.SignInAccount(t, "canva", u, "acct-2")
	accts := mcpauth.Accounts("canva")
	if len(accts) != 2 {
		t.Fatalf("accounts = %+v", accts)
	}
	// acct-2 is currently active; switch to acct-1 and trigger 429s on it.
	if err := mcpauth.Use("canva", accts[0].ID); err != nil {
		t.Fatal(err)
	}

	wantDelays := []time.Duration{
		30 * time.Second,
		60 * time.Second,
		120 * time.Second,
		240 * time.Second,
		480 * time.Second,
		15 * time.Minute,
		15 * time.Minute,
	}
	for i, want := range wantDelays {
		_, err := mcpauth.Token(t.Context(), "canva")
		var rle *mcpauth.RateLimitError
		if !errors.As(err, &rle) {
			t.Fatalf("step %d: err=%v, want RateLimitError", i, err)
		}
		if got := rle.RetryAt.Sub(now); got != want {
			t.Fatalf("step %d: backoff=%v, want %v", i, got, want)
		}
		if gotCalls := refreshCalls.Load(); gotCalls != int32(i+1) {
			t.Fatalf("step %d: refreshCalls=%d, want %d", i, gotCalls, i+1)
		}
		// Inside cooldown: blocked without HTTP call.
		advance(want - time.Second)
		if _, err := mcpauth.Token(t.Context(), "canva"); !errors.Is(err, mcpauth.ErrRateLimited) {
			t.Fatalf("step %d inside cooldown: err=%v", i, err)
		}
		if gotCalls := refreshCalls.Load(); gotCalls != int32(i+1) {
			t.Fatalf("step %d inside cooldown: refreshCalls=%d, want %d", i, gotCalls, i+1)
		}
		advance(2 * time.Second)
	}

	// Switching to acct-2 is unaffected by acct-1's cooldown and can renew.
	force429.Store(false)
	if err := mcpauth.Use("canva", accts[1].ID); err != nil {
		t.Fatal(err)
	}
	if tok, err := mcpauth.Token(t.Context(), "canva"); err != nil || tok == "" {
		t.Fatalf("acct-2 Token() = %q, %v", tok, err)
	}

	// Switch back to acct-1, let it succeed once, then confirm backoff resets to 30s.
	if err := mcpauth.Use("canva", accts[0].ID); err != nil {
		t.Fatal(err)
	}
	if tok, err := mcpauth.Token(t.Context(), "canva"); err != nil || tok == "" {
		t.Fatalf("acct-1 recovery Token() = %q, %v", tok, err)
	}
	advance(50 * time.Second)
	force429.Store(true)
	_, err := mcpauth.Token(t.Context(), "canva")
	var rle *mcpauth.RateLimitError
	if !errors.As(err, &rle) || rle.RetryAt.Sub(now) != 30*time.Second {
		t.Fatalf("acct-1 post-recovery backoff = %+v, want 30s", rle)
	}
}
