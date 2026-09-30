package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/runbear-io/beardrive/internal/config"
	"github.com/runbear-io/beardrive/internal/store"
)

const lessonHub = "https://hub.example.com/p/p-12345678"

func runLesson(t *testing.T, args ...string) (string, error) {
	t.Helper()
	c := lessonCmd()
	var out bytes.Buffer
	c.SetOut(&out)
	c.SetErr(&out)
	c.SetArgs(args)
	err := c.Execute()
	return out.String(), err
}

func lessonFile(t *testing.T, root string) string {
	t.Helper()
	dev, err := config.LoadDevice()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(root, "lessons", dev.ID+".md")
}

func TestLessonAppendsOneLine(t *testing.T) {
	t.Setenv("BDRIVE_HOME", t.TempDir())
	parent, _ := filepath.EvalSymlinks(t.TempDir())
	mountAt(t, parent, "wiki", lessonHub)
	root := filepath.Join(parent, "wiki")

	if _, err := runLesson(t, "--folder", root, "use pnpm,\n  never npm"); err != nil {
		t.Fatal(err)
	}
	out, err := runLesson(t, "--folder", root, "--", "- run go vet before pushing")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "lessons/") {
		t.Fatalf("output names no file: %q", out)
	}
	data, err := os.ReadFile(lessonFile(t, root))
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	if strings.Count(got, lessonHeader) != 1 || !strings.HasPrefix(got, lessonHeader) {
		t.Fatalf("header must open the file exactly once:\n%s", got)
	}
	lines := strings.Split(strings.TrimSpace(got), "\n")
	var bullets []string
	for _, l := range lines {
		if strings.HasPrefix(l, "- ") {
			bullets = append(bullets, l)
		}
	}
	if len(bullets) != 2 ||
		!strings.HasPrefix(bullets[0], "- use pnpm, never npm — ") ||
		!strings.HasPrefix(bullets[1], "- run go vet before pushing — ") {
		t.Fatalf("want two one-line lessons, got:\n%s", got)
	}
}

func TestLessonRefuses(t *testing.T) {
	cases := []struct {
		name   string
		text   string
		setup  func(t *testing.T, root string, proj config.Project)
		folder func(root string) string
		want   string
	}{
		{name: "outside a mount", text: "x", folder: func(string) string { return t.TempDir() }, want: "not a beardrive project"},
		{name: "empty text", text: "  \n ", want: "needs some text"},
		{name: "too long", text: strings.Repeat("a", lessonMaxRunes+1), want: "at most"},
		{name: "ignored", text: "x", want: "outside this folder's sync scope", setup: func(t *testing.T, root string, _ config.Project) {
			writeFile(t, filepath.Join(root, ".bdriveignore"), "lessons/\n")
		}},
		{name: "outside --only scope", text: "x", want: "bdrive scope add lessons", setup: func(t *testing.T, root string, _ config.Project) {
			if err := writeScopeDirs(root, []string{"wiki"}); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "read-only folder", text: "x", want: "folder permission", setup: func(t *testing.T, _ string, proj config.Project) {
			seedSync(t, proj, store.SyncState{ReadOnly: []string{"lessons/"}})
		}},
		{name: "denied folder", text: "x", want: "folder permission", setup: func(t *testing.T, _ string, proj config.Project) {
			seedSync(t, proj, store.SyncState{Denied: []string{"lessons/"}})
		}},
		{name: "read-only project", text: "x", want: "read-only", setup: func(t *testing.T, _ string, proj config.Project) {
			seedSync(t, proj, store.SyncState{Access: store.AccessReadOnly})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("BDRIVE_HOME", t.TempDir())
			parent, _ := filepath.EvalSymlinks(t.TempDir())
			proj := mountAt(t, parent, "wiki", lessonHub)
			root := filepath.Join(parent, "wiki")
			if tc.setup != nil {
				tc.setup(t, root, proj)
			}
			folder := root
			if tc.folder != nil {
				folder = tc.folder(root)
			}
			_, err := runLesson(t, "--folder", folder, tc.text)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one naming %q", err, tc.want)
			}
			if _, serr := os.Stat(filepath.Join(root, "lessons")); !os.IsNotExist(serr) {
				t.Fatalf("a refused lesson must write nothing (stat: %v)", serr)
			}
		})
	}
}

