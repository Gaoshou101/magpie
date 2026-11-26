package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/yetone/magpie/internal/provider"
)

// deferredVendor answers as Kimi Code's and DeepSeek's Anthropic endpoints
// answered the real requests (10-14): Kimi refuses any message holding a
// tool_addition, a tool_removal or a tool_reference with a 400 that names
// nothing; DeepSeek takes a tool_addition naming a tool, and refuses one
// defining it, a tool_removal and a message's tool_reference with a 422.
// Both take a tool_reference inside a tool_result.
type deferredVendor struct {
	deepseek bool
	mu       sync.Mutex
	bodies   []string
}

func (v *deferredVendor) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	v.mu.Lock()
	v.bodies = append(v.bodies, string(b))
	v.mu.Unlock()
	var m struct {
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	json.Unmarshal(b, &m)
	refused := ""
	for _, msg := range m.Messages {
		var blocks []struct {
			Type string `json:"type"`
			Tool struct {
				Type string `json:"type"`
			} `json:"tool"`
		}
		json.Unmarshal(msg.Content, &blocks)
		for _, bl := range blocks {
			switch {
			case bl.Type == "tool_addition" && v.deepseek && bl.Tool.Type == "tool_definition":
				refused = "tool_definition"
			case bl.Type == "tool_addition" && v.deepseek:
			case bl.Type == "tool_addition", bl.Type == "tool_removal", bl.Type == "tool_reference":
				refused = bl.Type
			}
		}
	}
	w.Header().Set("Content-Type", "application/json")
	switch {
	case refused != "" && v.deepseek:
		w.WriteHeader(422)
		io.WriteString(w, `{"error":{"message":"Failed to deserialize the JSON body into the target type: messages[1].content: unknown variant `+"`"+refused+"`"+`, expected one of `+"`text`, `image`, `document`, `thinking`, `server_tool_use`, `tool_use`, `tool_result`, `web_search_tool_result`, `tool_addition`"+` at line 1 column 233 (request_id: 160c0348-740b-41e4-8a7f-6f4dc596c741)","type":"invalid_request_error","param":null,"code":"invalid_request_error"}}`)
	case refused != "":
		w.WriteHeader(400)
		io.WriteString(w, `{"error":{"type":"invalid_request_error","message":"Invalid request Error"},"type":"error"}`)
	default:
		io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	}
}

func (v *deferredVendor) sent() []string {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]string(nil), v.bodies...)
}

// Claude Code's deferred-tool blocks, as 2.1.296 builds them and as the
// reporter captured them.
const (
	ccAddRef = `{"type":"tool_addition","tool":{"type":"tool_reference","name":"Read"}}`
	ccAddDef = `{"type":"tool_addition","tool":{"type":"tool_definition","definition":{"name":"Read","description":"read a file","input_schema":{"type":"object"}}}}`
	ccRemove = `{"type":"tool_removal","tool":{"type":"tool_reference","name":"Read"}}`
	// the ToolSearch answer: stays wherever it goes
	ccSearchResult = `{"role":"assistant","content":[{"type":"tool_use","id":"toolu_01","name":"ToolSearch","input":{"query":"select:Read"}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_01","content":[{"type":"tool_reference","tool_name":"Read"}]}]}`
)

func deferredBody(model, block string) string {
	return `{"model":"` + model + `","max_tokens":32,"stream":false,"tools":[{"name":"Read","description":"read a file","input_schema":{"type":"object"}}],"messages":[` +
		`{"role":"user","content":"hi"},` + ccSearchResult + `,` +
		`{"role":"system","content":[{"type":"text","text":"t"},` + block + `]},` +
		`{"role":"system","content":[` + block + `]},` +
		`{"role":"user","content":"ok?"}]}`
}

