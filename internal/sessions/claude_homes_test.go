package sessions

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
	"unsafe"
)

// Claude run with CLAUDE_CONFIG_DIR set to a folder of each account's
// (~/.claude-work beside the one magpie runs with) is counted too, a session
// kept in both folders once, and a folder with no projects/ is no Claude
// folder (@fivesmallq on X: only one Claude folder's token use counted).
func TestCallsReadEveryClaudeFolder(t *testing.T) {
	d := setupCalls(t)
	home, _ := os.UserHomeDir()
	work := filepath.Join(home, ".claude-work")
	writeLines(t, filepath.Join(d.claude, "projects", "-a", "main1.jsonl"),
		claudeMsg("m1", "claude-opus-5-5", 1, 2, 0, 0, 1))
	writeLines(t, filepath.Join(work, "projects", "-b", "work1.jsonl"),
		swap(claudeMsg("m2", "claude-opus-5-5", 3, 4, 0, 0, 2), `"sessionId":"sess1"`, `"sessionId":"work1"`))
	// the same session copied into both folders
	writeLines(t, filepath.Join(work, "projects", "-a", "main1.jsonl"),
		claudeMsg("m1", "claude-opus-5-5", 1, 2, 0, 0, 1))
	writeLines(t, filepath.Join(home, ".claude-notes", "todo.jsonl"), "{}")

	cs := Calls(time.Time{})
	got := map[string]int{}
	for _, c := range cs {
		got[c.RequestID]++
	}
	if len(cs) != 2 || got["req_m1"] != 1 || got["req_m2"] != 1 {
		t.Fatalf("calls = %+v, want req_m1 and req_m2 once each", cs)
	}
}

// A session kept in two Claude folders is one row of the list, and the
// other folder's own sessions are listed.
func TestListReadsEveryClaudeFolderOnce(t *testing.T) {
	d := setupCalls(t)
	home, _ := os.UserHomeDir()
	work := filepath.Join(home, ".claude-work")
	user := func(id string) string {
		return `{"type":"user","timestamp":"` + stamp(0) + `","message":{"role":"user","content":"hello"},"cwd":"/work/app","sessionId":"` + id + `"}`
	}
	writeLines(t, filepath.Join(d.claude, "projects", "-a", "main1.jsonl"), user("main1"))
	writeLines(t, filepath.Join(work, "projects", "-a", "main1.jsonl"), user("main1"))
	writeLines(t, filepath.Join(work, "projects", "-b", "work1.jsonl"), user("work1"))
	n := map[string]int{}
	for _, s := range List(0) {
		n[s.ID]++
	}
	if len(n) != 2 || n["main1"] != 1 || n["work1"] != 1 {
		t.Fatalf("rows = %v, want main1 and work1 once each", n)
	}
}

// Every Claude folder's files are listed into one slice made at their
// size, not grown file by file on every pass (a pass every 10 s while
// the Sessions page is open), a pass like the last gives the last one's
// list, and a session kept in two folders is still listed once.
func TestClaudeFilesMadeAtTheirSize(t *testing.T) {
	d := setupCalls(t)
	home, _ := os.UserHomeDir()
	work := filepath.Join(home, ".claude-work")
	user := func(id string) string {
		return `{"type":"user","timestamp":"` + stamp(0) + `","message":{"role":"user","content":"hello"},"cwd":"/work/app","sessionId":"` + id + `"}`
	}
	for i := range 20 {
		id := "main" + itoa(i)
		writeLines(t, filepath.Join(d.claude, "projects", "-a", id+".jsonl"), user(id))
		id = "work" + itoa(i)
		writeLines(t, filepath.Join(work, "projects", "-b", id+".jsonl"), user(id))
	}
	writeLines(t, filepath.Join(work, "projects", "-a", "main0.jsonl"), user("main0"))
	if len(ClaudeDirs()) != 2 {
		t.Fatalf("folders %v", ClaudeDirs())
	}
	fs := claudeFiles()
	if len(fs) != 40 {
		t.Fatalf("%d files, want 40", len(fs))
	}
	if cap(fs) != 40 {
		t.Fatalf("listed into room for %d, want the 40 kept", cap(fs))
	}
	if again := claudeFiles(); unsafe.SliceData(again) != unsafe.SliceData(fs) {
		t.Fatal("an unchanged pass made its list again")
	}
}

