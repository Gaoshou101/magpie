package gui

import (
	"context"
	"log"
	"net/http"
	"runtime"
	"runtime/debug"
)

// windowSchemeKey carries the native transport scheme, which Wails removes
// from r.URL before handing the request to us. Network headers cannot set it.
type windowSchemeKey struct{}

type windowWriter struct {
	http.ResponseWriter
	wrote bool
}

func (w *windowWriter) WriteHeader(code int) {
	w.wrote = true
	w.ResponseWriter.WriteHeader(code)
}

func (w *windowWriter) Write(b []byte) (int, error) {
	w.wrote = true
	return w.ResponseWriter.Write(b)
}

func (w *windowWriter) Flush() {
	w.wrote = true
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *windowWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// ownWindow serves the app's own windows. Wails hands their requests over
// with no RemoteAddr and fills in 192.0.2.1:1234 (TEST-NET-1), so the
// allow list took the desktop window for a computer at 192.0.2.1 and
// asked for that address to be added (Mathresearcher on Discord). Only
// the webview reaches this handler, never the network, so its requests
// come from this computer: loopback. Wails runs each request in its own
// goroutine without net/http's panic recovery, so a panic here must be
// caught rather than taking the gateway process down with it (#7045).
func ownWindow(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ww := &windowWriter{ResponseWriter: w}
		defer func() {
			if p := recover(); p != nil {
				if p != http.ErrAbortHandler {
					log.Printf("gui: %s %s panicked: %v\n%s", r.Method, r.URL.Path, p, debug.Stack())
					if !ww.wrote {
						http.Error(ww, "internal error", http.StatusInternalServerError)
					}
				}
			}
		}()
		r.RemoteAddr = "127.0.0.1:0"
		scheme := "wails"
		if runtime.GOOS == "windows" {
			scheme = "http"
		}
		r = r.WithContext(context.WithValue(r.Context(), windowSchemeKey{}, scheme))
		next.ServeHTTP(ww, r)
	})
}
