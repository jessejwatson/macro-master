package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"macro-master/internal/store"
)

// aliases reads the aliases saved in config.json.
func (ta *testApp) aliases(t *testing.T) map[string]store.Alias {
	t.Helper()
	s, err := store.Open(ta.Home)
	if err != nil {
		t.Fatal(err)
	}
	return s.Config.Aliases
}

func TestAliasAddRunRm(t *testing.T) {
	ta := newApp(t)
	ta.save(t, "personal/web/deploy", "echo deploy {{env:dev}} \"$@\"\n")
	if code := ta.run(nil, "alias", "add", "dp", "deploy", "env=prod"); code != 0 {
		t.Fatalf("add: %s", ta.err)
	}
	if got := ta.aliases(t)["dp"]; got.Macro != "personal/web/deploy" || strings.Join(got.Args, " ") != "env=prod" {
		t.Errorf("saved %+v", got)
	}

	// Presets go first, so typed args add to them and can override them.
	for _, args := range [][]string{{"dp", "x"}, {"__alias", "dp", "x"}, {"run", "dp", "x"}} {
		if code := ta.run(nil, args...); code != 0 || ta.out.String() != "deploy prod x\n" {
			t.Errorf("%v: exit %d out %q err %s", args, code, ta.out, ta.err)
		}
	}
	if code := ta.run(nil, "dp", "env=staging"); code != 0 || ta.out.String() != "deploy staging\n" {
		t.Errorf("override: %q %s", ta.out, ta.err)
	}
	if code := ta.run(nil, "print", "dp"); code != 0 || ta.out.String() != "echo deploy prod \"$@\"\n" {
		t.Errorf("print: %q %s", ta.out, ta.err)
	}

	// Aliases show with their macro.
	ta.run(nil, "ls")
	if !strings.Contains(ta.out.String(), "deploy (dp)") {
		t.Errorf("ls: %q", ta.out)
	}
	ta.run(nil, "show", "deploy")
	if !strings.Contains(ta.out.String(), "dp env=prod") {
		t.Errorf("show: %q", ta.out)
	}
	ta.run(nil, "alias", "ls")
	if !strings.Contains(ta.out.String(), "dp     personal/web/deploy  env=prod") {
		t.Errorf("alias ls: %q", ta.out)
	}

	// Re-pointing an alias asks first.
	ta.save(t, "personal/other", "echo other\n")
	if code := ta.run(nil, "alias", "add", "dp", "other"); code == 0 {
		t.Error("replaced without a terminal to ask")
	}
	if code := ta.run(ans("y"), "alias", "add", "dp", "other"); code != 0 || ta.aliases(t)["dp"].Macro != "personal/other" {
		t.Errorf("replace: %d %s", code, ta.err)
	}

	if code := ta.run(nil, "alias", "rm", "dp"); code != 0 {
		t.Fatalf("rm: %s", ta.err)
	}
	if code := ta.run(nil, "dp"); code == 0 {
		t.Error("removed alias still runs")
	}
}

func TestAliasNameChecks(t *testing.T) {
	ta := newApp(t)
	ta.save(t, "personal/web/deploy", "echo\n")
	cases := map[string]string{
		"deploy":  "already runs the macro personal/web/deploy",
		"cd":      "shell builtin",
		"ls":      "word mm uses",
		"mm":      "word mm uses",
		"Big":     "not a valid alias name",
		"9lives":  "not a valid alias name",
		"sh":      "pass --force",
		"web/dep": "not a valid alias name",
	}
	for name, want := range cases {
		if code := ta.run(nil, "alias", "add", name, "deploy"); code == 0 || !strings.Contains(ta.err.String(), want) {
			t.Errorf("%s: exit %d, err %q, want %q", name, code, ta.err, want)
		}
	}
	// A command on PATH can be shadowed on purpose.
	if code := ta.run(nil, "alias", "add", "sh", "deploy", "--force"); code != 0 || !ta.aliases(t)["sh"].Force {
		t.Errorf("--force: %d %s", code, ta.err)
	}
	// A macro added later with the alias's name wins for mm <name>, and
	// mm alias ls says so.
	ta.run(nil, "alias", "add", "dp", "deploy")
	ta.save(t, "personal/dp", "echo macro\n")
	if ta.run(nil, "dp"); ta.out.String() != "macro\n" {
		t.Errorf("mm dp ran %q", ta.out)
	}
	if ta.run(nil, "__alias", "dp"); ta.out.String() != "\n" {
		t.Errorf("the alias ran %q", ta.out)
	}
	ta.run(nil, "alias", "ls")
	if !strings.Contains(ta.out.String(), "mm dp runs personal/dp") {
		t.Errorf("alias ls: %q", ta.out)
	}
}