// #2042 (davisjiahao): once Claude Code's ToolSearch put a tool_addition
// in the conversation, every request to Kimi Code's Anthropic endpoint was
// 400 "Invalid request Error". Kimi is sent no deferred-tool block, a
// message left empty goes, and the rest (the system text, the ToolSearch
// answer's tool_reference) stays.
func TestKimiIsSentNoDeferredToolBlocks(t *testing.T) {
	fresh(t)
	up := &deferredVendor{}
	srv := httptest.NewServer(up)
	t.Cleanup(srv.Close)
	if err := provider.Save(provider.Provider{ID: "kimi-code", Name: "Kimi Code", Preset: "kimi-code", Key: "k", Anthropic: srv.URL,
		Models: []string{"k3-256k"}}); err != nil {
		t.Fatal(err)
	}
	h := New().Handler()
	reporter := `{"model":"kimi-code/k3-256k","max_tokens":32,"messages":[{"role":"user","content":"hi"},` +
		`{"role":"system","content":[{"type":"text","text":"t"},{"tool":"Read","type":"tool_addition"}]},` +
		`{"role":"user","content":"ok?"}],"thinking":{"type":"disabled"}}`
	cases := map[string]string{
		"reporter's bytes":         reporter,
		"addition by name":         deferredBody("kimi-code/k3-256k", ccAddRef),
		"addition defined":         deferredBody("kimi-code/k3-256k", ccAddDef),
		"removal":                  deferredBody("kimi-code/k3-256k", ccRemove),
		"message's tool_reference": deferredBody("kimi-code/k3-256k", `{"type":"tool_reference","tool_name":"Read"}`),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			n := len(up.sent())
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body)))
			if rec.Code != 200 {
				t.Fatalf("status %d: %s", rec.Code, rec.Body)
			}
			sent := up.sent()[n:]
			if len(sent) != 1 {
				t.Fatalf("Kimi was asked %d times, want once", len(sent))
			}
			got := sent[0]
			for _, b := range []string{`"tool_addition"`, `"tool_removal"`} {
				if strings.Contains(got, b) {
					t.Errorf("Kimi was sent %s: %s", b, got)
				}
			}
			if !strings.Contains(got, `"text":"t"`) || !strings.Contains(got, `"text":"ok?"`) && !strings.Contains(got, `"content":"ok?"`) {
				t.Errorf("the conversation's text was lost: %s", got)
			}
			if name != "reporter's bytes" && !strings.Contains(got, `"tool_name":"Read"}]}`) {
				t.Errorf("the ToolSearch answer's tool_reference was dropped: %s", got)
			}
			var m struct {
				Messages []json.RawMessage `json:"messages"`
			}
			json.Unmarshal([]byte(got), &m)
			for _, x := range m.Messages {
				if string(x) == `{"role":"system","content":[]}` || strings.Contains(string(x), `"content":[]`) {
					t.Errorf("a message left empty was sent: %s", x)
				}
			}
		})
	}
}

