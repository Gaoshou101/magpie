package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tidwall/gjson"

	"github.com/yetone/magpie/internal/access"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/testenv"
)

// Claude Code's /advisor through a Claude subscription (recih on X,
// #6804, #6813): the run's Claude Code is offered magpie's tools only, so
// the advisor, Anthropic's server tool, was left out of the request and
// its instructions said to be void. magpie now runs it as it does for a
// vendor's model: it is read as the model's own advisor, its instructions
// kept as Claude Code wrote them, and an advisor magpie doesn't serve is
// still left out.
func TestBridgeRequestKeepsTheAdvisor(t *testing.T) {
	advisedSetup(t)
	r := httptest.NewRequest("POST", "/v1/messages", nil)
	req, err := bridgeRequest(r, provider.Anthropic, "Claude Code", []byte(advisorTurn("claude/claude-sonnet-5-5", "adv/adv-pro")))
	if err != nil {
		t.Fatal(err)
	}
	if req.Advisor != "adv/adv-pro" {
		t.Fatalf("the advisor is %q", req.Advisor)
	}
	if words := allText(req); !strings.Contains(words, advisorNote) || strings.Contains(words, advisorGone) {
		t.Fatalf("the advisor's instructions were changed:\n%s", words)
	}
	for _, c := range []struct{ name, advisor string }{
		{"Claude Desktop", "adv/adv-pro"},
		{"Claude Code", "nobody/x"},
	} {
		req, err := bridgeRequest(r, provider.Anthropic, c.name, []byte(advisorTurn("claude/claude-sonnet-5-5", c.advisor)))
		if err != nil {
			t.Fatal(err)
		}
		if req.Advisor != "" {
			t.Fatalf("%s, advisor %s: advised by %q", c.name, c.advisor, req.Advisor)
		}
	}
}

// allText is every text part of req's messages.
func allText(req *Request) string {
	var b strings.Builder
	for _, m := range req.Messages {
		for _, p := range m.Parts {
			if p.Kind == Text {
				b.WriteString(p.Text)
				b.WriteString("\n")
			}
		}
	}
	return b.String()
}

// The run's Claude Code calls magpie's advisor tool: magpie asks the
// advisor model, hands the advice back to the run, and tells the client
// as Anthropic's server does, a server_tool_use and an
// advisor_tool_result with the advice, never a call of a tool of its own.
// The advisor sees what the reply said before the call; a reply's fourth
// call is answered max_uses_exceeded without asking.
func TestClaudeTUIAdvisorAnsweredHere(t *testing.T) {
	u := advisedSetup(t)
	c := newTUICalls(t)
	q, err := bridgeRequest(httptest.NewRequest("POST", "/v1/messages", nil), provider.Anthropic, "Claude Code", []byte(advisorTurn("claude/claude-sonnet-5-5", "adv/adv-pro")))
	if err != nil {
		t.Fatal(err)
	}
	c.run.adviseOn(context.Background(), q, New().adviceRound)
	first := c.run.segment
	// a message's blocks are each a row of their own, by its id
	c.write(tuiRow("msg_a0", `{"type":"text","text":"Let me ask."}`, "tool_use", 4))
	var res []mcpToolResult
	for i := range advisorUses + 1 {
		id := "toolu_adv" + itoa(i)
		c.write(tuiRow("msg_a"+itoa(i), `{"type":"tool_use","id":"`+id+`","name":"mcp__magpie__advisor","input":{}}`, "tool_use", 2))
		res = append(res, c.adviceCall(id))
	}
	c.write(tuiRow("msg_end", `{"type":"text","text":"Done."}`, "end_turn", 2), tuiDone)
	text, calls, stop, _, raw := c.reply(first)
	if len(calls) != 0 || stop != "end_turn" || !strings.Contains(text, "Done.") {
		t.Fatalf("the reply %q %v %q:\n%s", text, calls, stop, raw)
	}
	for i, r := range res[:advisorUses] {
		if r.IsError || r.Content[0]["text"] != "Check the tests first." {
			t.Fatalf("call %d was answered %+v", i, r)
		}
	}
	if last := res[advisorUses]; !last.IsError || !strings.Contains(last.Content[0]["text"].(string), "max_uses_exceeded") {
		t.Fatalf("a call past the limit was answered %+v", last)
	}
	u.mu.Lock()
	asked := append([]string(nil), u.advisor...)
	u.mu.Unlock()
	if len(asked) != advisorUses || !strings.Contains(asked[0], "Let me ask.") {
		t.Fatalf("the advisor model was asked %d times: %v", len(asked), asked)
	}
	if strings.Contains(asked[0], "You have access to an `advisor` tool") || strings.Contains(asked[0], advisorGone) {
		t.Fatalf("the advisor was told of itself: %s", asked[0])
	}
	var uses, results, advice int
	for _, l := range strings.Split(raw, "\n") {
		data, ok := strings.CutPrefix(l, "data: ")
		if !ok {
			continue
		}
		b := gjson.Get(data, "content_block")
		switch {
		case b.Get("type").String() == "server_tool_use" && b.Get("name").String() == advisorTool:
			uses++
		case b.Get("type").String() == "advisor_tool_result":
			results++
			if b.Get("content.text").String() == "Check the tests first." {
				advice++
			}
		case b.Get("type").String() == "tool_use":
			t.Fatalf("the client was told a call of its own:\n%s", raw)
		}
	}
	if uses != advisorUses+1 || results != advisorUses+1 || advice != advisorUses {
		t.Fatalf("told %d uses, %d results, %d with the advice:\n%s", uses, results, advice, raw)
	}
}

