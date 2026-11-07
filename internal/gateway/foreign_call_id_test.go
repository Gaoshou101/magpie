package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/yetone/magpie/internal/provider"
)

// aliCallConversation is Eleef's conversation after the routing group fell
// back to OpenAI (#5609): the user's ask, a call an Ali/Kimi turn made
// under an id of a message's kind, its output, and the next ask.
const aliCallConversation = `{"type":"message","role":"user","content":[{"type":"input_text","text":"Continue after the tool call."}]},
  {"type":"function_call","id":"msg_00000000-0000-0000-0000-000000000000","call_id":"call_00000000","name":"get_weather","arguments":"{\"city\":\"Paris\"}","status":"completed"},
  {"type":"function_call_output","call_id":"call_00000000","output":"{\"weather\":\"sunny\"}"},
  {"type":"message","id":"msg_kept","role":"assistant","content":[{"type":"output_text","text":"It is sunny."}]}`

// fcChecker plays OpenAI's /responses: a function_call whose id is not
// fc_… is refused word for word as Eleef got it; anything else goes on to
// idChecker.
type fcChecker struct {
	mu sync.Mutex
	n  int
	idChecker
}

func (u *fcChecker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	u.mu.Lock()
	u.n++
	u.mu.Unlock()
	var q struct {
		Input []map[string]any `json:"input"`
	}
	json.Unmarshal(body, &q)
	for i, it := range q.Input {
		id, ok := it["id"].(string)
		if it["type"] == "function_call" && ok && !strings.HasPrefix(id, "fc") {
			u.idChecker.mu.Lock()
			u.idChecker.inputs = append(u.idChecker.inputs, q.Input)
			u.idChecker.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"error":{"message":"Invalid 'input[`+strconv.Itoa(i)+`].id': '`+id+`'. Expected an ID that begins with 'fc'.","type":"invalid_request_error","param":"input[`+strconv.Itoa(i)+`].id","code":"invalid_value"}}`)
			return
		}
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	u.idChecker.ServeHTTP(w, r)
}

// checkAliCallSent fails unless input is aliCallConversation as OpenAI
// takes it: the call without its foreign id, still paired with its output
// by call_id, and every other item as it was.
func checkAliCallSent(t *testing.T, input []map[string]any) {
	t.Helper()
	var types []string
	for _, it := range input {
		types = append(types, it["type"].(string))
		if it["type"] == "function_call" {
			if _, ok := it["id"]; ok {
				t.Errorf("function_call sent with its foreign id: %v", it)
			}
			if it["call_id"] != "call_00000000" || it["name"] != "get_weather" ||
				it["arguments"] != `{"city":"Paris"}` || it["status"] != "completed" {
				t.Errorf("function_call sent as %v", it)
			}
		}
		if it["type"] == "function_call_output" && it["call_id"] != "call_00000000" {
			t.Errorf("output no longer paired with its call: %v", it)
		}
	}
	if got := strings.Join(types, ","); !strings.HasPrefix(got, "message,function_call,function_call_output,message") {
		t.Errorf("items sent: %s", got)
	}
	b, _ := json.Marshal(input)
	if !strings.Contains(string(b), `"msg_kept"`) {
		t.Errorf("an assistant message's own id not kept: %s", b)
	}
}

func TestForeignFunctionCallIDsLeftOut(t *testing.T) {
	body := []byte(`{"model":"gpt-6.1","input":[` + aliCallConversation + `,
	  {"type":"function_call","id":"fc_ours","call_id":"call_1","name":"shell","arguments":"{}"},
	  {"type":"custom_tool_call","id":"msg_other","call_id":"call_2","name":"apply_patch","input":"x"}]}`)
	var q struct {
		Input []map[string]any `json:"input"`
	}
	if err := json.Unmarshal(foreignFunctionCallIDs(body), &q); err != nil {
		t.Fatal(err)
	}
	checkAliCallSent(t, q.Input[:4])
	if q.Input[4]["id"] != "fc_ours" {
		t.Errorf("an fc_ id not kept: %v", q.Input[4])
	}
	if q.Input[5]["id"] != "msg_other" {
		t.Errorf("another kind of call's id changed (callItemIDs' to bind): %v", q.Input[5])
	}
	// a request with nothing to change goes byte for byte
	for _, s := range []string{
		`{"input":[{"type":"function_call","id":"fc_1","call_id":"c","name":"n","arguments":"{}"}]}`,
		`{"input":[{"type":"function_call","call_id":"c","name":"n","arguments":"{}"}]}`,
		`{"input":[{"type":"message","id":"msg_1","role":"user","content":"function_call"}]}`,
		``, `null`, `{"input":null}`, `{`,
	} {
		if got := foreignFunctionCallIDs([]byte(s)); !bytes.Equal(got, []byte(s)) {
			t.Errorf("%s changed to %s", s, got)
		}
	}
	// nor is a long session of fc_ calls decoded to find nothing (#1532)
	session := codexSession(60, func(int, map[string]any) {})
	if got := foreignFunctionCallIDs(session); !bytes.Equal(got, session) {
		t.Fatal("a session of fc_ calls changed")
	}
	if a := testing.AllocsPerRun(5, func() { foreignFunctionCallIDs(session) }); a > 10 {
		t.Errorf("%v allocations for a session it leaves as it came, want it not decoded", a)
	}
}

// Eleef (#5609): a routing group's Ali/Kimi turn gave Codex a call under
// a msg_ id, and when the group fell back to OpenAI every turn was turned
// away "Expected an ID that begins with 'fc'". An upstream that checks
// item ids as OpenAI does (Azure here) gets the call without that id from
// the first ask.
func TestOpenAITakesAnotherVendorsFunctionCallID(t *testing.T) {
	fresh(t)
	up := &fcChecker{idChecker: idChecker{check: true}}
	srv := httptest.NewServer(up)
	t.Cleanup(srv.Close)
	p, err := provider.FromPreset(provider.AzurePreset)
	if err != nil {
		t.Fatal(err)
	}
	p.Key = "azure-key"
	p.Chat = srv.URL + "/openai/v1"
	p.Responses = p.Chat
	p.Models = []string{"gpt-6.1"}
	if err := provider.Save(p); err != nil {
		t.Fatal(err)
	}
	code, body := codexPost(t, `{"model":"azure/gpt-6.1","stream":true,"store":false,"input":[`+aliCallConversation+`]}`)
	if code != 200 || !strings.Contains(body, "DONE") {
		t.Fatalf("%d %s", code, body)
	}
	asked := up.asked()
	if up.n != 1 || len(asked) != 1 {
		t.Fatalf("asked %d times", up.n)
	}
	checkAliCallSent(t, asked[0])
}

// Back on one of Codex's own models, on the ChatGPT backend, which checks
// a function_call's id as OpenAI does.
func TestCodexOwnModelTakesAnotherVendorsFunctionCallID(t *testing.T) {
	setup(t, provider.Chat, &fake{t: t})
	up := &fcChecker{idChecker: idChecker{check: true}}
	chatgpt(t, up.ServeHTTP)
	code, body := codexPost(t, `{"model":"gpt-6-luna","stream":true,"store":false,"input":[`+aliCallConversation+`]}`)
	if code != 200 || !strings.Contains(body, "DONE") {
		t.Fatalf("%d %s", code, body)
	}
	asked := up.asked()
	if len(asked) != 1 {
		t.Fatalf("asked the backend %d times", len(asked))
	}
	checkAliCallSent(t, asked[0])
}

// A relay in front of OpenAI, which magpie can't tell from any other
// upstream, refuses the call's id as OpenAI does: asked once more without
// it, and from then on asked that way at once.
func TestRelayRefusingFunctionCallIDAskedAgain(t *testing.T) {
	fresh(t)
	up := &fcChecker{idChecker: idChecker{check: true}}
	srv := httptest.NewServer(up)
	t.Cleanup(srv.Close)
	if err := provider.Save(provider.Provider{ID: "relay", Name: "Relay", Key: "k", Models: []string{"gpt-6.1"}, Responses: srv.URL + "/v1"}); err != nil {
		t.Fatal(err)
	}
	s := New()
	for turn := range 2 {
		rec := codexPostTo(s, `{"model":"relay/gpt-6.1","stream":true,"store":false,"input":[`+aliCallConversation+`]}`)
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), "DONE") {
			t.Fatalf("turn %d: %d %s", turn, rec.Code, rec.Body.String())
		}
	}
	asked := up.asked()
	if up.n != 3 || len(asked) != 3 {
		t.Fatalf("asked the relay %d times, want 2 for the first turn and 1 for the second", up.n)
	}
	checkAliCallSent(t, asked[1])
	checkAliCallSent(t, asked[2])
}

// A provider that doesn't check item ids (the Ali/Kimi one that made the
// call) gets it as Codex sent it, its msg_ id and all.
func TestUncheckingProviderKeepsFunctionCallID(t *testing.T) {
	fresh(t)
	up := &idChecker{}
	srv := httptest.NewServer(up)
	t.Cleanup(srv.Close)
	if err := provider.Save(provider.Provider{ID: "ali", Name: "Ali", Key: "k", Models: []string{"kimi-k3.1-pro"}, Responses: srv.URL + "/v1"}); err != nil {
		t.Fatal(err)
	}
	rec := codexPostTo(New(), `{"model":"ali/kimi-k3.1-pro","stream":true,"store":false,"input":[`+aliCallConversation+`]}`)
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	asked := up.asked()
	if len(asked) != 1 {
		t.Fatalf("asked %d times", len(asked))
	}
	call := asked[0][1]
	if call["type"] != "function_call" || call["id"] != "msg_00000000-0000-0000-0000-000000000000" ||
		call["call_id"] != "call_00000000" || call["arguments"] != `{"city":"Paris"}` || call["status"] != "completed" {
		t.Errorf("call sent as %v, want it as Codex sent it", call)
	}
}