// DeepSeek's Anthropic endpoint takes a tool_addition naming a tool but
// refuses a tool_removal (422 "unknown variant `tool_removal`"): it is
// asked again at once without the blocks, and from then on sent none. A
// provider that takes them is sent them as Claude Code wrote them.
func TestDeferredToolBlocksRefusedOnceLeftOut(t *testing.T) {
	fresh(t)
	ds := &deferredVendor{deepseek: true}
	dsSrv := httptest.NewServer(ds)
	t.Cleanup(dsSrv.Close)
	takes := &deferredVendor{deepseek: true}
	tkSrv := httptest.NewServer(takes)
	t.Cleanup(tkSrv.Close)
	if err := provider.Save(provider.Provider{ID: "deepseek", Name: "DeepSeek", Preset: "deepseek", Key: "k", Anthropic: dsSrv.URL,
		Models: []string{"deepseek-chat"}}); err != nil {
		t.Fatal(err)
	}
	if err := provider.Save(provider.Provider{ID: "other", Name: "Other", Key: "k", Anthropic: tkSrv.URL,
		Models: []string{"deepseek-chat"}}); err != nil {
		t.Fatal(err)
	}
	h := New().Handler()
	post := func(body string) {
		t.Helper()
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body)))
		if rec.Code != 200 {
			t.Fatalf("status %d: %s", rec.Code, rec.Body)
		}
	}
	post(deferredBody("deepseek/deepseek-chat", ccRemove))
	sent := ds.sent()
	if len(sent) != 2 || !strings.Contains(sent[0], `"tool_removal"`) || strings.Contains(sent[1], `"tool_removal"`) {
		t.Fatalf("DeepSeek was asked %d times, want the removal once and then without it: %q", len(sent), sent)
	}
	// learned: the next request goes without them from the start, an
	// addition DeepSeek would take included
	post(deferredBody("deepseek/deepseek-chat", ccAddRef))
	if sent = ds.sent(); len(sent) != 3 || strings.Contains(sent[2], `"tool_addition"`) {
		t.Fatalf("after the refusal DeepSeek was sent %q", sent[2:])
	}
	// a provider that took them keeps getting them
	post(deferredBody("other/deepseek-chat", ccAddRef))
	if got := takes.sent(); len(got) != 1 || !strings.Contains(got[0], `"tool_addition"`) {
		t.Fatalf("a provider that takes tool_addition was sent %q", got)
	}
}

// resultDefVendor answers as an Anthropic endpoint that takes Claude
// Code's deferred-tool blocks but refuses the definition a newer
// ToolSearch answer carries in its tool_result (#5404, api.deepseek.com's
// answer of 2026-11-05), and a tool_reference naming a tool the request's
// tools list lacks.
type resultDefVendor struct {
	mu     sync.Mutex
	bodies []string
}

func (v *resultDefVendor) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	v.mu.Lock()
	v.bodies = append(v.bodies, string(b))
	v.mu.Unlock()
	var m struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
		Messages []struct {
			Content []struct {
				Type    string `json:"type"`
				Content []struct {
					Type     string `json:"type"`
					ToolName string `json:"tool_name"`
				} `json:"content"`
			} `json:"content"`
		} `json:"messages"`
	}
	json.Unmarshal(b, &m)
	declared := map[string]bool{}
	for _, t := range m.Tools {
		declared[t.Name] = true
	}
	refused := ""
	for _, msg := range m.Messages {
		for _, bl := range msg.Content {
			if bl.Type != "tool_result" {
				continue
			}
			for _, it := range bl.Content {
				switch {
				case it.Type == "tool_definition":
					refused = "unknown variant `tool_definition`, expected `tool_reference`"
				case it.Type == "tool_reference" && !declared[it.ToolName]:
					refused = "tool reference not found in available tools"
				}
			}
		}
	}
	w.Header().Set("Content-Type", "application/json")
	if refused != "" {
		w.WriteHeader(422)
		io.WriteString(w, `{"error":{"message":"Failed to deserialize the JSON body into the target type: messages[1].content: `+refused+` at line 1 column 473","type":"invalid_request_error"}}`)
		return
	}
	io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
}

func (v *resultDefVendor) sent() []string {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]string(nil), v.bodies...)
}

// nestedDefBody is a conversation whose ToolSearch answer carries the
// loaded tool's definition itself in its tool_result, as newer Claude Code
// builds send it (#5404), with no deferred-tool block at the top level of
// any message.
const nestedDefBody = `{"model":"relay/deepseek-chat","max_tokens":32,"stream":false,` +
	`"tools":[{"name":"ToolSearch","description":"find tools","input_schema":{"type":"object"}}],` +
	`"messages":[` +
	`{"role":"user","content":"find a reader"},` +
	`{"role":"assistant","content":[{"type":"tool_use","id":"toolu_01","name":"ToolSearch","input":{"query":"read"}}]},` +
	`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_01","content":[{"type":"tool_definition","definition":{"name":"Read","description":"read a file","input_schema":{"type":"object"}}}]}]},` +
	`{"role":"user","content":"go"}` + `]}`

