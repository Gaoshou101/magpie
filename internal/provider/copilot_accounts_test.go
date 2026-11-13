package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/catalog"
)

// A second Copilot account, signed in from magpie with GitHub's device
// code, is kept beside the editors' own and can go first.
func TestCopilotAccounts(t *testing.T) {
	signIn(t) // the editors' own: octocat, gho_x
	var polls atomic.Int32
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		switch r.URL.Path {
		case "/device":
			if r.Form.Get("client_id") != copilotClientID {
				w.WriteHeader(400)
				return
			}
			w.Write([]byte(`{"device_code":"dev","user_code":"ABCD-1234","verification_uri":"https://github.com/login/device","interval":0}`))
		case "/token":
			if r.Form.Get("device_code") != "dev" {
				w.WriteHeader(400)
				return
			}
			if polls.Add(1) < 2 {
				w.Write([]byte(`{"error":"authorization_pending"}`))
				return
			}
			w.Write([]byte(`{"access_token":"gho_hubot","token_type":"bearer"}`))
		case "/user":
			if r.Header.Get("Authorization") != "token gho_hubot" {
				w.WriteHeader(401)
				return
			}
			w.Write([]byte(`{"login":"hubot"}`))
		case "/copilot_user":
			w.Write([]byte(`{"copilot_plan":"individual_pro"}`))
		case "/copilot_token":
			tok := strings.TrimPrefix(r.Header.Get("Authorization"), "token ")
			json.NewEncoder(w).Encode(map[string]any{"token": "session-" + tok, "expires_at": time.Now().Add(time.Hour).Unix()})
		}
	}))
	defer gh.Close()
	oldDev, oldTok, oldUser, oldCU, oldCT := gitHubDeviceURL, gitHubTokenURL, GitHubUserURL, CopilotUserURL, CopilotTokenURL
	gitHubDeviceURL, gitHubTokenURL, GitHubUserURL, CopilotUserURL, CopilotTokenURL = gh.URL+"/device", gh.URL+"/token", gh.URL+"/user", gh.URL+"/copilot_user", gh.URL+"/copilot_token"
	defer func() {
		gitHubDeviceURL, gitHubTokenURL, GitHubUserURL, CopilotUserURL, CopilotTokenURL = oldDev, oldTok, oldUser, oldCU, oldCT
	}()
	copilotSessions = map[string]copilotSession{}

	st, err := StartSignIn("copilot")
	if err != nil || st.Code != "ABCD-1234" || st.URL != "https://github.com/login/device" {
		t.Fatalf("start: %+v %v", st, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	st, _ = WaitSignIn(ctx, st.ID)
	if st.State != "done" || st.User != "hubot" || st.Plan != "Pro+" || st.Using || st.Code != "ABCD-1234" {
		t.Fatalf("done: %+v", st)
	}

	users := func() string {
		var s []string
		for _, l := range Logins("copilot") {
			s = append(s, l.User+map[bool]string{true: "*", false: ""}[l.Active]+map[bool]string{true: "+", false: ""}[l.On])
		}
		return strings.Join(s, " ")
	}
	if got := users(); got != "octocat*+ hubot+" {
		t.Fatalf("logins: %s", got)
	}
	p, ok := find(All(), "copilot")
	if !ok || p.Account.User != "octocat" {
		t.Fatalf("first: %+v", p)
	}
	also := p.AlsoOn()
	if len(also) != 1 || also[0].Account.User != "hubot" || also[0].Account.Plan != "Pro+" {
		t.Fatalf("also on: %+v", also)
	}
	req, _ := http.NewRequest("POST", also[0].Chat+"/chat/completions", nil)
	if err := also[0].Sign(context.Background(), req, Chat, []byte(`{}`)); err != nil || req.Header.Get("Authorization") != "Bearer session-gho_hubot" {
		t.Fatalf("hubot signs with its own: %v %s", err, req.Header.Get("Authorization"))
	}

	if err := SwitchLogin("copilot", "hubot"); err != nil {
		t.Fatal(err)
	}
	if got := users(); got != "hubot*+ octocat+" {
		t.Fatalf("after switch: %s", got)
	}
	// ticked off, the first hands first to the next ticked (Rime_Nan on X)
	if err := SetLoginOn("copilot", "hubot", false); err != nil {
		t.Fatal(err)
	}
	if got := users(); got != "octocat*+ hubot" {
		t.Fatalf("after turning the first off: %s", got)
	}
	if err := SetLoginOn("copilot", "octocat", false); err == nil {
		t.Fatal("turned off the only account in use")
	}
	if err := SetLoginOn("copilot", "hubot", true); err != nil {
		t.Fatal(err)
	}
	if err := ForgetLogin("copilot", "hubot"); err != nil {
		t.Fatal(err)
	}
	if got := users(); got != "octocat*+" {
		t.Fatalf("after forget: %s", got)
	}
}

// #6388: copilot provider must declare Catalog: "github-copilot" so that models
// that refuse temperature (e.g. gpt-6-luna) have RejectsTemperature == true.
func TestCopilotProviderCatalogRejectsTemperature(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	if err := os.MkdirAll(filepath.Dir(catalog.CachePath()), 0o755); err != nil {
		t.Fatal(err)
	}
	data := `{
		"github-copilot": {"id": "github-copilot", "models": {"gpt-6-luna": {"id": "gpt-6-luna", "temperature": false}}}
	}`
	if err := os.WriteFile(catalog.CachePath(), []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	catalog.Reset()
	t.Cleanup(catalog.Reset)

	app := copilotApp{User: "octocat"}
	p := copilotProvider(app, "Pro")
	if p.Catalog != "github-copilot" {
		t.Errorf("copilotProvider().Catalog = %q, want %q", p.Catalog, "github-copilot")
	}
	if !p.RejectsTemperature("gpt-6-luna") {
		t.Errorf("p.RejectsTemperature(\"gpt-6-luna\") = false, want true")
	}
}
