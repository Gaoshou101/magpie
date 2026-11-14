package gateway

import (
	"encoding/json"
	"reflect"
	"testing"
)

// #6592: Stop sequences are passed through when a request is translated to the
// Responses API, or when a Responses client sends them itself.
func TestBuildResponsesStop(t *testing.T) {
	r := &Request{
		Model: "m",
		Stop:  []string{"STOP", "END"},
	}
	raw := buildResponses(r, "m", "relay.example", false)
	var got struct {
		Stop []string `json:"stop"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	want := []string{"STOP", "END"}
	if !reflect.DeepEqual(got.Stop, want) {
		t.Fatalf("buildResponses got stop %v, want %v (raw: %s)", got.Stop, want, raw)
	}
}

func TestParseResponsesStop(t *testing.T) {
	// array of stop sequences
	r, err := parseResponses([]byte(`{"model":"m","input":"hi","stop":["STOP","END"]}`))
	if err != nil {
		t.Fatalf("parseResponses array: %v", err)
	}
	if want := []string{"STOP", "END"}; !reflect.DeepEqual(r.Stop, want) {
		t.Errorf("got %v, want %v", r.Stop, want)
	}

	// single string stop sequence
	r, err = parseResponses([]byte(`{"model":"m","input":"hi","stop":"STOP"}`))
	if err != nil {
		t.Fatalf("parseResponses single string: %v", err)
	}
	if want := []string{"STOP"}; !reflect.DeepEqual(r.Stop, want) {
		t.Errorf("got %v, want %v", r.Stop, want)
	}
}

// TestTranslateStopToResponses verifies that Chat Completions and Anthropic Messages
// requests with stop sequences preserve them when built for a Responses upstream.
func TestTranslateStopToResponses(t *testing.T) {
	// Chat completions with stop
	chatReq, err := parseChat([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"stop":["STOP"]}`))
	if err != nil {
		t.Fatalf("parseChat: %v", err)
	}
	chatBuilt := buildResponses(chatReq, "m", "relay.example", false)
	var chatGot struct {
		Stop []string `json:"stop"`
	}
	if err := json.Unmarshal(chatBuilt, &chatGot); err != nil {
		t.Fatalf("unmarshal chatBuilt: %v", err)
	}
	if want := []string{"STOP"}; !reflect.DeepEqual(chatGot.Stop, want) {
		t.Errorf("chat -> responses stop got %v, want %v", chatGot.Stop, want)
	}

	// Anthropic messages with stop_sequences
	anthReq, err := parseAnthropic([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"stop_sequences":["STOP"]}`))
	if err != nil {
		t.Fatalf("parseAnthropic: %v", err)
	}
	anthBuilt := buildResponses(anthReq, "m", "relay.example", false)
	var anthGot struct {
		Stop []string `json:"stop"`
	}
	if err := json.Unmarshal(anthBuilt, &anthGot); err != nil {
		t.Fatalf("unmarshal anthBuilt: %v", err)
	}
	if want := []string{"STOP"}; !reflect.DeepEqual(anthGot.Stop, want) {
		t.Errorf("anthropic -> responses stop got %v, want %v", anthGot.Stop, want)
	}
}
