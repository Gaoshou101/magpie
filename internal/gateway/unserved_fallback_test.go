package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/provider"
)

// modelscopeNoProvider400 is ModelScope's answer to a model id none of its
// instances serves, as the reporter of #2887's usage.jsonl has it: the
// request itself is fine, and a member that serves the model may answer it.
const modelscopeNoProvider400 = `{"error":{"message":"Model id : deepseek-ai/DeepSeek-V4.1-Flash , has no provider supported"}}`

// A member whose vendor serves the model nowhere hands the request to the
// group's next one, and the model rests on that key alone, as any unserved
// model's does — so the client's own retry, landing on the group again, is
// no longer sent to the dead seat first (#2887: 28 requests in a row hit
// the first member and the later ones were never asked).
func TestGroupFailsOverAModelWithNoProvider(t *testing.T) {
	fresh(t)
	a := &scripted{replies: []reply{{400, "", modelscopeNoProvider400}}}
	b := &scripted{replies: []reply{{200, "", chatOK}}}
	scriptedOn(t, "ms", provider.Chat, a)
	scriptedOn(t, "b", provider.Chat, b)
	if err := provider.SaveGroup(provider.Group{Name: "G", Members: []string{"ms/m", "b/m"}, Routing: provider.Ordered}); err != nil {
		t.Fatal(err)
	}
	s := New()
	code, body := postAs(t, s, "", `{"model":"group/g","messages":[{"role":"user","content":"hi"}]}`)
	if code != 200 || !strings.Contains(body, "hello") || a.n != 1 || b.n != 1 {
		t.Fatalf("%d %s (ms %d, b %d)", code, body, a.n, b.n)
	}
	r := s.trace.routes[len(s.trace.routes)-1]
	if len(r.Tries) != 2 || r.Tries[0].Status != 400 || r.Tries[0].Fail != failOther {
		t.Fatalf("tries %+v", r.Tries)
	}
	if rest := r.Tries[0].Rest; rest == nil || rest.Key != "ms/m" {
		t.Fatalf("the model alone was to rest: %+v", r.Tries[0].Rest)
	}

	// the rested seat is passed over: the next group request, as the
	// client's retry is, is answered by the second member alone
	code, body = postAs(t, s, "", `{"model":"group/g","messages":[{"role":"user","content":"hi"}]}`)
	if code != 200 || !strings.Contains(body, "hello") || a.n != 1 || b.n != 2 {
		t.Fatalf("next: %d %s (ms %d, b %d)", code, body, a.n, b.n)
	}

	for _, x := range []struct {
		status int
		body   string
		want   bool
	}{
		{400, modelscopeNoProvider400, true},
		// as magpie tells it on, with the provider's name before, and
		// ModelScope's own invalid_request_error type around it
		{400, `ModelScope: ` + modelscopeNoProvider400, true},
		{400, `{"error":{"message":"Model id : deepseek-ai/DeepSeek-V4.1-Flash , has no provider supported","type":"invalid_request_error"}}`, true},
		// Azure OpenAI's error for a deployment its resource has none of
		// (DeploymentNotFound, "The API deployment for this resource does
		// not exist"): a 404 fails over by its status already, a relay's
		// 400 says the words
		{400, `{"error":{"code":"DeploymentNotFound","message":"The API deployment for this resource does not exist. If you created the deployment recently, please wait a few minutes and try again."}}`, true},
		{400, `{"error":{"message":"Deployment not found for ModelName: gpt-4o"}}`, true},
		// model_not_found fails over as it did
		{400, `{"error":{"message":"The model gpt-x does not exist","code":"model_not_found"}}`, true},
		// a request at fault stays the agent's: no member is asked after
		{400, `{"error":{"message":"Invalid parameter: messages.3.content"}}`, false},
		{400, `{"error":{"message":"messages: field required"}}`, false},
	} {
		if got := retryable(x.status, []byte(x.body)); got != x.want {
			t.Errorf("retryable(%d, %s) = %v", x.status, x.body, got)
		}
		if got := shapeRefused(x.status, []byte(x.body)); got {
			t.Errorf("shapeRefused(%d, %s) = %v; an unserved model rests, it is not a shape", x.status, x.body, got)
		}
	}
	if !modelRefused(400, []byte(modelscopeNoProvider400)) {
		t.Errorf("modelRefused false: the model wouldn't rest alone on its key")
	}
}