// Sessions in a Claude config folder other than ClaudeDir() include
// CLAUDE_CONFIG_DIR on their Resume line, while a link to ClaudeDir() and a
// WSL session do not.
func TestClaudeResumeCommandNamesNonDefaultConfigDir(t *testing.T) {
	d := setupCalls(t)
	home, _ := os.UserHomeDir()
	work := filepath.Join(home, ".claude-work")
	prof := filepath.Join(home, ".claude-profiles", "client-a")
	user := func(id string) string {
		return `{"type":"user","timestamp":"` + stamp(0) + `","message":{"role":"user","content":"hello"},"cwd":"/work/app","sessionId":"` + id + `"}`
	}
	writeLines(t, filepath.Join(d.claude, "projects", "-a", "main1.jsonl"), user("main1"))
	writeLines(t, filepath.Join(work, "projects", "-b", "work1.jsonl"), user("work1"))
	writeLines(t, filepath.Join(prof, "projects", "-c", "prof1.jsonl"), user("prof1"))

	rows := List(0)
	byID := map[string]Session{}
	for _, r := range rows {
		byID[r.ID] = r
	}
	wantResume := func(dir, id, cwd string) string {
		run := "claude --resume " + id
		if dir != "" {
			if runtime.GOOS == "windows" {
				run = "$__claude_cfg = $env:CLAUDE_CONFIG_DIR; try { $env:CLAUDE_CONFIG_DIR = " + psQuote(dir) + "; " + run + " } finally { $env:CLAUDE_CONFIG_DIR = $__claude_cfg }"
			} else {
				run = "CLAUDE_CONFIG_DIR=" + shellQuote(dir) + " " + run
			}
		}
		if cwd == "" {
			return run
		}
		if runtime.GOOS == "windows" {
			return "Set-Location -LiteralPath " + psQuote(cwd) + "; " + run
		}
		return "cd " + shellQuote(cwd) + " && " + run
	}

	if got, want := byID["main1"].Resume, wantResume("", "main1", "/work/app"); got != want {
		t.Errorf("default folder resume = %q, want %q", got, want)
	}
	if got, want := byID["work1"].Resume, wantResume(work, "work1", "/work/app"); got != want {
		t.Errorf("work folder resume = %q, want %q", got, want)
	}
	if got, want := byID["prof1"].Resume, wantResume(prof, "prof1", "/work/app"); got != want {
		t.Errorf("profile folder resume = %q, want %q", got, want)
	}

	if got, want := resumeCommand("", "claude", "work1", "", filepath.Join(work, "projects", "-b", "work1.jsonl")), wantResume(work, "work1", ""); got != want {
		t.Errorf("no-cwd resume = %q, want %q", got, want)
	}

	quoteDir := filepath.Join(home, ".claude-o'brien")
	if got, want := resumeCommand("", "claude", "q1", "", filepath.Join(quoteDir, "projects", "-q", "q1.jsonl")), wantResume(quoteDir, "q1", ""); got != want {
		t.Errorf("quoted folder resume = %q, want %q", got, want)
	}

	if got, want := psQuote("C:\\Users\\o'brien\\dev\u2019s\\.claude-work"), "'C:\\Users\\o''brien\\dev\u2019\u2019s\\.claude-work'"; got != want {
		t.Errorf("psQuote smart quotes = %q, want %q", got, want)
	}

	t.Run("symlink_to_default", func(t *testing.T) {
		link := filepath.Join(home, ".claude-link")
		if err := os.Symlink(d.claude, link); err != nil {
			t.Skipf("symlink unsupported: %v", err)
		}
		linkPath := filepath.Join(link, "projects", "-a", "main1.jsonl")
		if got, want := resumeCommand("", "claude", "main1", "/work/app", linkPath), wantResume("", "main1", "/work/app"); got != want {
			t.Errorf("symlink to default folder resume = %q, want %q", got, want)
		}
	})

	wslPath := `\\wsl.localhost\Ubuntu\home\me\.claude\projects\-app\wsl1.jsonl`
	if got := resumeCommand("Ubuntu", "claude", "wsl1", "/home/me/app", wslPath); strings.Contains(got, "CLAUDE_CONFIG_DIR") {
		t.Errorf("WSL resume unexpectedly set CLAUDE_CONFIG_DIR: %q", got)
	}
}
