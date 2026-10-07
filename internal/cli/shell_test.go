package cli

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"macro-master/internal/hosts"
)

func TestComplete(t *testing.T) {
	ta := newApp(t)
	ta.save(t, "personal/deploy", "echo {{host}} {{port:22}}\n")
	ta.save(t, "infra/deploy", "echo\n")
	ta.save(t, "infra/dns", "echo\n")
	cases := map[string]string{
		"dn":                      "dns",
		"infra/":                  "infra/deploy infra/dns",
		"add ":                    "--stdin infra/ personal/",
		"lib ":                    "add default ls rm",
		"lib rm ":                 "infra personal",
		"init ":                   "bash fish zsh",
		"personal/deploy ":        "host= port=",
		"print personal/deploy p": "port=",
		"mv dns ":                 "infra/ personal/",
		"ls --":                   "--fav",
	}
	for line, want := range cases {
		words := strings.Split(line, " ")
		ta.run(nil, append([]string{"__complete"}, words...)...)
		got := strings.Join(strings.Fields(ta.out.String()), " ")
		if got != want {
			t.Errorf("%q: got %q, want %q", line, got, want)
		}
	}
	// Ambiguous names are only offered qualified.
	ta.run(nil, "__complete", "dep")
	if strings.Contains("\n"+ta.out.String(), "\ndeploy\n") {
		t.Errorf("ambiguous bare name offered: %q", ta.out)
	}
}

func TestShellScripts(t *testing.T) {
	ta := newApp(t)
	for _, sh := range shells {
		if code := ta.run(nil, "init", sh); code != 0 || !strings.Contains(ta.out.String(), "MM_SOURCE_FILE") {
			t.Errorf("init %s: %d %q", sh, code, ta.out)
		}
		if code := ta.run(nil, "completion", sh); code != 0 || !strings.Contains(ta.out.String(), "__complete") {
			t.Errorf("completion %s: %d", sh, code)
		}
	}
	if code := ta.run(nil, "init", "tcsh"); code == 0 {
		t.Error("unknown shell accepted")
	}
	// The scripts at least parse.
	for sh, flag := range map[string]string{"bash": "-n", "zsh": "-n"} {
		if _, err := exec.LookPath(sh); err != nil {
			continue
		}
		for _, kind := range []string{"init", "completion"} {
			ta.run(nil, kind, sh)
			cmd := exec.Command(sh, flag)
			cmd.Stdin = strings.NewReader(ta.out.String())
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Errorf("%s %s doesn't parse: %v\n%s", kind, sh, err, out)
			}
		}
	}
}

func TestSourceModeWritesFile(t *testing.T) {
	ta := newApp(t)
	ta.save(t, "personal/goto", "#!/usr/bin/env bash\n# mode: source\ncd {{dir:/tmp}} && echo \"$1\"\n")
	src := filepath.Join(t.TempDir(), "src")
	ta.env["MM_SOURCE_FILE"] = src
	ta.env["MM_SHELL"] = "zsh"
	if code := ta.run(nil, "goto", "it's"); code != 0 {
		t.Fatalf("exit %d: %s", code, ta.err)
	}
	got, _ := os.ReadFile(src)
	if string(got) != "set -- 'it'\\''s'\ncd /tmp && echo \"$1\"\n" {
		t.Errorf("source file = %q", got)
	}
	ta.env["MM_SHELL"] = "fish"
	ta.run(nil, "goto", `a\b'c`)
	got, _ = os.ReadFile(src)
	if !strings.HasPrefix(string(got), `set argv 'a\\b\'c'`+"\n") {
		t.Errorf("fish source file = %q", got)
	}

	// Without the hook: a hint, then it runs normally.
	ta.env["MM_SOURCE_FILE"] = ""
	if code := ta.run(nil, "goto", "x"); code != 0 || ta.out.String() != "x\n" || !strings.Contains(ta.err.String(), "mm init zsh") {
		t.Errorf("no hook: exit %d out %q err %q", code, ta.out, ta.err)
	}
}

func TestSourceModeThroughRealShell(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash")
	}
	bin := filepath.Join(t.TempDir(), "mm")
	if out, err := exec.Command("go", "build", "-o", bin, "macro-master/cmd/mm").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	home := t.TempDir()
	dest := t.TempDir()
	os.MkdirAll(filepath.Join(home, "libraries", "personal"), 0o755)
	os.WriteFile(filepath.Join(home, "config.json"), []byte(`{"default_library":"personal"}`), 0o644)
	os.WriteFile(filepath.Join(home, "libraries", "personal", "go.sh"), []byte("# mode: source\ncd \"$1\"\nexport MM_TEST_VAR=set\n"), 0o755)
	script := `eval "$(mm init bash)"; mm go "` + dest + `"; echo "$PWD $MM_TEST_VAR"`
	cmd := exec.Command("bash", "--norc", "-c", script)
	cmd.Env = append(os.Environ(), "MM_HOME="+home, "PATH="+filepath.Dir(bin)+":"+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	resolved, _ := filepath.EvalSymlinks(dest)
	if err != nil || !(strings.Contains(string(out), dest+" set") || strings.Contains(string(out), resolved+" set")) {
		t.Errorf("err %v, out %q", err, out)
	}
}

func TestAuth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "token good" {
			w.WriteHeader(401)
			return
		}
		w.Write([]byte(`{"login":"me"}`))
	}))
	defer srv.Close()
	ta := newApp(t)
	ta.Hosts = hosts.NewClient()
	ta.Hosts.Base = func(string) string { return srv.URL }
	ta.Tokens = hosts.NewTokens(filepath.Join(ta.Home, "auth.json"))
	ta.Tokens.Keychain = false
	os.MkdirAll(ta.Home, 0o755)
	os.WriteFile(filepath.Join(ta.Home, "config.json"), []byte(`{"default_library":"personal","hosts":{"forge.test":{"type":"gitea"}}}`), 0o644)

	secret := "bad"
	ta.ReadSecret = func(string) (string, error) { return secret, nil }
	if code := ta.run(ans(""), "auth", "forge.test"); code == 0 || !strings.Contains(ta.err.String(), "rejected that token") {
		t.Errorf("bad token: %d %s", code, ta.err)
	}
	secret = "good"
	if code := ta.run(ans(""), "auth", "forge.test"); code != 0 {
		t.Fatalf("good token: %s", ta.err)
	}
	if v, _ := ta.Tokens.Stored("forge.test"); v != "good" {
		t.Errorf("stored %q", v)
	}
	if code := ta.run(nil, "auth", "forge.test", "--remove"); code != 0 {
		t.Errorf("remove: %s", ta.err)
	}
	if v, _ := ta.Tokens.Stored("forge.test"); v != "" {
		t.Errorf("still stored %q", v)
	}
	if code := ta.run(ans(""), "auth", "https://forge.test/x"); code == 0 {
		t.Error("URL accepted as a host")
	}
}
