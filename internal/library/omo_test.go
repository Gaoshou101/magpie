package library

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// OmO's engine reads its agent folder's AGENTS.md, skills and mcp.json
// (#264), the servers in the shape Pi 0.99's own has; a key its schema
// doesn't know refuses the whole file, so none of the adapters' is
// written. OMO_CODING_AGENT_DIR moves all three.
func TestOmoTarget(t *testing.T) {
	h := sandbox(t)
	d := filepath.Join(h, ".omo", "agent")
	write(t, filepath.Join(d, "settings.json"), "{}")
	holdCLI(t, h, "omo") // the folder alone is gone (#2177)
	tg := targetByID("omo")
	if tg == nil || tg.MCP == nil || tg.MCP.Path != filepath.Join(d, "mcp.json") || tg.MCP.Format != fmtPiNative ||
		tg.Instructions != filepath.Join(d, "AGENTS.md") || tg.Skills != filepath.Join(d, "skills") {
		t.Fatalf("%+v", tg)
	}
	native := tg.MCP.Path
	write(t, native, `{"settings": {"toolPrefix": "mcp"}, "mcpServers": {"theirs": {"command": "t", "lifecycle": "eager"}}}`)
	ok(t)(SaveServer("", Server{Name: "docs", Transport: "http", URL: "https://example.com/mcp",
		Headers: map[string]string{"Authorization": "Bearer ${DOCS_TOKEN}"}, Agents: []string{"omo"}}))
	ok(t)(SaveServer("", Server{Name: "fs", Transport: "stdio", Command: "npx", Args: []string{"-y", "fs"},
		Env: map[string]string{"K": "${K}"}, Agents: []string{"omo"}}))
	got := piServers(t, native)
	allowed := []string{"type", "url", "command", "args", "env", "cwd", "headers", "auth", "bearerTokenEnv", "oauth", "enabled",
		"lifecycle", "idleTimeoutMin", "requestTimeoutMs", "connectTimeoutMs", "startupTimeoutMs", "includeTools", "excludeTools",
		"directTools", "exposure", "logLevel"}
	for name, e := range got {
		for k := range e {
			if !slices.Contains(allowed, k) {
				t.Errorf("%s has %q, which OmO refuses", name, k)
			}
		}
	}
	if got["docs"]["url"] != "https://example.com/mcp" || got["fs"]["command"] != "npx" || got["theirs"]["lifecycle"] != "eager" ||
		piDoc(t, native)["settings"] == nil {
		t.Fatalf("mcp.json: %s", read(t, native))
	}
	if exists(filepath.Join(h, ".pi", "agent", "mcp.json")) || exists(filepath.Join(h, ".pi", "agent", "mcp-adapter.json")) {
		t.Error("Pi's files written")
	}
	if err := tg.MCP.supports(&Server{Transport: "sse"}); err != errNoSSE {
		t.Errorf("sse: %v", err)
	}
	ok(t)(RemoveServer("fs"))
	if piServers(t, native)["fs"] != nil {
		t.Error("fs left behind")
	}

	// with the MCP router on (including Magpie Artifacts behind it), the
	// router entry keeps only keys OmO's ServerSchema allows: no
	// description or toolExposure
	ok(t)(SaveServer("", Server{Name: "wiki", Transport: "http", URL: "https://mcp.deepwiki.com/mcp", Agents: []string{"omo"}}))
	art := fakeStdio(t, "ok", nil)
	art.Name, art.Args, art.Agents = ArtifactsServerName, []string{"mcp", "artifacts"}, []string{"omo"}
	ok(t)(SaveServer("", *art))
	ok(t)(SetMCPHub(true))
	got = piServers(t, native)
	if got[HubEntry] == nil {
		t.Fatalf("router entry missing: %s", read(t, native))
	}
	for name, e := range got {
		for k := range e {
			if !slices.Contains(allowed, k) {
				t.Errorf("hub: %s has %q, which OmO refuses", name, k)
			}
		}
	}
	if got[HubEntry]["description"] != nil || got[HubEntry]["toolExposure"] != nil {
		t.Fatalf("router entry carries Pi-only keys: %v", got[HubEntry])
	}
	// a direct Artifacts server for OmO also omits toolExposure
	ok(t)(ServerHub(ArtifactsServerName, true))
	got = piServers(t, native)
	if got[ArtifactsServerName] == nil {
		t.Fatalf("direct artifacts missing: %s", read(t, native))
	}
	for k := range got[ArtifactsServerName] {
		if !slices.Contains(allowed, k) {
			t.Errorf("direct artifacts has %q, which OmO refuses", k)
		}
	}
	// a user-owned exposure on an entry in mcp.json stays untouched when
	// magpie re-syncs the file, while legacy Pi-only keys previously written
	// on HubEntry or direct Artifacts are stripped on re-sync
	ok(t)(SaveServer("", Server{Name: "custom", Transport: "stdio", Command: "custom-bin", Agents: []string{"omo"}, Hub: "direct"}))
	doc := read(t, native)
	patched := strings.Replace(doc, `"command":"custom-bin"`, `"command":"custom-bin","exposure":"direct"`, 1)
	if patched == doc {
		t.Fatalf("failed to inject exposure into %s", doc)
	}
	write(t, native, patched)
	ok(t)(SaveServer("custom", Server{Name: "custom", Transport: "stdio", Command: "custom-bin", Args: []string{"--v"}, Agents: []string{"omo"}, Hub: "direct"}))
	got = piServers(t, native)
	if got["custom"]["exposure"] != "direct" {
		t.Fatalf("user-owned exposure altered: %v", got["custom"])
	}
	// simulate legacy magpie output on HubEntry (description + toolExposure)
	// and direct Artifacts (toolExposure) and verify Sync() cleans both
	write(t, native, `{"mcpServers":{"magpie":{"url":"http://127.0.0.1:3425/mcp/magpie/hub/omo","description":"legacy","toolExposure":{"publish_artifact":"direct"}},"magpie-artifacts":{"command":"`+art.Command+`","args":["mcp","artifacts"],"toolExposure":{"publish_artifact":"direct"}},"custom":{"command":"custom-bin","args":["--v"],"exposure":"direct"}}}`)
	ok(t)(Sync())
	got = piServers(t, native)
	if got[HubEntry] == nil || got[HubEntry]["description"] != nil || got[HubEntry]["toolExposure"] != nil {
		t.Fatalf("legacy router keys remained after sync: %v", got[HubEntry])
	}
	if got[ArtifactsServerName] == nil || got[ArtifactsServerName]["toolExposure"] != nil {
		t.Fatalf("legacy artifacts toolExposure remained after sync: %v", got[ArtifactsServerName])
	}
	if got["custom"]["exposure"] != "direct" {
		t.Fatalf("user-owned exposure lost during legacy cleanup: %v", got["custom"])
	}

	own := filepath.Join(h, "omo-elsewhere")
	t.Setenv("OMO_CODING_AGENT_DIR", own)
	write(t, filepath.Join(own, "settings.json"), "{}")
	if tg := targetByID("omo"); tg == nil || tg.MCP.Path != filepath.Join(own, "mcp.json") || tg.Skills != filepath.Join(own, "skills") {
		t.Fatalf("OMO_CODING_AGENT_DIR: %+v", tg)
	}
}

