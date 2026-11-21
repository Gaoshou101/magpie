package catalog

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// A Claude account's list fetches the catalog again every 45 minutes
// (Asked), most often to the same bytes. Each fetch forgot the parsed
// catalog, told every agent to rewrite its list (Changed) and, as usage
// keyed its index on the file's mtime, had the next usage query read the
// whole session history again. The same bytes are only dated now; bytes
// that changed, in either file, are kept and told as before.
func TestSameCatalogFetchedAgainChangesNothing(t *testing.T) {
	freshCache(t)
	var mu sync.Mutex
	api := `{"openai":{"id":"openai","models":{"gpt-6.1-sol":{"id":"gpt-6.1-sol","cost":{"input":2,"output":10}}}}}`
	labs := `{"openai/gpt-6.1-sol":{"id":"gpt-6.1-sol","limit":{"context":400000}}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/api.json":
			w.Write([]byte(api))
		case "/models.json":
			w.Write([]byte(labs))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	pointAt(t, srv.URL+"/api.json")
	told := 0
	was := Changed
	Changed = func() { told++ }
	t.Cleanup(func() { Changed = was; labsTried.Store(false) })

	again := func() {
		t.Helper()
		if err := Sync(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	again()
	if told != 1 {
		t.Fatalf("a first fetch told the agents %d times", told)
	}
	key := CacheKey()
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(CachePath(), old, old); err != nil {
		t.Fatal(err)
	}
	if !Stale() {
		t.Fatal("a catalog two days old isn't stale")
	}
	again()
	if told != 1 {
		t.Error("the same catalog fetched again told the agents")
	}
	if got := CacheKey(); got != key {
		t.Error("the same catalog fetched again changed its key")
	}
	if Stale() {
		t.Error("the same catalog fetched again stayed stale: its time wasn't now")
	}

	mu.Lock()
	labs = `{"openai/gpt-6.1-sol":{"id":"gpt-6.1-sol","limit":{"context":800000}}}`
	mu.Unlock()
	again()
	if told != 2 {
		t.Error("a lab catalog that changed wasn't told")
	}
	if got := CacheKey(); got == key {
		t.Error("a lab catalog that changed kept the key")
	}
	key = CacheKey()

	mu.Lock()
	api = `{"openai":{"id":"openai","models":{"gpt-6.1-sol":{"id":"gpt-6.1-sol","cost":{"input":3,"output":10}}}}}`
	mu.Unlock()
	again()
	if told != 3 {
		t.Error("a catalog that changed wasn't told")
	}
	if got := CacheKey(); got == key {
		t.Error("a catalog that changed kept the key")
	}
	if pr, ok := PriceOf("openai", "gpt-6.1-sol"); !ok || pr.Input != 3 {
		t.Errorf("a catalog that changed wasn't read again: %+v %v", pr, ok)
	}
}

// Two writes of the same length inside one tick of a coarse filesystem
// clock (Linux / WSL2 stamps files from a clock a few milliseconds coarse)
// leave the same size and mtime: CacheKey must still hash the new bytes
// rather than keeping the first write's key, for both models.json and
// labs.json (#6572).
func TestCacheKeySameSizeSameTick(t *testing.T) {
	freshCache(t)
	check := func(name, p, a, b string) {
		t.Helper()
		if len(a) != len(b) {
			t.Fatalf("%s: fixture sizes differ (%d vs %d)", name, len(a), len(b))
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(a), 0o644); err != nil {
			t.Fatal(err)
		}
		st, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		k1 := CacheKey()
		if got := CacheKey(); got != k1 {
			t.Fatalf("%s: unchanged fresh file moved its key", name)
		}
		if err := os.WriteFile(p, []byte(b), 0o644); err != nil {
			t.Fatal(err)
		}
		// pin the second write to the first write's mtime to reproduce a
		// coarse clock tick deterministically on every kernel/filesystem
		if err := os.Chtimes(p, st.ModTime(), st.ModTime()); err != nil {
			t.Fatal(err)
		}
		k2 := CacheKey()
		if k2 == k1 {
			t.Fatalf("%s: same-size write in the same mtime tick kept the stale key %q", name, k1)
		}
	}
	check("catalog", CachePath(),
		`{"openai":{"id":"openai","models":{"gpt-6.1-sol":{"id":"gpt-6.1-sol","cost":{"input":2,"output":10}}}}}`,
		`{"openai":{"id":"openai","models":{"gpt-6.1-sol":{"id":"gpt-6.1-sol","cost":{"input":3,"output":10}}}}}`,
	)
	check("labs", LabsPath(),
		`{"openai/gpt-6.1-sol":{"id":"gpt-6.1-sol","limit":{"context":400000}}}`,
		`{"openai/gpt-6.1-sol":{"id":"gpt-6.1-sol","limit":{"context":800000}}}`,
	)
}

// Once a file's mtime is old enough to have settled past any coarse clock
// tick, fileKey caches its hash by size and mtime; a fresh or future-dated
// file is not cached, and rewriting or removing a settled file drops its
// cached entry. The fileKeys checks below are intentional white-box
// assertions (matching filememo's reread_test.go) paired with black-box
// CacheKey checks.
func TestCacheKeySettledCaching(t *testing.T) {
	freshCache(t)
	p := CachePath()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	const a = `{"openai":{"id":"openai","models":{"gpt-6.1-sol":{"id":"gpt-6.1-sol","cost":{"input":2,"output":10}}}}}`
	const b = `{"openai":{"id":"openai","models":{"gpt-6.1-sol":{"id":"gpt-6.1-sol","cost":{"input":3,"output":10}}}}}`
	if err := os.WriteFile(p, []byte(a), 0o644); err != nil {
		t.Fatal(err)
	}

	// fresh file: not cached
	_ = CacheKey()
	fileKeys.Lock()
	_, cachedFresh := fileKeys.m[p]
	fileKeys.Unlock()
	if cachedFresh {
		t.Fatal("fresh (unsettled) file was cached by stat")
	}

	// future-dated file: not cached, and same-size overwrite at same mtime is seen
	future := time.Now().Add(time.Minute)
	if err := os.Chtimes(p, future, future); err != nil {
		t.Fatal(err)
	}
	kf1 := CacheKey()
	fileKeys.Lock()
	_, cachedFuture := fileKeys.m[p]
	fileKeys.Unlock()
	if cachedFuture {
		t.Fatal("future-dated file was cached by stat")
	}
	if err := os.WriteFile(p, []byte(b), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, future, future); err != nil {
		t.Fatal(err)
	}
	if kf2 := CacheKey(); kf2 == kf1 {
		t.Fatal("future-dated same-size write in same mtime kept stale key")
	}

	// settled file: cached by stat (black-box proof: a synthetic overwrite
	// pinned back to the same old mtime reuses the cached hash without
	// rehashing)
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatal(err)
	}
	stOld, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	ks1 := CacheKey()
	fileKeys.Lock()
	entry, cachedSettled := fileKeys.m[p]
	fileKeys.Unlock()
	if !cachedSettled || entry.size != int64(len(b)) || !entry.mod.Equal(stOld.ModTime()) {
		t.Fatalf("settled file was not cached properly: cached=%v entry=%+v", cachedSettled, entry)
	}
	if err := os.WriteFile(p, []byte(a), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, stOld.ModTime(), stOld.ModTime()); err != nil {
		t.Fatal(err)
	}
	if ks2 := CacheKey(); ks2 != ks1 {
		t.Fatalf("settled file did not reuse its stat cache: %q vs %q", ks1, ks2)
	}

	// rewriting the settled file drops the stale entry and sees the new bytes
	if err := os.WriteFile(p, []byte(a), 0o644); err != nil {
		t.Fatal(err)
	}
	ks3 := CacheKey()
	if ks3 == ks1 {
		t.Fatal("rewritten catalog kept settled file's key")
	}
	fileKeys.Lock()
	_, cachedAfterRewrite := fileKeys.m[p]
	fileKeys.Unlock()
	if cachedAfterRewrite {
		t.Fatal("stale cache entry remained after fresh rewrite")
	}

	// removing a settled file returns "missing" and clears its cached entry
	if err := os.Chtimes(p, stOld.ModTime(), stOld.ModTime()); err != nil {
		t.Fatal(err)
	}
	_ = CacheKey()
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if got := CacheKey(); got != "missing|missing" {
		t.Fatalf("removed catalog key = %q, want missing|missing", got)
	}
	fileKeys.Lock()
	_, cachedAfterRemove := fileKeys.m[p]
	fileKeys.Unlock()
	if cachedAfterRemove {
		t.Fatal("stale cache entry remained after file removal")
	}
}
