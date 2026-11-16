package gateway

import (
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tidwall/gjson"
	"github.com/yetone/magpie/internal/catalog"
	"github.com/yetone/magpie/internal/provider"
)

func TestImagesUsageInputTokensDetails(t *testing.T) {
	// Case 1: upstream omits input_tokens_details -> magpie emits {text_tokens: input_tokens, image_tokens: 0}
	s, _ := easeled(t)
	code, _, raw := postImages(t, s, "/v1/images/generations", "application/json", `{"model":"art/gpt-image-1","prompt":"a magpie"}`)
	if code != 200 {
		t.Fatalf("code %d: %s", code, raw)
	}
	details := gjson.Get(raw, "usage.input_tokens_details")
	if !details.Exists() {
		t.Fatalf("expected usage.input_tokens_details in response: %s", raw)
	}
	if text := details.Get("text_tokens").Int(); text != 7 {
		t.Fatalf("got text_tokens %d, want 7", text)
	}
	if img := details.Get("image_tokens").Int(); img != 0 {
		t.Fatalf("got image_tokens %d, want 0", img)
	}

	// Case 2: upstream provides input_tokens_details -> magpie passes it through
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		b64 := base64.StdEncoding.EncodeToString(pngBytes)
		io.WriteString(w, `{"created":1,"data":[{"b64_json":"`+b64+`"}],"usage":{"input_tokens":15,"input_tokens_details":{"text_tokens":10,"image_tokens":5},"output_tokens":100}}`)
	}))
	t.Cleanup(up.Close)

	if err := provider.Save(provider.Provider{ID: "details-test", Name: "DetailsTest", Chat: up.URL + "/v1", Key: "key", Models: []string{"gpt-image-custom"}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { provider.Delete("details-test") })
	if err := catalog.SaveLive("details-test", up.URL+"/v1", []catalog.Model{{ID: "gpt-image-custom"}}); err != nil {
		t.Fatal(err)
	}

	code2, _, raw2 := postImages(t, s, "/v1/images/generations", "application/json", `{"model":"details-test/gpt-image-custom","prompt":"a magpie"}`)
	if code2 != 200 {
		t.Fatalf("code %d: %s", code2, raw2)
	}
	details2 := gjson.Get(raw2, "usage.input_tokens_details")
	if !details2.Exists() {
		t.Fatalf("expected usage.input_tokens_details in response: %s", raw2)
	}
	if text := details2.Get("text_tokens").Int(); text != 10 {
		t.Fatalf("got text_tokens %d, want 10", text)
	}
	if img := details2.Get("image_tokens").Int(); img != 5 {
		t.Fatalf("got image_tokens %d, want 5", img)
	}
}