// #5404: a ToolSearch answer's tool_definition survives a learned retry.
// The endpoint refuses it with 422 "unknown variant `tool_definition`,
// expected `tool_reference`"; the learned retry stripped the top-level
// blocks alone (none here), so the same variant failed again. Now the
// definition joins the tools and the answer goes as the reference the
// endpoint takes — the reference only once the tool is declared, which is
// what the hoisting is for.
func TestToolResultDefinitionRefusedOnceBecomesReference(t *testing.T) {
	fresh(t)
	up := &resultDefVendor{}
	srv := httptest.NewServer(up)
	t.Cleanup(srv.Close)
	if err := provider.Save(provider.Provider{ID: "relay", Name: "Relay", Key: "k", Anthropic: srv.URL,
		Models: []string{"deepseek-chat"}}); err != nil {
		t.Fatal(err)
	}
	h := New().Handler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(nestedDefBody)))
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	sent := up.sent()
	if len(sent) != 2 {
		t.Fatalf("the relay was asked %d times, want the refusal once and then a body it takes: %d", len(sent), len(sent))
	}
	if !strings.Contains(sent[0], `"tool_definition"`) {
		t.Fatalf("the first ask came already reshaped: %s", sent[0])
	}
	if strings.Contains(sent[1], `"tool_definition"`) {
		t.Fatalf("the retry still carried the definition: %s", sent[1])
	}
	if !strings.Contains(sent[1], `"tool_name":"Read"`) {
		t.Fatalf("the retry lost the ToolSearch answer's reference: %s", sent[1])
	}
	if !strings.Contains(sent[1], `"name":"Read","description":"read a file"`) {
		t.Fatalf("the definition did not join the retry's tools: %s", sent[1])
	}
	if !strings.Contains(sent[1], `"tool_use_id":"toolu_01"`) {
		t.Fatalf("the retry lost the answer's pairing: %s", sent[1])
	}
}