func TestOmoWSLNoHubMeta(t *testing.T) {
	h := wslSandbox(t)
	ok(t)(SaveServer("", Server{Name: "wiki", Transport: "http", URL: "https://mcp.deepwiki.com/mcp", Agents: []string{wslOmO}}))
	art := fakeStdio(t, "ok", nil)
	art.Name, art.Args, art.Agents = ArtifactsServerName, []string{"mcp", "artifacts"}, []string{wslOmO}
	ok(t)(SaveServer("", *art))
	ok(t)(SetMCPHub(true))
	wslFile := filepath.Join(h, ".omo", "agent", "mcp.json")
	got := piServers(t, wslFile)
	if got[HubEntry] == nil {
		t.Fatalf("wsl router entry missing: %v", got)
	}
	if got[HubEntry]["description"] != nil || got[HubEntry]["toolExposure"] != nil {
		t.Fatalf("wsl router entry carries Pi-only keys: %v", got[HubEntry])
	}
	// legacy Pi-only keys in WSL mcp.json are also stripped on Sync()
	write(t, wslFile, `{"mcpServers":{"magpie":{"url":"http://127.0.0.1:3425/mcp/magpie/hub/omo","description":"legacy","toolExposure":{"publish_artifact":"direct"}}}}`)
	ok(t)(Sync())
	got = piServers(t, wslFile)
	if got[HubEntry] == nil || got[HubEntry]["description"] != nil || got[HubEntry]["toolExposure"] != nil {
		t.Fatalf("wsl legacy router keys remained after sync: %v", got[HubEntry])
	}
}