// adviceCall makes Claude Code's call of the advisor tool and returns
// what the MCP helper is told.
func (c *tuiCalls) adviceCall(id string) mcpToolResult {
	c.t.Helper()
	r := httptest.NewRequest("POST", "/", strings.NewReader(`{"tool_call_id":"`+id+`","name":"advisor","arguments":{}}`))
	r.SetPathValue("token", "run")
	rec := httptest.NewRecorder()
	c.bridge.mcpCall(rec, r)
	var res mcpToolResult
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		c.t.Fatalf("the advisor tool said %q: %v", rec.Body.String(), err)
	}
	return res
}

// The advisor a Claude subscription's run calls is asked as the client's
// request, held to its gateway key, never as the run's own MCP call from
// this computer, which carries no key. A key held to the subscription's
// model alone that names another model as its advisor is not given that
// model through the advisor: the call fails, and the advisor's provider
// is never asked.
func TestClaudeTUIAdvisorHeldToTheCallersKey(t *testing.T) {
	u := advisedSetup(t)
	c := newTUICalls(t)
	q, err := bridgeRequest(httptest.NewRequest("POST", "/v1/messages", nil), provider.Anthropic, "Claude Code", []byte(advisorTurn("claude/claude-sonnet-5-5", "adv/adv-pro")))
	if err != nil {
		t.Fatal(err)
	}
	if q.Advisor != "adv/adv-pro" {
		t.Fatalf("the advisor is %q", q.Advisor)
	}
	held := access.WithIdentity(context.Background(), access.Identity{KeyID: "friend", KeyName: "Friend", Models: []string{"claude/claude-sonnet-5-5"}})
	c.run.adviseOn(held, q, New().adviceRound)
	c.write(tuiRow("msg_a0", `{"type":"text","text":"Let me ask."}`, "tool_use", 4))
	c.write(tuiRow("msg_a1", `{"type":"tool_use","id":"toolu_adv0","name":"mcp__magpie__advisor","input":{}}`, "tool_use", 2))
	res := c.adviceCall("toolu_adv0")
	c.write(tuiRow("msg_end", `{"type":"text","text":"Done."}`, "end_turn", 2), tuiDone)
	if !res.IsError || res.Content[0]["text"] == "Check the tests first." {
		t.Fatalf("a key held to the subscription's model was advised by adv/adv-pro: %+v", res)
	}
	u.mu.Lock()
	asked := len(u.advisor)
	u.mu.Unlock()
	if asked != 0 {
		t.Fatalf("the advisor's provider was asked %d times for a key that doesn't list it", asked)
	}
}

