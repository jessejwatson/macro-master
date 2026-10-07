package cli

import (
	"bufio"
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type testApp struct {
	*App
	out, err *bytes.Buffer
	clip     string
	env      map[string]string
	spawned  [][]string // background syncs that would have started
}

// newApp builds an App on a temp home. fzf is hidden so the built-in menu
// is used.
func newApp(t *testing.T) *testApp {
	t.Helper()
	ta := &testApp{out: &bytes.Buffer{}, err: &bytes.Buffer{}, env: map[string]string{}}
	ta.App = &App{
		Version: "test",
		Stdin:   strings.NewReader(""),
		Stdout:  ta.out,
		Stderr:  ta.err,
		Home:    t.TempDir(),
		LookPath: func(name string) (string, error) {
			if name == "fzf" {
				return "", errors.New("not found")
			}
			return exec.LookPath(name)
		},
	}
	ta.ReadClipboard = func() (string, error) { return ta.clip, nil }
	ta.Spawn = func(args []string) error {
		ta.spawned = append(ta.spawned, args)
		return nil
	}
	ta.Getenv = func(k string) string {
		if v, ok := ta.env[k]; ok {
			return v
		}
		return os.Getenv(k)
	}
	return ta
}

// run executes mm with answers fed to prompts (nil answers means no
// terminal) and returns the exit code.
func (ta *testApp) run(answers *string, args ...string) int {
	ta.out.Reset()
	ta.err.Reset()
	ta.Prompts = nil
	if answers != nil {
		ta.Prompts = bufio.NewReader(strings.NewReader(*answers))
	}
	return ta.Run(args)
}

func ans(s string) *string { return &s }

// save writes a macro file directly.
func (ta *testApp) save(t *testing.T, id, content string) {
	t.Helper()
	lib, name, _ := strings.Cut(id, "/")
	dir := filepath.Join(ta.Home, "libraries", lib)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".sh"), []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}

