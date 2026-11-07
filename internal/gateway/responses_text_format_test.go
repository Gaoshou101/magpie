package gateway

import (
	"encoding/json"
	"testing"
)

// A Responses client's text with no format (Codex sends {"verbosity":"low"}
// for a model whose catalog entry supports verbosity) goes upstream with
// the format OpenAI defaults to, {"type":"text"}: Baidu Qianfan's Responses
// endpoint refuses a text without text.format.type ("Invalid value: ,
// Supported values are: text, json_object, json_schema", #5600). A format
// the client did name goes on as it was.
func TestResponsesTextWithoutFormatSaysPlainText(t *testing.T) {
	for _, c := range []struct {
		name, text, want string
	}{
		{"verbosity only", `{"verbosity":"low"}`, `{"format":{"type":"text"},"verbosity":"low"}`},
		{"empty text", `{}`, `{"format":{"type":"text"}}`},
		{"null format", `{"verbosity":"low","format":null}`, `{"format":{"type":"text"},"verbosity":"low"}`},
		{"format with no type", `{"format":{}}`, `{"format":{"type":"text"}}`},
		{"format with null type", `{"format":{"type":null}}`, `{"format":{"type":"text"}}`},
		{"format with empty type", `{"verbosity":"high","format":{"type":""}}`, `{"format":{"type":"text"},"verbosity":"high"}`},
		{"plain text kept", `{"verbosity":"low","format":{"type":"text"}}`, `{"format":{"type":"text"},"verbosity":"low"}`},
		{"json object kept", `{"verbosity":"low","format":{"type":"json_object"}}`, `{"format":{"type":"json_object"},"verbosity":"low"}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			r, err := parseResponses([]byte(`{"model":"baidu-qianfan/deepseek-v4.1-flash","input":"hi","text":` + c.text + `}`))
			if err != nil {
				t.Fatal(err)
			}
			var built struct {
				Text json.RawMessage `json:"text"`
			}
			if err := json.Unmarshal(buildResponses(r, "deepseek-v4.1-flash", "qianfan.baidubce.com", false), &built); err != nil {
				t.Fatal(err)
			}
			if !sameFormat(t, string(built.Text), c.want) {
				t.Fatalf("text = %s, want %s", built.Text, c.want)
			}
		})
	}
	// a client that sent no text sends none upstream
	r, err := parseResponses([]byte(`{"model":"gpt-5","input":"hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	var built map[string]json.RawMessage
	if err := json.Unmarshal(buildResponses(r, "gpt-5", "api.openai.com", false), &built); err != nil {
		t.Fatal(err)
	}
	if text, ok := built["text"]; ok {
		t.Fatalf("no text sent, upstream got text %s", text)
	}
}