// The run's web search is asked as the client's request too: the
// searcher's spend counts against the client's gateway key and its limits
// hold it, never as the run's own MCP call, which carries no key.
func TestClaudeRunSearchAskedAsTheCaller(t *testing.T) {
	var asked []access.Identity
	run := &subscriptionRun{pending: map[string]chan mcpToolResult{}, searchName: "web_search"}
	run.search = func(ctx context.Context, query string) (string, []Hit, error) {
		asked = append(asked, access.Caller(ctx))
		return "found", nil, nil
	}
	b := &subscriptionBridge{runs: map[string]*subscriptionRun{"run": run}, calls: map[string]*subscriptionRun{}}
	friend := access.Identity{KeyID: "friend", KeyName: "Friend", Limit: &access.Limit{Period: "day", Tokens: 700}}
	q := &Request{Model: "claude/claude-sonnet-5-5"}
	run.adviseOn(access.WithIdentity(context.Background(), friend), q, New().adviceRound)
	r := httptest.NewRequest("POST", "/", strings.NewReader(`{"tool_call_id":"toolu_s","name":"web_search","arguments":{"query":"magpie"}}`))
	r.SetPathValue("token", "run")
	rec := httptest.NewRecorder()
	b.mcpCall(rec, r)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "found") {
		t.Fatalf("the search call was answered %d %s", rec.Code, rec.Body)
	}
	if len(asked) != 1 || asked[0].KeyID != "friend" || asked[0].Limit == nil {
		t.Fatalf("the search was asked as %+v, not the client's key", asked)
	}
}