func (ta *testApp) read(t *testing.T, id string) string {
	t.Helper()
	lib, name, _ := strings.Cut(id, "/")
	b, err := os.ReadFile(filepath.Join(ta.Home, "libraries", lib, name+".sh"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func (ta *testApp) exists(id string) bool {
	lib, name, _ := strings.Cut(id, "/")
	_, err := os.Stat(filepath.Join(ta.Home, "libraries", lib, name+".sh"))
	return err == nil
}

func TestAddFromClipboard(t *testing.T) {
	ta := newApp(t)
	ta.clip = "$ ssh {{user:root}}@{{host}}  \r\n"
	if code := ta.run(ans("Log in\n\n"), "add", "login"); code != 0 {
		t.Fatalf("exit %d: %s", code, ta.err)
	}
	want := "#!/usr/bin/env bash\n# description: Log in\nssh {{user:root}}@{{host}}\n"
	if got := ta.read(t, "personal/login"); got != want {
		t.Errorf("file = %q, want %q", got, want)
	}
	for _, s := range []string{"personal/login", "   1  ssh {{user:root}}@{{host}}", `user (default "root"), host`} {
		if !strings.Contains(ta.err.String(), s) {
			t.Errorf("preview missing %q:\n%s", s, ta.err)
		}
	}
	info, _ := os.Stat(filepath.Join(ta.Home, "libraries", "personal", "login.sh"))
	if info.Mode().Perm()&0o100 == 0 {
		t.Error("macro file isn't executable")
	}
}

func TestAddStdinAndOverwrite(t *testing.T) {
	ta := newApp(t)
	ta.run(nil, "lib", "add", "infra")
	ta.Stdin = strings.NewReader("echo one\n")
	if code := ta.run(ans("\ny\n"), "add", "infra/x", "--stdin"); code != 0 {
		t.Fatalf("exit %d: %s", code, ta.err)
	}
	// Declining the overwrite keeps the old file.
	ta.Stdin = strings.NewReader("echo two\n")
	if code := ta.run(ans("\nn\n"), "add", "infra/x", "--stdin"); code != 1 {
		t.Errorf("declined overwrite exit = %d", code)
	}
	if got := ta.read(t, "infra/x"); !strings.Contains(got, "echo one") {
		t.Errorf("file overwritten: %q", got)
	}
	ta.Stdin = strings.NewReader("echo two\n")
	ta.run(ans("\ny\n\n"), "add", "infra/x", "--stdin")
	if got := ta.read(t, "infra/x"); !strings.Contains(got, "echo two") {
		t.Errorf("file not overwritten: %q", got)
	}
}

func TestAddErrors(t *testing.T) {
	ta := newApp(t)
	cases := []struct {
		clip    string
		answers *string
		args    []string
		want    string
	}{
		{"echo", ans("\n\n"), []string{"add", "ls"}, "reserved"},
		{"echo", ans("\n\n"), []string{"add", "Bad"}, "not a valid name"},
		{"  \n", ans("\n\n"), []string{"add", "x"}, "clipboard is empty"},
		{"echo", nil, []string{"add", "x"}, "needs a terminal"},
		{"echo", ans("\n\n"), []string{"add", "nolib/x"}, "no library"},
		{"echo", ans("\n\n"), []string{"add", "x", "--bogus"}, "unknown option"},
	}
	for _, c := range cases {
		ta.clip = c.clip
		if code := ta.run(c.answers, c.args...); code == 0 || !strings.Contains(ta.err.String(), c.want) {
			t.Errorf("%v: exit %d, stderr %q, want %q", c.args, code, ta.err, c.want)
		}
	}
	if ta.exists("personal/x") {
		t.Error("a failed add left a file behind")
	}
}

func TestRunPassesArgsEnvAndExitCode(t *testing.T) {
	ta := newApp(t)
	ta.save(t, "personal/hello", "#!/bin/sh\necho \"$1 $2 $MM_MACRO $MM_LIBRARY\"\nexit 3\n")
	code := ta.run(nil, "hello", "a", "b c")
	if code != 3 {
		t.Errorf("exit = %d, want 3", code)
	}
	if got := ta.out.String(); got != "a b c hello personal\n" {
		t.Errorf("stdout = %q", got)
	}
	if code := ta.run(nil, "run", "hello"); code != 3 {
		t.Errorf("mm run exit = %d", code)
	}
}

func TestRunDefaultsToBashAndRecordsLastRun(t *testing.T) {
	ta := newApp(t)
	ta.save(t, "personal/b", "echo ${BASH_VERSION:+bash}\n")
	if code := ta.run(nil, "b"); code != 0 || ta.out.String() != "bash\n" {
		t.Errorf("exit %d, stdout %q, stderr %q", code, ta.out, ta.err)
	}
	if _, ok := ta.store.State.LastRun["personal/b"]; !ok {
		t.Error("last run not recorded")
	}
}

func TestRunKilledBySignal(t *testing.T) {
	ta := newApp(t)
	ta.save(t, "personal/die", "#!/bin/sh\nkill -TERM $$\n")
	if code := ta.run(nil, "die"); code != 128+15 {
		t.Errorf("exit = %d, want 143", code)
	}
}

func TestPlaceholders(t *testing.T) {
	ta := newApp(t)
	ta.save(t, "personal/greet", "#!/bin/sh\necho \"{{greeting:hi}} {{who}} $1\"\n")

	// name=value fills, other args pass through, and the default is used
	// without a terminal.
	if code := ta.run(nil, "greet", "who=sam", "x"); code != 0 || ta.out.String() != "hi sam x\n" {
		t.Errorf("exit %d, stdout %q, stderr %q", code, ta.out, ta.err)
	}
	// Prompts: Enter takes the default, then a typed value.
	if code := ta.run(ans("\nkim\n"), "greet"); code != 0 || ta.out.String() != "hi kim \n" {
		t.Errorf("prompted: exit %d, stdout %q", code, ta.out)
	}
	if !strings.Contains(ta.err.String(), "greeting [hi]: ") {
		t.Errorf("prompt text: %q", ta.err)
	}
	// Without a terminal a placeholder with no default is an error.
	if code := ta.run(nil, "greet"); code == 0 || !strings.Contains(ta.err.String(), "who=<value>") {
		t.Errorf("missing value: exit %d, stderr %q", code, ta.err)
	}
	// The saved file is untouched.
	if !strings.Contains(ta.read(t, "personal/greet"), "{{who}}") {
		t.Error("placeholder values leaked into the saved file")
	}
}

func TestPrint(t *testing.T) {
	ta := newApp(t)
	ta.save(t, "personal/cd-proj", "#!/usr/bin/env bash\n# description: go\ncd ~/src/{{proj:mm}}\n")
	if code := ta.run(nil, "print", "cd-proj"); code != 0 || ta.out.String() != "cd ~/src/mm\n" {
		t.Errorf("exit %d, stdout %q", code, ta.out)
	}
	if code := ta.run(nil, "print", "cd-proj", "proj=x"); ta.out.String() != "cd ~/src/x\n" {
		t.Errorf("exit %d, stdout %q", code, ta.out)
	}
	if code := ta.run(nil, "print", "cd-proj", "extra"); code == 0 {
		t.Error("print accepted a positional arg")
	}
}

func TestResolveAmbiguousAndMissing(t *testing.T) {
	ta := newApp(t)
	ta.save(t, "personal/deploy", "#!/bin/sh\necho personal\n")
	ta.save(t, "infra/deploy", "#!/bin/sh\necho infra\n")

	if code := ta.run(nil, "deploy"); code == 0 || !strings.Contains(ta.err.String(), "infra/deploy, personal/deploy") {
		t.Errorf("ambiguous: exit %d, stderr %q", code, ta.err)
	}
	if ta.run(nil, "infra/deploy"); ta.out.String() != "infra\n" {
		t.Errorf("qualified: stdout %q", ta.out)
	}
	if code := ta.run(nil, "deplyo"); code == 0 || !strings.Contains(ta.err.String(), "did you mean infra/deploy, personal/deploy?") {
		t.Errorf("missing: exit %d, stderr %q", code, ta.err)
	}

	// Interactively, an ambiguous name opens the picker on the matches.
	ta.Interactive = true
	if code := ta.run(ans("2\n"), "deploy"); code != 0 || ta.out.String() != "personal\n" {
		t.Errorf("picked: exit %d, stdout %q, stderr %q", code, ta.out, ta.err)
	}
}

func TestPickerMenuOrderAndFilter(t *testing.T) {
	ta := newApp(t)
	ta.Interactive = true
	ta.save(t, "personal/alpha", "#!/bin/sh\necho alpha\n")
	ta.save(t, "personal/beta", "#!/bin/sh\n# description: the second\necho beta\n")
	ta.save(t, "personal/gamma", "#!/bin/sh\necho gamma\n")
	ta.run(nil, "fav", "gamma")
	ta.run(nil, "beta")

	// Order: favourite gamma, recently run beta, then alpha. Filter then pick.
	if code := ta.run(ans("second\n1\n")); code != 0 || ta.out.String() != "beta\n" {
		t.Errorf("exit %d, stdout %q, stderr %q", code, ta.out, ta.err)
	}
	menu := ta.err.String()
	g, b, a := strings.Index(menu, "1  ★ personal/gamma"), strings.Index(menu, "2    personal/beta  the second"), strings.Index(menu, "3    personal/alpha")
	if g < 0 || b < 0 || a < 0 {
		t.Errorf("menu order wrong:\n%s", menu)
	}

	if code := ta.run(ans("\n")); code != 130 {
		t.Errorf("cancel exit = %d", code)
	}
	ta.Interactive = false
	if code := ta.run(nil); code == 0 || !strings.Contains(ta.err.String(), "needs a terminal") {
		t.Errorf("non-interactive picker: exit %d, stderr %q", code, ta.err)
	}
}

func TestPickItemsOrder(t *testing.T) {
	ta := newApp(t)
	for _, n := range []string{"a", "b", "c", "d"} {
		ta.save(t, "personal/"+n, "echo\n")
	}
	ta.run(nil, "ls") // opens the store
	now := time.Now()
	ta.store.State.LastRun["personal/c"] = now.Add(-time.Hour)
	ta.store.State.LastRun["personal/d"] = now
	ta.store.State.ToggleFavourite("personal/b")
	var got []string
	for _, it := range ta.pickItems(ta.store.AllMacros()) {
		got = append(got, it.ref.Name)
	}
	if strings.Join(got, "") != "bdca" {
		t.Errorf("order = %v, want b d c a", got)
	}
}

func TestLsFavMvRm(t *testing.T) {
	ta := newApp(t)
	ta.run(nil, "lib", "add", "infra")
	ta.save(t, "personal/one", "# description: first one\necho\n")
	ta.save(t, "personal/two", "echo\n")

	ta.run(nil, "fav", "one")
	ta.run(nil, "ls")
	want := "infra\n    (empty)\n\npersonal\n  ★ one  first one\n    two  \n"
	if ta.out.String() != want {
		t.Errorf("ls = %q, want %q", ta.out, want)
	}
	ta.run(nil, "ls", "--fav")
	if ta.out.String() != "personal\n  ★ one  first one\n" {
		t.Errorf("ls --fav = %q", ta.out)
	}

	// Moving carries the favourite.
	if code := ta.run(nil, "mv", "one", "infra/"); code != 0 {
		t.Fatalf("mv: %s", ta.err)
	}
	if ta.exists("personal/one") || !ta.exists("infra/one") || !ta.store.IsFavourite("infra/one") {
		t.Errorf("mv didn't move file and favourite: %q", ta.store.State.Favourites)
	}
	if code := ta.run(nil, "mv", "two", "one"); code != 0 || !ta.exists("personal/one") {
		t.Errorf("rename: exit %d %s", code, ta.err)
	}
	if code := ta.run(nil, "mv", "personal/one", "infra/"); code == 0 {
		t.Error("mv onto an existing macro succeeded")
	}
	if code := ta.run(nil, "mv", "personal/one", "add"); code == 0 {
		t.Error("mv to a reserved name succeeded")
	}

	// rm asks first.
	if code := ta.run(nil, "rm", "infra/one"); code == 0 {
		t.Error("rm without a terminal succeeded")
	}
	ta.run(ans("n\n"), "rm", "infra/one")
	if !ta.exists("infra/one") {
		t.Error("declined rm deleted the file")
	}
	ta.run(ans("y\n"), "rm", "infra/one")
	if ta.exists("infra/one") || ta.store.IsFavourite("infra/one") {
		t.Error("rm left the file or favourite")
	}
}

func TestEdit(t *testing.T) {
	ta := newApp(t)
	ta.save(t, "personal/e", "echo old\n")
	dir := t.TempDir()
	editor := func(name, script string) string {
		p := filepath.Join(dir, name)
		os.WriteFile(p, []byte("#!/bin/sh\n"+script+"\n"), 0o755)
		return p
	}

	ta.env["EDITOR"] = editor("fail", `echo "echo new" > "$1"; exit 1`)
	if code := ta.run(nil, "edit", "e"); code == 0 || ta.read(t, "personal/e") != "echo old\n" {
		t.Errorf("failed editor saved: exit %d", code)
	}
	ta.env["EDITOR"] = editor("noop", "true")
	if ta.run(nil, "edit", "e"); !strings.Contains(ta.err.String(), "No changes") {
		t.Errorf("unchanged: %q", ta.err)
	}
	ta.env["EDITOR"] = editor("ok", `echo "echo new" > "$1"`)
	if code := ta.run(nil, "edit", "e"); code != 0 || ta.read(t, "personal/e") != "echo new\n" {
		t.Errorf("edit: exit %d, file %q", code, ta.read(t, "personal/e"))
	}
}

func TestShow(t *testing.T) {
	ta := newApp(t)
	ta.save(t, "personal/s", "#!/bin/sh\n# description: Show me\necho {{x:1}}\n")
	ta.run(nil, "show", "s")
	for _, want := range []string{"personal/s\n", "Description:   Show me", `Placeholders:  x (default "1")`, "---\n#!/bin/sh\n"} {
		if !strings.Contains(ta.out.String(), want) {
			t.Errorf("show missing %q:\n%s", want, ta.out)
		}
	}
}

func TestLibCommands(t *testing.T) {
	ta := newApp(t)
	ta.run(nil, "lib", "add", "work")
	ta.run(nil, "lib", "default", "work")
	ta.clip = "echo w"
	ta.run(ans("\n\n"), "add", "w")
	if !ta.exists("work/w") {
		t.Error("add didn't use the new default library")
	}
	ta.run(nil, "lib", "ls")
	if !strings.Contains(ta.out.String(), "work      local  writable  1       default") {
		t.Errorf("lib ls:\n%s", ta.out)
	}
	if code := ta.run(ans("y\n"), "lib", "rm", "work"); code == 0 {
		t.Error("removed the default library")
	}
	ta.run(nil, "lib", "default", "personal")
	ta.run(nil, "fav", "w")
	if code := ta.run(ans("y\n"), "lib", "rm", "work"); code != 0 {
		t.Fatalf("lib rm: %s", ta.err)
	}
	if ta.exists("work/w") || len(ta.store.State.Favourites) != 0 {
		t.Error("lib rm left files or favourites")
	}
	// A failed clone reports git's error and leaves nothing behind.
	if code := ta.run(nil, "lib", "add", "x", filepath.Join(t.TempDir(), "missing.git")); code == 0 || !strings.Contains(ta.err.String(), "couldn't clone") {
		t.Errorf("bad clone: exit %d, %s", code, ta.err)
	}
	if _, err := os.Stat(filepath.Join(ta.Home, "libraries", "x")); err == nil {
		t.Error("failed clone left a folder")
	}
}

func TestPrunesFavouritesOfDeletedLibrary(t *testing.T) {
	ta := newApp(t)
	ta.save(t, "gone/x", "echo\n")
	ta.run(nil, "fav", "x")
	os.RemoveAll(filepath.Join(ta.Home, "libraries", "gone"))
	ta.run(nil, "ls")
	if len(ta.store.State.Favourites) != 0 {
		t.Errorf("favourites = %q", ta.store.State.Favourites)
	}
}

func TestHelpVersionUnknown(t *testing.T) {
	ta := newApp(t)
	if ta.run(nil, "--version"); ta.out.String() != "mm test\n" {
		t.Errorf("version = %q", ta.out)
	}
	if ta.run(nil, "help"); !strings.Contains(ta.out.String(), "inserted exactly as typed") {
		t.Error("help doesn't explain placeholder quoting")
	}
	if code := ta.run(nil, "--nope"); code == 0 {
		t.Error("unknown option accepted")
	}
}
