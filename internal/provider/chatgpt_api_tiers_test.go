package provider

import (
	"testing"

	"github.com/yetone/magpie/internal/catalog"
)

// #7493: a ChatGPT API account's model list advertises service_tiers
// (priority and ultrafast) per model; parseSIWCModels drops them, so the
// gateway's /v1/models carries no service_tiers on a chatgpt-api model.
func TestParseSIWCModelsKeepsServiceTiers(t *testing.T) {
	fixture := []byte(`{"models":[{"slug":"gpt-6.2-sol","visibility":"list","service_tiers":[{"id":"priority"},{"id":"ultrafast"}]}]}`)
	ms, hidden := parseSIWCModels(fixture)
	if len(hidden) != 0 {
		t.Fatalf("hidden %v", hidden)
	}
	if len(ms) != 1 || ms[0].ID != "gpt-6.2-sol" {
		t.Fatalf("models %v", ms)
	}
	if got := ms[0].Tiers; len(got) != 2 || got[0] != "priority" || got[1] != "ultrafast" {
		t.Fatalf("service_tiers lost: %+v", ms[0])
	}
}

// #7493: the tiers a subscription account's list advertises survive
// CodexTiersWith on the entry built from it, on the provider the account
// is served by (not a remote magpie's, not a plugin's).
func TestCodexTiersKeepAnAccountAdvertisedTier(t *testing.T) {
	m := catalog.Model{ID: "gpt-6.2-sol", Name: "GPT-6.2 Sol"}
	p := Provider{ID: ChatGPTAPIID, Name: "ChatGPT API", Responses: siwcAPI,
		Account: &Account{Agent: ChatGPTAPIID, User: "u"}}
	e := Entry{ID: "chatgpt-api/gpt-6.2-sol", Model: "gpt-6.2-sol", Provider: &p, Tiers: []string{"priority", "ultrafast"}}
	CodexTiersWith(&m, e, func(string) []Member { return nil }, func() map[string]bool { return nil })
	if got := m.Tiers; len(got) != 2 || got[0] != "priority" || got[1] != "ultrafast" {
		t.Fatalf("codex tiers %v", got)
	}
}