func TestLessonMultiMountNeedsFolder(t *testing.T) {
	t.Setenv("BDRIVE_HOME", t.TempDir())
	parent, _ := filepath.EvalSymlinks(t.TempDir())
	mountAt(t, parent, "projA", "https://hub.example.com/p/p-aaaaaaaa")
	mountAt(t, parent, "projB", "https://hub.example.com/p/p-bbbbbbbb")
	_, err := runLesson(t, "--folder", parent, "x")
	if err == nil || !strings.Contains(err.Error(), "--folder") {
		t.Fatalf("err = %v, want a request for --folder", err)
	}
}

func writeFile(t *testing.T, p, s string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

func seedSync(t *testing.T, proj config.Project, st store.SyncState) {
	t.Helper()
	vdir, err := config.VolumeDir(proj.ID)
	if err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(vdir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SaveSync(st); err != nil {
		t.Fatal(err)
	}
}

// writeLessons writes a teammate's lesson file as it would arrive by sync.
func writeLessons(t *testing.T, root, name string, lines ...string) string {
	t.Helper()
	body := lessonHeader + "teammate\n\n"
	for _, l := range lines {
		body += "- " + l + "\n"
	}
	rel := "lessons/" + name + ".md"
	writeFile(t, filepath.Join(root, rel), body)
	return rel
}

func hookContext(t *testing.T, out string) string {
	t.Helper()
	var v struct {
		HookSpecificOutput struct {
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("hook output is not one JSON object: %v\n%s", err, out)
	}
	return v.HookSpecificOutput.AdditionalContext
}

const lessonsSection = "Teammates recorded these corrections"

func TestSyncHookModeLessonsNewLinesOnly(t *testing.T) {
	t.Setenv("BDRIVE_HOME", t.TempDir())
	parent, _ := filepath.EvalSymlinks(t.TempDir())
	proj := mountAt(t, parent, "wiki", lessonHub)
	root := filepath.Join(parent, "wiki")

	rel := writeLessons(t, root, "dev-b", "use pnpm, never npm")
	seedInbound(t, proj, rel)
	got := hookContext(t, runHook(t, root))
	if !strings.Contains(got, `"use pnpm, never npm" (`+"`lessons/dev-b.md`)") {
		t.Fatalf("first turn must carry the lesson:\n%s", got)
	}

	if got := hookContext(t, runHook(t, root)); strings.Contains(got, lessonsSection) {
		t.Fatalf("a lesson is told once:\n%s", got)
	}

	writeLessons(t, root, "dev-b", "use pnpm, never npm", "run go vet first")
	seedInbound(t, proj, rel)
	got = hookContext(t, runHook(t, root))
	if !strings.Contains(got, "run go vet first") || strings.Contains(got, "use pnpm") {
		t.Fatalf("only the new line may be shown:\n%s", got)
	}
}

func TestSyncHookModeLessonsEditAndDelete(t *testing.T) {
	t.Setenv("BDRIVE_HOME", t.TempDir())
	parent, _ := filepath.EvalSymlinks(t.TempDir())
	proj := mountAt(t, parent, "wiki", lessonHub)
	root := filepath.Join(parent, "wiki")

	rel := writeLessons(t, root, "dev-b", "one", "two")
	seedInbound(t, proj, rel)
	runHook(t, root)

	writeLessons(t, root, "dev-b", "one, edited", "two")
	seedInbound(t, proj, rel)
	got := hookContext(t, runHook(t, root))
	if !strings.Contains(got, `"one, edited"`) || strings.Contains(got, `"two"`) {
		t.Fatalf("an edited line is new, an untouched one is not:\n%s", got)
	}

	writeLessons(t, root, "dev-b", "two")
	seedInbound(t, proj, rel)
	if got := hookContext(t, runHook(t, root)); strings.Contains(got, lessonsSection) {
		t.Fatalf("a removed line shows nothing:\n%s", got)
	}
}

func TestSyncHookModeLessonsCap(t *testing.T) {
	t.Setenv("BDRIVE_HOME", t.TempDir())
	parent, _ := filepath.EvalSymlinks(t.TempDir())
	proj := mountAt(t, parent, "wiki", lessonHub)
	root := filepath.Join(parent, "wiki")

	var lines []string
	for i := range 15 {
		lines = append(lines, fmt.Sprintf("lesson %02d", i))
	}
	rel := writeLessons(t, root, "dev-b", lines...)
	seedInbound(t, proj, rel)
	got := hookContext(t, runHook(t, root))
	if n := strings.Count(got, `"lesson `); n != hookLessonsMax || !strings.Contains(got, "+5 more in lessons/") {
		t.Fatalf("want %d lessons and a +5 tail, got %d:\n%s", hookLessonsMax, n, got)
	}
	seedInbound(t, proj, rel)
	if got := hookContext(t, runHook(t, root)); strings.Contains(got, lessonsSection) {
		t.Fatalf("overflow lines are marked seen too:\n%s", got)
	}
}

func TestSyncHookModeLessonsMultipleMounts(t *testing.T) {
	t.Setenv("BDRIVE_HOME", t.TempDir())
	parent, _ := filepath.EvalSymlinks(t.TempDir())
	a := mountAt(t, parent, "projA", "https://hub.example.com/p/p-aaaaaaaa")
	mountAt(t, parent, "projB", "https://hub.example.com/p/p-bbbbbbbb")

	rel := writeLessons(t, filepath.Join(parent, "projA"), "dev-b", "use pnpm")
	seedInbound(t, a, rel)
	got := hookContext(t, runHook(t, parent))
	if !strings.Contains(got, "`projA/lessons/dev-b.md`") || !strings.Contains(got, "live in projA/lessons/.") {
		t.Fatalf("lesson path must carry the mount's prefix:\n%s", got)
	}
}

// A session in a subfolder that does not contain lessons/ still gets the
// lesson — it applies to the whole project — without a path it can't reach.
func TestSyncHookModeLessonsInsideMount(t *testing.T) {
	t.Setenv("BDRIVE_HOME", t.TempDir())
	parent, _ := filepath.EvalSymlinks(t.TempDir())
	proj := mountAt(t, parent, "wiki", lessonHub)
	root := filepath.Join(parent, "wiki")
	if err := os.MkdirAll(filepath.Join(root, "notes"), 0o755); err != nil {
		t.Fatal(err)
	}
	rel := writeLessons(t, root, "dev-b", "use pnpm")
	seedInbound(t, proj, rel)
	got := hookContext(t, runHook(t, filepath.Join(root, "notes")))
	if !strings.Contains(got, `"use pnpm"`) || strings.Contains(got, "dev-b.md") {
		t.Fatalf("want the lesson without an unreachable path:\n%s", got)
	}
}

func TestSyncHookModeLessonsNeedHeader(t *testing.T) {
	t.Setenv("BDRIVE_HOME", t.TempDir())
	parent, _ := filepath.EvalSymlinks(t.TempDir())
	proj := mountAt(t, parent, "wiki", lessonHub)
	root := filepath.Join(parent, "wiki")
	writeFile(t, filepath.Join(root, "lessons/week1.md"), "# Week 1\n\n- photosynthesis\n")
	seedInbound(t, proj, "lessons/week1.md")
	if got := hookContext(t, runHook(t, root)); strings.Contains(got, lessonsSection) {
		t.Fatalf("a file without the lesson header is not a lesson file:\n%s", got)
	}
}

// A turn with no new lessons pays nothing: the context is exactly what it is
// without the feature.
func TestSyncHookModeNoLessonsIsByteIdentical(t *testing.T) {
	t.Setenv("BDRIVE_HOME", t.TempDir())
	parent, _ := filepath.EvalSymlinks(t.TempDir())
	proj := mountAt(t, parent, "wiki", lessonHub)
	root := filepath.Join(parent, "wiki")
	rel := writeLessons(t, root, "dev-b", "use pnpm")
	seedInbound(t, proj, rel)
	runHook(t, root) // shows it, marks it seen

	seedInbound(t, proj, "notes/readme.md", rel)
	got := runHook(t, root)
	var links []hookLink
	links = append(links, hookLink{base: "https://hub.example.com/p-12345678",
		paths: []store.InboundEvent{{Path: "notes/readme.md"}, {Path: rel}}})
	c := syncCmd()
	var want bytes.Buffer
	c.SetOut(&want)
	emitHookContext(c, links)
	if got != want.String() {
		t.Fatalf("quiet-turn context changed:\n got %s\nwant %s", got, want.String())
	}
}