// A provider returning HTTP 400 with a localized or English "model is
// currently unavailable" refusal (#7184: Russian "Модель ... сейчас недоступна")
// must fail over to the group's next member and rest only that model on
// that provider, while leaving other models on the same provider unblocked
// and keeping service/endpoint-level outages out of modelRefused.
func TestGroupFailsOverModelUnavailable400(t *testing.T) {
	const russianUnavailable = "Модель deepseek-v4.1-flash сейчас недоступна.\nОткрой список моделей и выбери другую."

	fresh(t)
	a := &scripted{replies: []reply{{400, "text/plain; charset=utf-8", russianUnavailable}}}
	aSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bodyBytes, _ := io.ReadAll(r.Body)
		if strings.Contains(string(bodyBytes), `"model":"other"`) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, chatOK)
			return
		}
		a.ServeHTTP(w, r)
	}))
	t.Cleanup(aSrv.Close)
	if err := provider.Save(provider.Provider{ID: "a", Name: "A", Key: "k", Models: []string{"m", "other"}, Chat: aSrv.URL + "/v1"}); err != nil {
		t.Fatal(err)
	}
	b := &scripted{replies: []reply{{200, "", chatOK}}}
	scriptedOn(t, "b", provider.Chat, b)
	if err := provider.SaveGroup(provider.Group{Name: "G", Members: []string{"a/m", "b/m"}, Routing: provider.Auto}); err != nil {
		t.Fatal(err)
	}

	s := New()
	code, body := postAs(t, s, "", `{"model":"group/g","messages":[{"role":"user","content":"hi"}]}`)
	if code != 200 || !strings.Contains(body, "hello") || a.n != 1 || b.n != 1 {
		t.Fatalf("first try: %d %s (a %d, b %d)", code, body, a.n, b.n)
	}
	r := s.trace.routes[len(s.trace.routes)-1]
	if len(r.Tries) != 2 || r.Tries[0].Status != 400 || r.Tries[0].Fail != failOther {
		t.Fatalf("tries %+v", r.Tries)
	}
	if rest := r.Tries[0].Rest; rest == nil || rest.Key != "a/m" {
		t.Fatalf("expected only a/m to rest, got %+v", r.Tries[0].Rest)
	}

	// Next group request skips the rested a/m and goes directly to b/m.
	code, body = postAs(t, s, "", `{"model":"group/g","messages":[{"role":"user","content":"hi"}]}`)
	if code != 200 || !strings.Contains(body, "hello") || a.n != 1 || b.n != 2 {
		t.Fatalf("second try: %d %s (a %d, b %d)", code, body, a.n, b.n)
	}

	// Other models on provider a remain unblocked.
	code, body = postAs(t, s, "", `{"model":"a/other","messages":[{"role":"user","content":"hi"}]}`)
	if code != 200 || !strings.Contains(body, "hello") {
		t.Fatalf("sibling model a/other blocked unexpectedly: %d %s", code, body)
	}

	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{"russian model unavailable", 400, russianUnavailable, true},
		{"english model currently unavailable", 400, `{"error":{"message":"Model deepseek-v4.1-flash is currently unavailable."}}`, true},
		{"english model unavailable", 400, `{"error":{"message":"Model is unavailable"}}`, true},
		{"chinese model unavailable with id", 400, `{"error":{"message":"模型 deepseek-v4.1-flash 当前不可用"}}`, true},
		{"chinese model temp unavailable", 400, `{"error":{"message":"当前模型暂时不可用"}}`, true},
		{"english model in parens unavailable", 400, `{"error":{"message":"The selected model (deepseek-v4.1-flash) is currently unavailable."}}`, true},
		{"endpoint unavailable is not model unserved", 400, `{"error":{"message":"Endpoint is currently unavailable."}}`, false},
		{"plain service unavailable is not model unserved", 400, `{"error":{"message":"service unavailable"}}`, false},
		{"model prefix with service unavailable is not model unserved", 400, `{"error":{"message":"model gpt-4o: service is currently unavailable"}}`, false},
		{"missing field stays non-retryable", 400, `{"error":{"message":"messages: field required"}}`, false},
	} {
		if got := retryable(tc.status, []byte(tc.body)); got != tc.want {
			t.Errorf("%s: retryable(%d) = %v, want %v", tc.name, tc.status, got, tc.want)
		}
		if got := modelRefused(tc.status, []byte(tc.body)); got != tc.want {
			t.Errorf("%s: modelRefused(%d) = %v, want %v", tc.name, tc.status, got, tc.want)
		}
	}
}
