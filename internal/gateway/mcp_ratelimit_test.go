package gateway

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/mcpauth"
	"github.com/yetone/magpie/internal/mcpauth/mcpauthtest"
)

func TestMCPProxyRateLimitedReturns429AndCoolsDown(t *testing.T) {
	fresh(t)
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	t.Cleanup(mcpauth.SetClockForTest(func() time.Time { return now }))

	f := mcpauthtest.New(t)
	f.ExpiresIn = 60 // due immediately
	var refreshCalls atomic.Int32
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

	mcpauthtest.SignIn(t, "canva", proxyURL+"/mcp")
	gw := httptest.NewServer(New().Handler())
	defer gw.Close()

	for i := range 3 {
		req, _ := http.NewRequest(http.MethodPost, gw.URL+"/mcp/canva", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+Token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("WWW-Authenticate") != "" || !strings.Contains(string(b), "rate-limited") {
			t.Fatalf("call %d: status=%d hdr=%v body=%s", i, resp.StatusCode, resp.Header, b)
		}
	}
	if refreshCalls.Load() != 1 {
		t.Fatalf("refreshCalls = %d, want 1", refreshCalls.Load())
	}
	if st := mcpauth.StatusOf("canva", proxyURL+"/mcp"); !st.SignedIn || st.Dead || st.RetryAt == 0 {
		t.Fatalf("StatusOf = %+v, want alive signed-in with RetryAt", st)
	}
}