func TestAliasFollowsMoves(t *testing.T) {
	ta := newApp(t)
	ta.save(t, "personal/web/deploy", "echo hi\n")
	ta.save(t, "personal/web/build", "echo hi\n")
	ta.run(nil, "lib", "add", "ops")
	ta.run(nil, "alias", "add", "d", "deploy")
	ta.run(nil, "alias", "add", "b", "build")

	steps := []struct {
		args []string
		d, b string
	}{
		{[]string{"mv", "deploy", "ship"}, "personal/web/ship", "personal/web/build"},
		{[]string{"mv", "web/", "ops/"}, "ops/web/ship", "ops/web/build"},
		{[]string{"lib", "rename", "ops", "infra"}, "infra/web/ship", "infra/web/build"},
	}
	for _, s := range steps {
		if code := ta.run(nil, s.args...); code != 0 {
			t.Fatalf("%v: %s", s.args, ta.err)
		}
		al := ta.aliases(t)
		if al["d"].Macro != s.d || al["b"].Macro != s.b {
			t.Errorf("after %v: d=%s b=%s", s.args, al["d"].Macro, al["b"].Macro)
		}
	}

	// Deleting a macro takes its aliases with it.
	if code := ta.run(ans("y"), "rm", "ship"); code != 0 || !strings.Contains(ta.err.String(), "alias(es): d") {
		t.Errorf("rm: %d %s", code, ta.err)
	}
	if _, ok := ta.aliases(t)["d"]; ok {
		t.Error("alias kept after rm")
	}

	// A macro deleted some other way leaves a clear error.
	os.Remove(filepath.Join(ta.Home, "libraries", "infra", "web", "build.sh"))
	if code := ta.run(nil, "__alias", "b"); code == 0 || !strings.Contains(ta.err.String(), "doesn't exist any more") {
		t.Errorf("missing target: %d %s", code, ta.err)
	}
	ta.run(nil, "alias", "ls")
	if !strings.Contains(ta.out.String(), "macro missing") {
		t.Errorf("alias ls: %q", ta.out)
	}
}

func TestAliasSuggestions(t *testing.T) {
	ta := newApp(t)
	ta.save(t, "personal/deploy", "# alias: dp env=prod\n# alias: ds env=staging\necho {{env}}\n")
	ta.save(t, "team/up", "# alias: up2\necho up\n")
	ta.save(t, "team/upgrade", "# alias: up2\necho upgrade\n")

	// Suggestions do nothing until accepted.
	if code := ta.run(nil, "dp"); code == 0 {
		t.Error("a suggestion ran before it was accepted")
	}
	ta.run(nil, "show", "deploy")
	if !strings.Contains(ta.out.String(), "mm alias accept dp") {
		t.Errorf("show: %q", ta.out)
	}
	ta.run(nil, "alias", "ls")
	for _, s := range []string{"Suggested by macros", "dp   personal/deploy env=prod", "up2  team/up"} {
		if !strings.Contains(ta.out.String(), s) {
			t.Errorf("alias ls missing %q: %q", s, ta.out)
		}
	}

	if code := ta.run(nil, "alias", "accept", "dp"); code != 0 {
		t.Fatalf("accept: %s", ta.err)
	}
	if ta.run(nil, "dp"); ta.out.String() != "prod\n" {
		t.Errorf("accepted alias ran %q", ta.out)
	}
	if code := ta.run(nil, "alias", "accept", "up2"); code == 0 || !strings.Contains(ta.err.String(), "more than one macro") {
		t.Errorf("ambiguous: %d %s", code, ta.err)
	}
	if code := ta.run(nil, "alias", "accept", "nope"); code == 0 {
		t.Error("unknown suggestion accepted")
	}

	// With no names, each suggestion is asked about.
	if code := ta.run(nil, "alias", "accept"); code == 0 {
		t.Error("asked without a terminal")
	}
	if code := ta.run(ans("y\nn\nn\n"), "alias", "accept"); code != 0 {
		t.Fatalf("accept all: %s", ta.err)
	}
	al := ta.aliases(t)
	if al["ds"].Macro != "personal/deploy" || al["up2"].Macro != "" {
		t.Errorf("aliases %+v", al)
	}

	// A suggestion changing later doesn't change the accepted alias.
	ta.save(t, "personal/deploy", "# alias: dp rm=-rf\necho {{env}}\n")
	if got := ta.aliases(t)["dp"]; strings.Join(got.Args, " ") != "env=prod" {
		t.Errorf("args %v", got.Args)
	}
}

