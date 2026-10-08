package cli

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var ansiRE = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func plain(s string) string { return ansiRE.ReplaceAllString(s, "") }

func styledApp(t *testing.T) *testApp {
	t.Helper()
	ta := newApp(t)
	ta.StyleOut, ta.StyleErr, ta.Width = true, true, 80
	ta.save(t, "personal/top", "echo top\n")
	ta.save(t, "personal/web/deploy", "# description: Ship it\nrsync {{host:web1}}\n")
	ta.save(t, "personal/web/api/test", "curl x\n")
	return ta
}

func TestStyledLsTree(t *testing.T) {
	ta := styledApp(t)
	ta.run(nil, "fav", "deploy")
	if code := ta.run(nil, "ls"); code != 0 {
		t.Fatalf("exit %d: %s", code, ta.err)
	}
	if !ansiRE.MatchString(ta.out.String()) {
		t.Error("ls isn't coloured")
	}
	want := "◆ personal  3 macros\n" +
		"├── top\n" +
		"╰── web/\n" +
		"    ├── ★ deploy  Ship it\n" +
		"    ╰── api/\n" +
		"        ╰── test\n"
	if got := plain(ta.out.String()); got != want {
		t.Errorf("ls:\n%s\nwant:\n%s", got, want)
	}
}

func TestStyledShowAndLibLs(t *testing.T) {
	ta := styledApp(t)
	if code := ta.run(nil, "show", "deploy"); code != 0 {
		t.Fatalf("exit %d: %s", code, ta.err)
	}
	got := plain(ta.out.String())
	for _, s := range []string{"╭", "personal/web/deploy", "Description   Ship it", `host (default "web1")`, "2 rsync {{host:web1}}", "╯"} {
		if !strings.Contains(got, s) {
			t.Errorf("show missing %q:\n%s", s, got)
		}
	}
	ta.run(nil, "lib", "ls")
	if got := plain(ta.out.String()); !strings.Contains(got, "│ ◆ personal │ local │ writable │      3 │ default │") {
		t.Errorf("lib ls:\n%s", got)
	}
}

func TestStyledHelpAndMessages(t *testing.T) {
	ta := styledApp(t)
	ta.run(nil, "help")
	if plain(ta.out.String()) != helpText {
		t.Error("styling changed the help text itself")
	}
	if !strings.Contains(ta.out.String(), "\x1b[36mmm add ") {
		t.Errorf("help commands aren't coloured:\n%q", ta.out.String()[:400])
	}
	ta.run(nil, "nope-nothing")
	if got := plain(ta.err.String()); !strings.HasPrefix(got, "✗ error: ") {
		t.Errorf("error = %q", got)
	}
	ta.clip = "echo {{who}}\n"
	if code := ta.run(ans("Greets\n\n"), "add", "hi"); code != 0 {
		t.Fatalf("add exit %d: %s", code, ta.err)
	}
	got := plain(ta.err.String())
	for _, s := range []string{"Saving personal/hi from the clipboard", "1 echo {{who}}", "Placeholders: who", "? Description (optional, Enter to skip): ", "? Save? [Y/n]", "✓ Saved personal/hi. Run it with: mm hi"} {
		if !strings.Contains(got, s) {
			t.Errorf("add missing %q:\n%s", s, got)
		}
	}
}

func TestPreviewCommand(t *testing.T) {
	ta := newApp(t)
	p := filepath.Join(t.TempDir(), "m.sh")
	os.WriteFile(p, []byte("# hi\necho {{x}}\n"), 0o644)
	if code := ta.run(nil, "__preview", p); code != 0 {
		t.Fatalf("exit %d: %s", code, ta.err)
	}
	if got := plain(ta.out.String()); got != "  1 # hi\n  2 echo {{x}}\n" || !ansiRE.MatchString(ta.out.String()) {
		t.Errorf("preview = %q", ta.out)
	}
}
