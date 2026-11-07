package edit

import "testing"

// TestSetTOMLTablesKeepsCommentsBeforeTheNextTable: the comments between an
// owned table's last key and the next header introduce that next table (as
// another tool's comment-delimited managed block does in Kimi Code's
// config.toml), so dropping the owned table keeps them (#5614).
func TestSetTOMLTablesKeepsCommentsBeforeTheNextTable(t *testing.T) {
	const foreign = `# >>> other-tool managed block >>>
# DO NOT EDIT
[[hooks]]
event = "Stop"
command = "true"
# <<< other-tool managed block <<<
`
	for _, tc := range []struct {
		name   string
		input  string
		tables []Table
		want   string
	}{
		{
			"replace",
			"default_model = \"x\"\n\n[models.\"magpie/a\"]\nprovider = \"magpie\"\n\n" + foreign,
			[]Table{{Name: `models."magpie/b"`, KVs: []KV{{Path: "provider", Value: "magpie"}}}},
			"default_model = \"x\"\n\n" + foreign + "\n[models.\"magpie/b\"]\nprovider = \"magpie\"\n",
		},
		{
			"remove",
			"default_model = \"x\"\n\n[models.\"magpie/a\"]\nprovider = \"magpie\"\n" + foreign,
			nil,
			"default_model = \"x\"\n\n" + foreign,
		},
		{
			"between two owned tables",
			"[models.\"magpie/a\"]\nprovider = \"magpie\"\n\n# about b\n\n[models.\"magpie/b\"]\nprovider = \"magpie\"\n\n" + foreign,
			nil,
			"# about b\n\n" + foreign,
		},
		{
			"owned table without keys",
			"[keep]\nk = 1\n\n[models.\"magpie/a\"]\n# note for hooks\n[[hooks]]\nevent = \"Stop\"\n",
			nil,
			"[keep]\nk = 1\n\n# note for hooks\n[[hooks]]\nevent = \"Stop\"\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := tmpFile(t, "config.toml", tc.input)
			if err := SetTOMLTables(p, []string{`models."magpie/`}, tc.tables); err != nil {
				t.Fatal(err)
			}
			assertTOMLContent(t, p, tc.want)
		})
	}
}
