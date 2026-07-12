package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/runbear-io/beardrive/internal/config"
	"github.com/runbear-io/beardrive/internal/store"
)

// Real hook payload shapes from the supported platforms — read-log must find
// the file paths wherever each platform puts them.
func TestExtractEventPaths(t *testing.T) {
	cases := map[string]struct {
		payload string
		want    []string
	}{
		"claude read": {
			`{"session_id":"abc","hook_event_name":"PostToolUse","tool_name":"Read",
			  "tool_input":{"file_path":"/proj/wiki/a.md"},"tool_response":{"type":"text"}}`,
			[]string{"/proj/wiki/a.md"},
		},
		"gemini read_many_files": {
			`{"session_id":"g1","tool":{"name":"read_many_files","args":{"paths":["wiki/a.md","wiki/b.md"]}}}`,
			[]string{"wiki/a.md", "wiki/b.md"},
		},
		"gemini read_file absolute": {
			`{"session_id":"g2","tool":{"name":"read_file","args":{"absolute_path":"/proj/notes.md"}}}`,
			[]string{"/proj/notes.md"},
		},
		"hermes read_file": {
			`{"hook_event_name":"post_tool_call","tool_name":"read_file","tool_args":{"path":"docs/x.md"}}`,
			[]string{"docs/x.md"},
		},
		"duplicates collapse": {
			`{"tool_input":{"file_path":"a.md"},"extra":{"file_path":"a.md"}}`,
			[]string{"a.md"},
		},
		"no paths": {
			`{"session_id":"abc","prompt":"hello"}`,
			nil,
		},
		"not json": {
			`plain text`,
			nil,
		},
	}
	for name, c := range cases {
		got := extractEventPaths([]byte(c.payload))
		if strings.Join(got, ",") != strings.Join(c.want, ",") {
			t.Errorf("%s: paths = %v, want %v", name, got, c.want)
		}
	}
}

// End to end through the cobra command: a claude-style event lands in the
// mount's read spool, filtered to project-relative synced paths.
func TestReadLogCommand(t *testing.T) {
	t.Setenv("BDRIVE_HOME", t.TempDir())
	folder := t.TempDir()
	folder, _ = filepath.EvalSymlinks(folder)
	proj, err := config.SaveProject(folder, config.Project{Volume: "wiki"})
	if err != nil {
		t.Fatal(err)
	}

	c := readLogCmd()
	c.SetIn(bytes.NewReader([]byte(`{"session_id":"abc","tool_name":"Read",
		"tool_input":{"file_path":"` + folder + `/wiki/a.md"},
		"other":{"file_path":"/somewhere/else/entirely.md"}}`)))
	c.SetArgs([]string{folder})
	if err := c.Execute(); err != nil {
		t.Fatal(err)
	}

	vdir, err := config.VolumeDir(proj.ID)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(vdir)
	if err != nil {
		t.Fatal(err)
	}
	evs, err := st.PendingReads()
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].Path != "wiki/a.md" {
		t.Fatalf("spool = %+v, want just the in-project read, mount-relative", evs)
	}
}
