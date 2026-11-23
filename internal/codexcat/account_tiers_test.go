package codexcat

import (
	"encoding/json"
	"testing"

	"github.com/yetone/magpie/internal/catalog"
)

// #7493: the tiers a ChatGPT API account's own list advertises reach the
// entries Codex is handed, so the gateway's /v1/models tells them.
func TestAccountAdvertisedTiersReachTheEntries(t *testing.T) {
	v1Home(t, `{"etag":"W/\"a\"","models":[]}`)
	ms := []catalog.Model{{ID: "chatgpt-api/gpt-6.2-sol", Name: "GPT-6.2 Sol", Tiers: []string{"priority", "ultrafast"}}}
	var got struct {
		Models []struct {
			Slug  string `json:"slug"`
			Tiers []struct {
				ID string `json:"id"`
			} `json:"service_tiers"`
		} `json:"models"`
	}
	if err := json.Unmarshal(Catalog(ms), &got); err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, m := range got.Models {
		if m.Slug == "chatgpt-api/gpt-6.2-sol" {
			found = true
			if len(m.Tiers) != 2 || m.Tiers[0].ID != "priority" || m.Tiers[1].ID != "ultrafast" {
				t.Fatalf("tiers %v", m.Tiers)
			}
		}
	}
	if !found {
		t.Fatalf("model lost: %v", got)
	}
}