// The reshaping, on the bytes: a definition in a tool_result joins the
// tools once, the block goes as the named reference, the rest of the
// result (a reference already there, text) stays; a body with only the
// nested definition is still one that gets worked, and one whose result
// can't be read goes as it came.
func TestNestedToolResultDefinitionBecomesReference(t *testing.T) {
	body := `{"model":"m","max_tokens":8,"stream":false,` +
		`"tools":[{"name":"ToolSearch","description":"find tools","input_schema":{"type":"object"}},{"name":"Read","description":"read a file","input_schema":{"type":"object"}}],` +
		`"messages":[` +
		`{"role":"user","content":"find readers"},` +
		`{"role":"assistant","content":[{"type":"tool_use","id":"toolu_01","name":"ToolSearch","input":{"query":"read"}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_01","content":[{"type":"tool_reference","tool_name":"Read"},{"type":"tool_definition","definition":{"name":"Grep","description":"search files","input_schema":{"type":"object"}}},{"type":"text","text":"2 tools found"}]}]},` +
		`{"role":"assistant","content":[{"type":"tool_use","id":"toolu_02","name":"ToolSearch","input":{"query":"grep"}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_02","content":[{"type":"tool_definition","definition":{"name":"Grep","description":"search files","input_schema":{"type":"object"}}}]}]},` +
		`{"role":"user","content":"go"}]}`
	if !hasDeferredTools([]byte(body)) {
		t.Fatal("a nested tool_definition was not seen as a deferred tool")
	}
	out := string(withoutDeferredTools([]byte(body)))
	if strings.Contains(out, `"tool_definition"`) {
		t.Fatalf("the body still carried a tool_definition: %s", out)
	}
	for _, want := range []string{`"tool_name":"Read"`, `"tool_name":"Grep"`, `"text":"2 tools found"`, `"tool_use_id":"toolu_01"`, `"tool_use_id":"toolu_02"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("the reshaped body lost %s: %s", want, out)
		}
	}
	if n := strings.Count(out, `"name":"Grep","description":"search files"`); n != 1 {
		t.Fatalf("Grep's definition joined the tools %d times, want once: %s", n, out)
	}
	if n := strings.Count(out, `"name":"Read","description":"read a file"`); n != 1 {
		t.Fatalf("Read's definition on the tools list %d times, want once: %s", n, out)
	}
	// a result whose content is no array of blocks goes as it came
	unreadable := `{"model":"m","max_tokens":8,"stream":false,"tools":[{"name":"ToolSearch","description":"find tools","input_schema":{"type":"object"}}],"messages":[` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_01","content":{"type":"tool_definition","definition":{"name":"Read","description":"read a file","input_schema":{"type":"object"}}}}]}]}`
	if !hasDeferredTools([]byte(unreadable)) {
		t.Fatal("a result carrying a tool_definition was not seen as a deferred tool")
	}
	out = string(withoutDeferredTools([]byte(unreadable)))
	if !strings.Contains(out, `"content":{"type":"tool_definition"`) {
		t.Fatalf("the unreadable result was reworked: %s", out)
	}
}

// isDeepSeekAnthropic is DeepSeek's own Anthropic endpoint alone, however
// the provider is built (#5404): a preset pointed elsewhere keeps the
// blocks until that endpoint refuses them.
func TestDeepSeekAnthropicRecognized(t *testing.T) {
	ds := provider.Provider{ID: "deepseek", Name: "DeepSeek", Key: "k",
		Anthropic: "https://api.deepseek.com/anthropic", Models: []string{"deepseek-chat"}}
	if !isDeepSeekAnthropic(ds, provider.Anthropic) {
		t.Fatal("DeepSeek's own endpoint was not recognized")
	}
	relay := provider.Provider{ID: "relay", Name: "Relay", Preset: "deepseek", Key: "k",
		Anthropic: "https://relay.example.invalid/anthropic", Models: []string{"deepseek-chat"}}
	if isDeepSeekAnthropic(relay, provider.Anthropic) {
		t.Fatal("another host was taken for DeepSeek's endpoint")
	}
}

// #5404: DeepSeek's own endpoint takes a tool_addition at no position
// now, so a body to it goes without the deferred-tool blocks on the first
// ask, its ToolSearch answer's definition on the tools list and the
// answer as the reference, instead of a 422 to learn from first.
func TestDeepSeekAnthropicIsStrippedUpFront(t *testing.T) {
	fresh(t)
	up := &resultDefVendor{}
	srv := httptest.NewServer(up)
	t.Cleanup(srv.Close)
	if err := provider.Save(provider.Provider{ID: "deepseek", Name: "DeepSeek", Key: "k", Anthropic: "http://api.deepseek.com/anthropic",
		Models: []string{"deepseek-chat"}}); err != nil {
		t.Fatal(err)
	}
	s := New()
	addr := strings.TrimPrefix(srv.URL, "http://")
	s.client = &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}}}
	body := strings.Replace(nestedDefBody, `"relay/deepseek-chat"`, `"deepseek/deepseek-chat"`, 1)
	body = strings.Replace(body, `{"role":"user","content":"go"}`,
		`{"role":"system","content":[{"type":"text","text":"t"},`+ccAddDef+`]},{"role":"user","content":"go"}`, 1)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body)))
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	sent := up.sent()
	if len(sent) != 1 {
		t.Fatalf("DeepSeek was asked %d times, want once: %q", len(sent), sent)
	}
	for _, b := range []string{`"tool_addition"`, `"tool_definition"`} {
		if strings.Contains(sent[0], b) {
			t.Errorf("DeepSeek was sent %s: %s", b, sent[0])
		}
	}
	if !strings.Contains(sent[0], `"tool_name":"Read"`) || !strings.Contains(sent[0], `"name":"Read","description":"read a file"`) {
		t.Errorf("the ToolSearch answer did not go as a declared reference: %s", sent[0])
	}
}

