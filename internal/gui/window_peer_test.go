package gui

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestOwnWindowRecoversHandlerPanic(t *testing.T) {
	var flushed bool
	h := ownWindow(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/stream":
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
				flushed = true
			}
		case "/abort":
			panic(http.ErrAbortHandler)
		case "/partial":
			w.WriteHeader(http.StatusAccepted)
			panic("panic after header")
		default:
			panic("boom on webview goroutine")
		}
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/state", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}

	recAbort := httptest.NewRecorder()
	h.ServeHTTP(recAbort, httptest.NewRequest(http.MethodGet, "/abort", nil))
	if recAbort.Code == http.StatusInternalServerError {
		t.Fatalf("ErrAbortHandler wrote status %d", recAbort.Code)
	}

	recPartial := httptest.NewRecorder()
	h.ServeHTTP(recPartial, httptest.NewRequest(http.MethodGet, "/partial", nil))
	if recPartial.Code != http.StatusAccepted {
		t.Fatalf("partial status = %d, want 202", recPartial.Code)
	}

	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/stream", nil))
	if !flushed {
		t.Fatal("ownWindow response writer did not preserve http.Flusher")
	}
}
