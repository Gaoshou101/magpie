package gui

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/settings"
	"github.com/yetone/magpie/internal/update"
)

// #7614 (bbingz): POST /api/update/check used to run updates.check()
// synchronously, blocking the HTTP response across the entire release download.
// It must return as soon as the feed check reaches "downloading" so the UI
// can clear updateBusy and poll /api/update for live download progress.
func TestUpdateCheckDoesNotBlockAcrossDownload(t *testing.T) {
	h := t.TempDir()
	t.Setenv("HOME", h)
	t.Setenv("USERPROFILE", h)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(h, ".config"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(h, ".cache"))

	const chunk = 4 << 10
	body := make([]byte, chunk*2)
	for i := range body {
		body[i] = byte(i + 1)
	}
	sum := sha256.Sum256(body)

	downloadStarted := make(chan struct{})
	releaseDownload := make(chan struct{})
	var closeReleaseOnce sync.Once
	release := func() {
		closeReleaseOnce.Do(func() { close(releaseDownload) })
	}

	var feedHits, downloads atomic.Int32
	var startedOnce sync.Once

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/asset" {
			downloads.Add(1)
			w.Header().Set("Content-Length", "8192")
			w.Write(body[:chunk])
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			startedOnce.Do(func() { close(downloadStarted) })
			select {
			case <-releaseDownload:
			case <-r.Context().Done():
				return
			}
			w.Write(body[chunk:])
			return
		}
		feedHits.Add(1)
		json.NewEncoder(w).Encode(update.Release{
			Version: "0.1.11",
			Notes:   "**Urgent:** security fix",
			Assets: map[string]update.Asset{
				update.BinaryAsset(): {
					URL:    "http://" + r.Host + "/asset",
					Size:   int64(len(body)),
					SHA256: hex.EncodeToString(sum[:]),
				},
			},
		})
	}))
	defer srv.Close()
	t.Setenv("MAGPIE_UPDATE_FEED", srv.URL)

	oldTicks, oldTick := recheckTicks, recheckTick
	recheckTicks, recheckTick = 100, 10*time.Millisecond
	t.Cleanup(func() { recheckTicks, recheckTick = oldTicks, oldTick })

	oldV := Version
	Version = "0.1.10"
	defer func() { Version = oldV }()

	exe, err := update.Executable()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(exe + ".new")

	s := settings.Load()
	s.NoAutoDownload = false
	if err := settings.Save(s); err != nil {
		t.Fatal(err)
	}

	o := updates
	u := &updater{exe: exe}
	u.self, _ = os.Stat(exe)
	updates = u
	defer func() { updates = o }()

	mux := http.NewServeMux()
	updateRoutes(mux, &quitCount{})

	var wg sync.WaitGroup
	defer func() {
		release()
		wg.Wait()
	}()

	done := make(chan updateJSON, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("POST", "/api/update/check", strings.NewReader("{}")))
		var j updateJSON
		if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &j) != nil {
			t.Errorf("POST /api/update/check: %d %s", rec.Code, rec.Body)
		}
		done <- j
	}()

	select {
	case <-downloadStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("download never started")
	}

	var first updateJSON
	select {
	case first = <-done:
	case <-time.After(1 * time.Second):
		t.Fatal("POST /api/update/check blocked while download was in flight")
	}
	if first.State != "downloading" || first.Latest != "0.1.11" {
		t.Fatalf("expected downloading 0.1.11 while asset blocked, got %+v", first)
	}

	// A second POST /api/update/check while downloading also returns promptly
	// without blocking, and marks supersede (again) so checkByClock re-checks
	// once the active download finishes.
	secondDone := make(chan updateJSON, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("POST", "/api/update/check", strings.NewReader("{}")))
		var j updateJSON
		if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &j) != nil {
			t.Errorf("second POST /api/update/check: %d %s", rec.Code, rec.Body)
		}
		secondDone <- j
	}()
	select {
	case second := <-secondDone:
		if second.State != "downloading" {
			t.Fatalf("expected downloading on second check, got %+v", second)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("second POST /api/update/check blocked while downloading")
	}

	release()
	// Wait for the active download AND the superseded checkByClock run to settle.
	for range 500 {
		j := u.json()
		if j.State == "ready" && downloads.Load() == 1 && feedHits.Load() >= 2 && !u.asking() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if j := u.json(); j.State != "ready" || j.Latest != "0.1.11" || feedHits.Load() < 2 {
		t.Fatalf("expected ready 0.1.11 after superseded recheck settled, got %+v (feedHits=%d)", j, feedHits.Load())
	}
}

func TestUpdateCheckReturnsCheckingOnSlowFeedOrCanceledContext(t *testing.T) {
	h := t.TempDir()
	t.Setenv("HOME", h)
	t.Setenv("USERPROFILE", h)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(h, ".config"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(h, ".cache"))

	var feedEntered chan struct{}
	var releaseFeed chan struct{}
	var closeFeedOnce sync.Once
	var enteredOnce sync.Once
	release := func() {
		closeFeedOnce.Do(func() { close(releaseFeed) })
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		enteredOnce.Do(func() { close(feedEntered) })
		select {
		case <-releaseFeed:
		case <-r.Context().Done():
			return
		}
		json.NewEncoder(w).Encode(update.Release{Version: "0.1.10"})
	}))
	defer srv.Close()
	t.Setenv("MAGPIE_UPDATE_FEED", srv.URL)

	oldTicks, oldTick := recheckTicks, recheckTick
	recheckTicks, recheckTick = 3, 10*time.Millisecond
	t.Cleanup(func() { recheckTicks, recheckTick = oldTicks, oldTick })

	oldV := Version
	Version = "0.1.10"
	defer func() { Version = oldV }()

	o := updates
	defer func() { updates = o }()

	var wg sync.WaitGroup
	defer func() {
		release()
		wg.Wait()
	}()

	// Subcase 1: client context cancellation returns "checking" promptly.
	feedEntered = make(chan struct{})
	releaseFeed = make(chan struct{})
	u1 := &updater{}
	updates = u1

	mux := http.NewServeMux()
	updateRoutes(mux, &quitCount{})

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest("POST", "/api/update/check", strings.NewReader("{}")).WithContext(ctx)
	done1 := make(chan updateJSON, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		var j updateJSON
		_ = json.Unmarshal(rec.Body.Bytes(), &j)
		done1 <- j
	}()

	<-feedEntered
	cancel()
	select {
	case j := <-done1:
		if j.State != "checking" {
			t.Fatalf("expected checking state on canceled feed, got %+v", j)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("POST /api/update/check did not return on canceled context")
	}

	release()
	for range 200 {
		if u1.json().State == "latest" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if j := u1.json(); j.State != "latest" {
		t.Fatalf("expected u1 background check to settle to latest, got %+v", j)
	}

	// Subcase 2: slow feed exceeding recheckTicks*recheckTick returns "checking".
	feedEntered = make(chan struct{})
	releaseFeed = make(chan struct{})
	closeFeedOnce = sync.Once{}
	enteredOnce = sync.Once{}
	u2 := &updater{}
	updates = u2

	done2 := make(chan updateJSON, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("POST", "/api/update/check", strings.NewReader("{}")))
		var j updateJSON
		_ = json.Unmarshal(rec.Body.Bytes(), &j)
		done2 <- j
	}()

	<-feedEntered
	select {
	case j := <-done2:
		if j.State != "checking" {
			t.Fatalf("expected checking state when feed exceeds wait budget, got %+v", j)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("POST /api/update/check did not return after wait budget")
	}

	release()
	for range 200 {
		if u2.json().State == "latest" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("expected u2 background check to settle to latest, got %+v", u2.json())
}