// bailianDeferredVendor answers as Alibaba Cloud Bailian's /apps/anthropic
// endpoint does (#7776): any top-level deferred-tool block (tool_addition,
// tool_removal, or message-level tool_reference) triggers a generic 400
// "Request body format invalid" with no field name, while a tool_reference
// inside a tool_result is accepted.
type bailianDeferredVendor struct {
	mu     sync.Mutex
	bodies []string
}

func (v *bailianDeferredVendor) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	v.mu.Lock()
	v.bodies = append(v.bodies, string(b))
	v.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	var m struct {
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	json.Unmarshal(b, &m)
	for _, msg := range m.Messages {
		var blocks []struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(msg.Content, &blocks) != nil {
			continue
		}
		for _, bl := range blocks {
			if deferredBlock(bl.Type) {
				w.WriteHeader(400)
				io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":"Request body format invalid"}}`)
				return
			}
		}
	}
	if strings.HasSuffix(r.URL.Path, "/count_tokens") {
		io.WriteString(w, `{"input_tokens":12}`)
		return
	}
	io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","model":"deepseek-v4.1-flash-1017","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
}

func (v *bailianDeferredVendor) sent() []string {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]string(nil), v.bodies...)
}

// #7776 (mengliansheng): Bailian Token Plan's /apps/anthropic endpoint rejects
// tool_addition (and other top-level deferred-tool blocks) with a generic 400
// "Request body format invalid" that names no field to learn from. DashScope /
// Bailian / Qwen Cloud providers (preset or host) strip those blocks up front,
// hoisting any inline-only tool_definition to top-level tools and keeping
// ToolSearch's tool_reference inside tool_result; count_tokens strips them too.
// Unrelated Anthropic providers keep them untouched.
func TestDashScopeIsSentNoDeferredToolBlocks(t *testing.T) {
	fresh(t)
	up := &bailianDeferredVendor{}
	srv := httptest.NewServer(up)
	t.Cleanup(srv.Close)
	other := &bailianDeferredVendor{}
	otherSrv := httptest.NewServer(other)
	t.Cleanup(otherSrv.Close)

	if err := provider.Save(provider.Provider{
		ID:        "bailian-token-plan",
		Name:      "Bailian Token Plan",
		Preset:    "bailian-token-plan",
		Key:       "k",
		Anthropic: srv.URL + "/apps/anthropic",
		Models:    []string{"deepseek-v4.1-flash-1017"},
	}); err != nil {
		t.Fatal(err)
	}
	// Also test a custom provider matched only by its DashScope/Bailian host.
	if err := provider.Save(provider.Provider{
		ID:        "bailian-custom",
		Name:      "Bailian Custom",
		Key:       "k",
		Anthropic: "http://token-plan.cn-beijing.maas.aliyuncs.com/apps/anthropic",
		Models:    []string{"qwen3-coder-plus"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := provider.Save(provider.Provider{
		ID:        "other",
		Name:      "Other",
		Key:       "k",
		Anthropic: otherSrv.URL,
		Models:    []string{"claude-sonnet-4-5"},
	}); err != nil {
		t.Fatal(err)
	}

	s := New()
	srvAddr := strings.TrimPrefix(srv.URL, "http://")
	otherAddr := strings.TrimPrefix(otherSrv.URL, "http://")
	s.client = &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, network, host string) (net.Conn, error) {
		switch host {
		case "token-plan.cn-beijing.maas.aliyuncs.com:80", "token-plan.cn-beijing.maas.aliyuncs.com:443", srvAddr:
			return (&net.Dialer{}).DialContext(ctx, network, srvAddr)
		case otherAddr:
			return (&net.Dialer{}).DialContext(ctx, network, otherAddr)
		default:
			return nil, fmt.Errorf("unexpected outbound dial in test: %s", host)
		}
	}}}

	reporter := `{"model":"bailian-token-plan/deepseek-v4.1-flash-1017","max_tokens":16,"messages":[{"role":"user","content":[{"type":"text","text":"hi"},{"type":"tool_addition","tool":{"type":"tool_definition","definition":{"name":"Artifact","description":"d","input_schema":{"type":"object","properties":{}}}}}]}]}`

	cases := map[string]struct {
		path string
		body string
	}{
		"reporter repro":           {"/v1/messages", reporter},
		"reporter count_tokens":    {"/v1/messages/count_tokens", reporter},
		"custom host addition":     {"/v1/messages", deferredBody("bailian-custom/qwen3-coder-plus", ccAddDef)},
		"removal":                  {"/v1/messages", deferredBody("bailian-token-plan/deepseek-v4.1-flash-1017", ccRemove)},
		"message's tool_reference": {"/v1/messages", deferredBody("bailian-token-plan/deepseek-v4.1-flash-1017", `{"type":"tool_reference","tool_name":"Read"}`)},
	}

	h := s.Handler()
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			n := len(up.sent())
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest("POST", tc.path, strings.NewReader(tc.body)))
			if rec.Code != 200 {
				t.Fatalf("status %d: %s", rec.Code, rec.Body)
			}
			sent := up.sent()[n:]
			if len(sent) != 1 {
				t.Fatalf("Bailian was asked %d times, want once", len(sent))
			}
			got := sent[0]
			for _, b := range []string{`"tool_addition"`, `"tool_removal"`} {
				if strings.Contains(got, b) {
					t.Errorf("Bailian was sent %s: %s", b, got)
				}
			}
			var req struct {
				Tools []struct {
					Name string `json:"name"`
				} `json:"tools"`
				Messages []struct {
					Content json.RawMessage `json:"content"`
				} `json:"messages"`
			}
			if err := json.Unmarshal([]byte(got), &req); err != nil {
				t.Fatalf("unmarshal forwarded request: %v", err)
			}
			for _, msg := range req.Messages {
				var arr []json.RawMessage
				if json.Unmarshal(msg.Content, &arr) == nil && len(arr) == 0 {
					t.Errorf("a message left empty was sent: %s", got)
				}
			}
			if name == "reporter repro" || name == "reporter count_tokens" {
				hasArtifact := false
				for _, tool := range req.Tools {
					if tool.Name == "Artifact" {
						hasArtifact = true
					}
				}
				if !hasArtifact {
					t.Errorf("inline Artifact definition was not hoisted to top-level tools: %s", got)
				}
				if !strings.Contains(got, `"text":"hi"`) {
					t.Errorf("user text was lost: %s", got)
				}
			} else {
				foundToolResultRef := false
				for _, msg := range req.Messages {
					var blocks []struct {
						Type    string `json:"type"`
						Content []struct {
							Type     string `json:"type"`
							ToolName string `json:"tool_name"`
						} `json:"content"`
					}
					if json.Unmarshal(msg.Content, &blocks) != nil {
						continue
					}
					for _, b := range blocks {
						if b.Type != "tool_result" {
							continue
						}
						for _, c := range b.Content {
							if c.Type == "tool_reference" && c.ToolName == "Read" {
								foundToolResultRef = true
							}
						}
					}
				}
				if !foundToolResultRef {
					t.Errorf("ToolSearch tool_result reference was not preserved in tool_result: %s", got)
				}
			}
		})
	}

	// Non-DashScope Anthropic endpoint still receives deferred-tool blocks as sent.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(deferredBody("other/claude-sonnet-4-5", ccAddRef))))
	if rec.Code != 400 {
		t.Fatalf("unrelated provider status %d, want 400 from bailianDeferredVendor: %s", rec.Code, rec.Body)
	}
	if got := other.sent(); len(got) != 1 || !strings.Contains(got[0], `"tool_addition"`) {
		t.Fatalf("unrelated provider should receive tool_addition untouched: %q", got)
	}
}