func TestAliasFile(t *testing.T) {
	ta := newApp(t)
	ta.save(t, "personal/deploy", "echo\n")
	file := filepath.Join(ta.Home, "aliases")
	ta.run(nil, "ls")
	if _, err := os.Stat(file); err == nil {
		t.Error("alias file written with no aliases")
	}
	ta.run(nil, "alias", "add", "dp", "deploy")
	ta.run(nil, "alias", "add", "sh", "deploy", "--force")
	b, _ := os.ReadFile(file)
	gen, rest, _ := strings.Cut(string(b), "\n")
	if len(gen) != 12 || rest != "dp\nsh force\n" {
		t.Errorf("alias file %q", b)
	}
	// Hand edits to config.json reach the file at the next mm.
	cfg := filepath.Join(ta.Home, "config.json")
	data, _ := os.ReadFile(cfg)
	os.WriteFile(cfg, []byte(strings.Replace(string(data), `"dp"`, `"dq"`, 1)), 0o644)
	ta.run(nil, "ls")
	b, _ = os.ReadFile(file)
	if g, rest, _ := strings.Cut(string(b), "\n"); g == gen || rest != "dq\nsh force\n" {
		t.Errorf("after edit %q", b)
	}
	ta.run(nil, "alias", "rm", "dq")
	ta.run(nil, "alias", "rm", "sh")
	if _, err := os.Stat(file); err == nil {
		t.Error("alias file kept with no aliases")
	}
}

func TestAliasComplete(t *testing.T) {
	ta := newApp(t)
	ta.save(t, "personal/deploy", "# alias: ship\necho {{host}} {{port:22}}\n")
	ta.run(nil, "alias", "add", "dp", "deploy")
	cases := map[string]string{
		"__alias dp ":         "host= port=",
		"dp ":                 "host= port=",
		"alias ":              "accept add ls rm",
		"alias rm ":           "dp",
		"alias accept ":       "--force ship",
		"alias add x dep":     "deploy",
		"alias add x deploy ": "--force host= port=",
	}
	for line, want := range cases {
		ta.run(nil, append([]string{"__complete"}, strings.Split(line, " ")...)...)
		if got := strings.Join(strings.Fields(ta.out.String()), " "); got != want {
			t.Errorf("%q: got %q, want %q", line, got, want)
		}
	}
}

// TestAliasThroughRealShell loads aliases the way an interactive shell
// does, before a prompt, and runs one.
func TestAliasThroughRealShell(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "mm")
	if out, err := exec.Command("go", "build", "-o", bin, "macro-master/cmd/mm").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	home := t.TempDir()
	os.MkdirAll(filepath.Join(home, "libraries", "personal"), 0o755)
	os.WriteFile(filepath.Join(home, "libraries", "personal", "hi.sh"), []byte("echo hi {{who}} \"$@\"\n"), 0o755)
	os.WriteFile(filepath.Join(home, "libraries", "personal", "goto.sh"), []byte("# mode: source\ncd \"$1\"\n"), 0o755)
	dest := t.TempDir()

	for _, sh := range []string{"bash", "zsh"} {
		if _, err := exec.LookPath(sh); err != nil {
			continue
		}
		os.WriteFile(filepath.Join(home, "config.json"), []byte(`{"default_library":"personal","aliases":{
			"hi":{"macro":"personal/hi","args":["who=there"]},
			"go-to":{"macro":"personal/goto"},
			"taken":{"macro":"personal/hi"}}}`), 0o644)
		// taken is the user's own function, so mm leaves it alone. Loading
		// again with nothing changed is harmless.
		script := `taken() { echo mine; }
eval "$(mm init ` + sh + `)"
__mm_alias_load; __mm_alias_load
hi you
go-to "` + dest + `"; echo "$PWD"
taken
mm alias ls
mm alias rm hi >/dev/null 2>&1
__mm_alias_load
type hi >/dev/null 2>&1 && echo still defined || echo gone`
		args := []string{"--norc", "-c", script}
		if sh == "zsh" {
			args = []string{"-f", "-c", script}
		}
		cmd := exec.Command(sh, args...)
		cmd.Env = append(os.Environ(), "MM_HOME="+home, "NO_COLOR=1", "PATH="+filepath.Dir(bin)+":"+os.Getenv("PATH"))
		out, err := cmd.CombinedOutput()
		resolved, _ := filepath.EvalSymlinks(dest)
		got := string(out)
		for _, want := range []string{"hi there you\n", "mine\n", "name taken in your shell", "gone\n"} {
			if !strings.Contains(got, want) {
				t.Errorf("%s: missing %q (err %v):\n%s", sh, want, err, got)
			}
		}
		if !strings.Contains(got, dest+"\n") && !strings.Contains(got, resolved+"\n") {
			t.Errorf("%s: source-mode alias didn't cd:\n%s", sh, got)
		}
	}
}