// An advisor model named with a fixed effort suffix (swordencao, #6851:
// claude/claude-fable-5-2:high[1m]) keeps its 4096-token reply cap without
// clamping that effort down to low through a 4095-token thinking budget:
// the Claude subscription bridge runs at --effort high, a translated
// advisor receives reasoning_effort high with max_tokens 4096, an adaptive
// Claude on Messages receives adaptive thinking at high, and an older
// Claude on Messages still receives only its budget without output_config.
func TestClaudeAdvisorHonorsFixedEffort(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a shell script stands in for Claude Code")
	}
	u := advisedSetup(t)
	noRests()
	t.Cleanup(noRests)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	oauth := map[string]any{"claudeAiOauth": map[string]any{
		"accessToken": "tok-adv", "refreshToken": "tok-adv-refresh",
		"expiresAt": time.Now().Add(8 * time.Hour).UnixMilli(), "subscriptionType": "max",
	}}
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".claude", ".credentials.json"), mustJSON(oauth), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), mustJSON(map[string]any{
		"oauthAccount": map[string]any{"emailAddress": "adv@example.com", "accountUuid": "u-adv"},
	}), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	argsFile := filepath.Join(bin, "args")
	script := `#!/bin/sh
case "$1" in auth) exit 1;; esac
echo "$@" >> '` + argsFile + `'
while read -r line; do
  echo '{"type":"stream_event","event":{"type":"message_start","message":{"id":"m","model":"claude-fable-5-2","usage":{"input_tokens":1}}}}'
  echo '{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Check the tests first."}}}'
  echo '{"type":"stream_event","event":{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":4}}}'
  echo '{"type":"stream_event","event":{"type":"message_stop"}}'
  echo '{"type":"result","subtype":"success","is_error":false,"result":""}'
done
`
	testenv.Program(t, filepath.Join(bin, "claude"), script)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	provider.ForgetAccounts()
	t.Cleanup(provider.ForgetAccounts)

	var anthMu sync.Mutex
	var anthBodies []string
	anthSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		anthMu.Lock()
		anthBodies = append(anthBodies, string(b))
		anthMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"m","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"Check the tests first."}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":4}}`)
	}))
	t.Cleanup(anthSrv.Close)
	if err := provider.Save(provider.Provider{
		ID: "anth", Name: "Anth", Key: "k", Anthropic: anthSrv.URL,
		Models: []string{"claude-fable-5-2", "claude-sonnet-4-5"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := provider.SaveGroup(provider.Group{
		ID: "adv-grp", Name: "AdvGrp", Members: []string{"adv/adv-pro"},
	}); err != nil {
		t.Fatal(err)
	}

	q, err := bridgeRequest(httptest.NewRequest("POST", "/v1/messages", nil), provider.Anthropic, "Claude Code",
		[]byte(advisorTurn("claude/claude-sonnet-5-5", "claude/claude-fable-5-2:high[1m]")))
	if err != nil {
		t.Fatal(err)
	}
	if q.Advisor != "claude/claude-fable-5-2:high" {
		t.Fatalf("advisor parsed as %q, want claude/claude-fable-5-2:high", q.Advisor)
	}
	if !advisedInBridge(provider.Anthropic, []byte(advisorTurn("claude/claude-sonnet-5-5", "group/adv-grp:high"))) {
		t.Error("a routing group advisor with an explicit :high suffix was dropped")
	}

	s := New()
	t.Cleanup(s.subscription.abortAll)
	advice, err := s.advise(context.Background(), q, nil, q.Advisor)
	if err != nil || advice != "Check the tests first." {
		t.Fatalf("claude subscription advisor: %q, %v", advice, err)
	}
	rawArgs, _ := os.ReadFile(argsFile)
	if !strings.Contains(" "+strings.TrimSpace(string(rawArgs))+" ", " --effort high ") {
		t.Errorf("Claude Code run was not started at --effort high: %s", rawArgs)
	}
	if rt := lastRoute(s); len(rt.Tries) != 1 || rt.Tries[0].Fixed != "high" || rt.Tries[0].Effort != "high" {
		t.Errorf("subscription advisor route = %+v, want Fixed=high Effort=high", rt.Tries)
	}

	if advice, err := s.advise(context.Background(), q, nil, "adv/adv-pro:high"); err != nil || advice != "Check the tests first." {
		t.Fatalf("translated advisor: %q, %v", advice, err)
	}
	u.mu.Lock()
	chatBody := u.advisor[len(u.advisor)-1]
	u.mu.Unlock()
	if gjson.Get(chatBody, "max_tokens").Int() != 4096 || gjson.Get(chatBody, "reasoning_effort").String() != "high" {
		t.Errorf("translated advisor got max_tokens=%d reasoning_effort=%q, want 4096 and high: %s",
			gjson.Get(chatBody, "max_tokens").Int(), gjson.Get(chatBody, "reasoning_effort").String(), chatBody)
	}
	if rt := lastRoute(s); len(rt.Tries) != 1 || rt.Tries[0].Effort != "high" {
		t.Errorf("translated advisor route = %+v, want Effort=high", rt.Tries)
	}

	if advice, err := s.advise(context.Background(), q, nil, "anth/claude-fable-5-2:high"); err != nil || advice != "Check the tests first." {
		t.Fatalf("adaptive Messages advisor: %q, %v", advice, err)
	}
	anthMu.Lock()
	adaptiveBody := anthBodies[len(anthBodies)-1]
	anthMu.Unlock()
	if gjson.Get(adaptiveBody, "max_tokens").Int() != 4096 ||
		gjson.Get(adaptiveBody, "thinking.type").String() != "adaptive" ||
		gjson.Get(adaptiveBody, "output_config.effort").String() != "high" {
		t.Errorf("adaptive Messages advisor got %s", adaptiveBody)
	}

	if advice, err := s.advise(context.Background(), q, nil, "anth/claude-sonnet-4-5:high"); err != nil || advice != "Check the tests first." {
		t.Fatalf("older Claude Messages advisor: %q, %v", advice, err)
	}
	anthMu.Lock()
	olderBody := anthBodies[len(anthBodies)-1]
	anthMu.Unlock()
	if gjson.Get(olderBody, "max_tokens").Int() != 4096 ||
		gjson.Get(olderBody, "thinking.type").String() != "enabled" ||
		gjson.Get(olderBody, "thinking.budget_tokens").Int() != 4095 ||
		gjson.Get(olderBody, "output_config").Exists() {
		t.Fatalf("older Claude Messages advisor got %s", olderBody)
	}
}
