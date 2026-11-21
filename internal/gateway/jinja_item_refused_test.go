package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/yetone/magpie/internal/provider"
)

// llamaServer answers as llama.cpp's Anthropic-compatible server answers
// Claude Code through magpie (yetone/magpie#7383): a tool_reference block
// in a tool_result's content is a 500 "Jinja Exception: Unexpected item
// type in content." (its Qwen template takes text, image and video only),
// while the same result with the reference as text is a 200.
type llamaServer struct {
	mu     sync.Mutex
	bodies []string
}

const llamaJinjaRefused = `{"type":"error","error":{"type":"api_error","message":" While executing CallExpression at line 33, column 36 in source: ...raise_exception('Unexpected item type in content.')... ^ Error: Jinja Exception: Unexpected item type in content."}}`

func (v *llamaServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	v.mu.Lock()
	v.bodies = append(v.bodies, string(b))
	v.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if hasResultRefs(b) {
		w.WriteHeader(500)
		io.WriteString(w, llamaJinjaRefused)
		return
	}
	if strings.HasSuffix(r.URL.Path, "/count_tokens") {
		io.WriteString(w, `{"input_tokens":12}`)
		return
	}
	io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","model":"qwen","content":[{"type":"text","text":"OK"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
}

func (v *llamaServer) sent() []string {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]string(nil), v.bodies...)
}

// A local server that turns a tool_reference in a tool_result's content
// away with llama.cpp's 500 Jinja Exception is asked again at once with
// each reference as the text a translated route tells a model, and the
// shape is learned: the next turn goes as text from the first ask.
func TestJinjaItemRefusedGoesAsText(t *testing.T) {
	fresh(t)
	ls := &llamaServer{}
	srv := httptest.NewServer(ls)
	t.Cleanup(srv.Close)
	if err := provider.Save(provider.Provider{ID: "local", Name: "Local", Anthropic: srv.URL, Models: []string{"qwen3.6"}}); err != nil {
		t.Fatal(err)
	}
	gw := New()
	post := func(body string) int {
		t.Helper()
		rec := httptest.NewRecorder()
		gw.Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body)))
		return rec.Code
	}
	if code := post(toolSearchTurn("local/qwen3.6")); code != 200 {
		t.Fatalf("status %d, want the turn retried as text and answered: %s", code, ls.sent())
	}
	sent := ls.sent()
	if len(sent) != 2 || !hasResultRefs([]byte(sent[0])) {
		t.Fatalf("asked %d times, want the references once and then text: %q", len(sent), sent)
	}
	if hasResultRefs([]byte(sent[1])) {
		t.Fatalf("a tool_reference was sent again: %s", sent[1])
	}
	if !strings.Contains(sent[1], "Tool WebFetch is loaded and can be called now.") {
		t.Fatalf("the reference did not go as text: %s", sent[1])
	}
	if gw.fits("local", shapeRefs, provider.Anthropic) {
		t.Fatal("the shape was not learned")
	}
	// the next turn is sent as text from the first ask
	if code := post(toolSearchTurn("local/qwen3.6")); code != 200 {
		t.Fatalf("next turn: status %d", code)
	}
	if sent = ls.sent(); len(sent) != 3 || hasResultRefs([]byte(sent[2])) {
		t.Fatalf("the learned turn held the references: %q", sent[2:])
	}
}
