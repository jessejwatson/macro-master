package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func (ta *testApp) isDir(rel string) bool {
	fi, err := os.Stat(filepath.Join(ta.Home, "libraries", filepath.FromSlash(rel)))
	return err == nil && fi.IsDir()
}

func TestFolderAddRunAndLs(t *testing.T) {
	ta := newApp(t)
	ta.clip = "echo deploying"
	if code := ta.run(ans("Ship it\n\n"), "add", "web/deploy"); code != 0 {
		t.Fatalf("add: %s", ta.err)
	}
	if !strings.Contains(ta.err.String(), "Saving personal/web/deploy (creates folder personal/web/)") ||
		!strings.Contains(ta.err.String(), "Run it with: mm deploy") {
		t.Errorf("add output: %s", ta.err)
	}
	ta.save(t, "personal/top", "echo top\n")
	ta.save(t, "personal/web/api/test", "echo api-test\n")
	ta.save(t, "personal/ops/deploy", "echo ops-deploy\n")
	ta.run(nil, "fav", "web/deploy")

	for addr, want := range map[string]string{
		"web/deploy":          "deploying\n",
		"personal/web/deploy": "deploying\n",
		"api/test":            "api-test\n",
		"test":                "api-test\n",
		"ops/deploy":          "ops-deploy\n",
	} {
		if code := ta.run(nil, addr); code != 0 || ta.out.String() != want {
			t.Errorf("mm %s: exit %d, out %q, err %q", addr, code, ta.out, ta.err)
		}
	}
	if code := ta.run(nil, "deploy"); code == 0 || !strings.Contains(ta.err.String(), "personal/ops/deploy, personal/web/deploy") {
		t.Errorf("ambiguous: %s", ta.err)
	}

	ta.run(nil, "ls")
	want := "personal\n" +
		"    top\n" +
		"    ops/\n" +
		"      deploy\n" +
		"    web/\n" +
		"  ★   deploy  Ship it\n" +
		"      api/\n" +
		"        test\n"
	if ta.out.String() != want {
		t.Errorf("ls tree:\n%s\nwant:\n%s", ta.out, want)
	}
	ta.run(nil, "ls", "web")
	if ta.out.String() != "personal/web/\n  ★ deploy  Ship it\n    api/\n      test\n" {
		t.Errorf("ls web:\n%q", ta.out)
	}
	// The picker shows flat full paths.
	ta.Interactive = true
	ta.run(ans("\n"))
	if !strings.Contains(ta.err.String(), "★ personal/web/deploy  Ship it") || !strings.Contains(ta.err.String(), "personal/web/api/test") {
		t.Errorf("picker:\n%s", ta.err)
	}
}

func TestFolderRmCleansUp(t *testing.T) {
	ta := newApp(t)
	ta.save(t, "personal/a/b/only", "echo\n")
	ta.run(ans("y\n"), "rm", "only")
	if ta.isDir("personal/a") {
		t.Error("empty folders left behind")
	}
}

func TestMoveAndRenameMacros(t *testing.T) {
	ta := newApp(t)
	ta.run(nil, "lib", "add", "infra")
	ta.save(t, "personal/deploy", "echo\n")
	ta.run(nil, "fav", "deploy")

	steps := []struct {
		args []string
		want string // full id afterwards
	}{
		{[]string{"mv", "deploy", "web/"}, "personal/web/deploy"},         // into a folder
		{[]string{"rename", "web/deploy", "ship"}, "personal/web/ship"},   // rename in place
		{[]string{"mv", "ship", "ops/release"}, "personal/ops/release"},   // path in own library
		{[]string{"mv", "release", "infra/"}, "infra/release"},            // into a library
		{[]string{"mv", "release", "infra/tools/rel"}, "infra/tools/rel"}, // library path
	}
	for _, st := range steps {
		if code := ta.run(nil, st.args...); code != 0 {
			t.Fatalf("%v: %s", st.args, ta.err)
		}
		if !ta.exists(st.want) || !ta.store.IsFavourite(st.want) {
			t.Fatalf("%v: %s missing or lost its favourite (%q)", st.args, st.want, ta.store.State.Favourites)
		}
	}
	if ta.isDir("personal/web") || ta.isDir("personal/ops") {
		t.Error("emptied folders left behind")
	}
	if code := ta.run(nil, "rename", "rel", "x/y"); code == 0 {
		t.Error("rename accepted a path")
	}
	ta.save(t, "infra/tools/other", "echo\n")
	if code := ta.run(nil, "mv", "rel", "other"); code == 0 {
		t.Error("mv onto an existing macro succeeded")
	}
	ta.save(t, "infra/tools/sub/x", "echo\n")
	if code := ta.run(nil, "rename", "rel", "sub"); code == 0 {
		t.Error("macro renamed over a folder name")
	}
}

func TestMoveAndRenameFolders(t *testing.T) {
	ta := newApp(t)
	ta.run(nil, "lib", "add", "infra")
	ta.save(t, "personal/web/deploy", "echo d\n")
	ta.save(t, "personal/web/api/test", "echo t\n")
	ta.run(nil, "fav", "web/api/test")

	if code := ta.run(nil, "rename", "web", "site"); code != 0 {
		t.Fatalf("rename folder: %s", ta.err)
	}
	if !ta.exists("personal/site/api/test") || ta.isDir("personal/web") || !ta.store.IsFavourite("personal/site/api/test") {
		t.Errorf("after folder rename: %q", ta.store.State.Favourites)
	}
	if !strings.Contains(ta.err.String(), "Moved folder personal/web/ to personal/site/ (2 macro(s))") {
		t.Errorf("message: %s", ta.err)
	}
	if code := ta.run(nil, "mv", "site/", "infra/"); code != 0 {
		t.Fatalf("move folder to library: %s", ta.err)
	}
	if !ta.exists("infra/site/deploy") || !ta.store.IsFavourite("infra/site/api/test") {
		t.Errorf("after cross-library move: %q", ta.store.State.Favourites)
	}
	if code := ta.run(nil, "mv", "site/", "site/api/"); code == 0 || !strings.Contains(ta.err.String(), "inside itself") {
		t.Errorf("move into itself: %s", ta.err)
	}
	if code := ta.run(nil, "mv", "nope/", "x/"); code == 0 || !strings.Contains(ta.err.String(), "no folder") {
		t.Errorf("missing folder: %s", ta.err)
	}
	ta.save(t, "personal/a/b/c/d/e", "echo\n")
	if code := ta.run(nil, "mv", "personal/a/", "infra/site/api/x/y/"); code == 0 || !strings.Contains(ta.err.String(), "deep") {
		t.Errorf("too deep: exit %d %s", code, ta.err)
	}
}

func TestLibRename(t *testing.T) {
	ta := newApp(t)
	ta.save(t, "personal/web/x", "echo\n")
	ta.run(nil, "fav", "x")
	if code := ta.run(nil, "lib", "rename", "personal", "mine"); code != 0 {
		t.Fatalf("lib rename: %s", ta.err)
	}
	if !ta.exists("mine/web/x") || ta.store.Config.DefaultLibrary != "mine" || !ta.store.IsFavourite("mine/web/x") {
		t.Errorf("after rename: default %q favs %q", ta.store.Config.DefaultLibrary, ta.store.State.Favourites)
	}
	if code := ta.run(nil, "rename", "mine", "x"); code == 0 || !strings.Contains(ta.err.String(), "mm lib rename") {
		t.Errorf("rename of a library via mm rename: %s", ta.err)
	}

	// A synced library keeps syncing under its new name.
	remote := newRemote(t, map[string]string{"a.sh": "echo a\n"})
	ta.run(nil, "lib", "add", "team", remote)
	ta.run(nil, "trust", "team/a")
	if code := ta.run(nil, "lib", "rename", "team", "crew"); code != 0 {
		t.Fatalf("synced rename: %s", ta.err)
	}
	if !strings.Contains(ta.err.String(), "the git repo is the same") || ta.store.State.Trust["crew/a"].Blob == "" {
		t.Errorf("synced rename: %s %+v", ta.err, ta.store.State.Trust)
	}
	ta.clip = "echo b"
	ta.run(ans("\n\n"), "add", "crew/b")
	ta.sync(t)
	if !strings.Contains(git(t, remote, "log", "--format=%s", "main"), "mm: add b") {
		t.Error("renamed library no longer pushes")
	}
}

func TestSyncedFoldersAndTrust(t *testing.T) {
	remote := newRemote(t, map[string]string{"top.sh": "echo top\n"})
	a, b := clone(t, remote), clone(t, remote)
	a.clip = "echo nested"
	a.run(ans("\n\n"), "add", "infra/web/deploy")
	a.sync(t)
	b.sync(t)
	if code := b.run(nil, "web/deploy"); code == 0 || !strings.Contains(b.err.String(), "mm trust infra/web/deploy") {
		t.Errorf("untrusted nested macro: %s", b.err)
	}
	b.run(nil, "trust", "web/deploy")
	if code := b.run(nil, "web/deploy"); code != 0 || b.out.String() != "nested\n" {
		t.Errorf("trusted nested: %s", b.err)
	}
	// Moving a folder in a synced library is one commit, and reaches b.
	a.run(nil, "mv", "web/", "site")
	if got := git(t, a.libPath("infra"), "log", "-1", "--format=%s"); strings.TrimSpace(got) != "mm: mv web/ to site/" {
		t.Errorf("commit message %q", got)
	}
	a.sync(t)
	b.sync(t)
	if !b.exists("infra/site/deploy") || b.isDir("infra/web") {
		t.Error("folder move didn't reach b")
	}
}

func TestLibShare(t *testing.T) {
	ta := newApp(t)
	ta.save(t, "personal/web/deploy", "echo d\n")
	ta.save(t, "personal/top", "echo t\n")
	ta.run(nil, "fav", "deploy")

	// A failed share leaves the library untouched.
	if code := ta.run(nil, "lib", "share", "personal", filepath.Join(t.TempDir(), "missing.git")); code == 0 {
		t.Error("share to a missing repo succeeded")
	}
	if ta.isDir("personal/.git") || !ta.exists("personal/web/deploy") {
		t.Error("failed share changed the library")
	}

	remote := newRemote(t, nil)
	if code := ta.run(nil, "lib", "share", "personal", remote); code != 0 {
		t.Fatalf("share: %s", ta.err)
	}
	if !ta.isDir("personal/.git") || !strings.Contains(git(t, remote, "log", "--format=%s", "--all"), "mm: share personal") {
		t.Error("library wasn't pushed")
	}
	if code := ta.run(nil, "deploy"); code != 0 {
		t.Errorf("own macro not trusted after share: %s", ta.err)
	}
	if !ta.store.IsFavourite("personal/web/deploy") {
		t.Error("favourite lost")
	}
	if code := ta.run(nil, "lib", "share", "personal", remote); code == 0 {
		t.Error("shared a synced library twice")
	}

	// Sharing into a repo that already has files merges them in.
	full := newRemote(t, map[string]string{"README.md": "hi\n", "theirs.sh": "echo theirs\n", "mine.sh": "echo old\n"})
	ta.run(nil, "lib", "add", "two")
	ta.save(t, "two/mine", "echo new\n")
	if code := ta.run(nil, "lib", "share", "two", full); code != 0 {
		t.Fatalf("share into non-empty: %s", ta.err)
	}
	if !strings.Contains(ta.err.String(), "the repo already had mine.sh; your version replaced it") {
		t.Errorf("replace note: %s", ta.err)
	}
	if got, _ := os.ReadFile(filepath.Join(ta.Home, "libraries", "two", "mine.sh")); string(got) != "echo new\n" {
		t.Errorf("mine.sh = %q", got)
	}
	if code := ta.run(nil, "theirs"); code == 0 {
		t.Error("a macro that was already in the repo ran without being trusted")
	}
}

func TestCompleteFolders(t *testing.T) {
	ta := newApp(t)
	ta.save(t, "personal/web/deploy", "echo\n")
	ta.save(t, "personal/web/api/test", "echo\n")
	ta.save(t, "infra/web/deploy", "echo\n")
	cases := map[string]string{
		"w":                         "", // "web/deploy" isn't unique, "web/" is
		"personal/":                 "personal/web/",
		"personal/web/":             "personal/web/api/ personal/web/deploy",
		"api/":                      "api/test",
		"mv personal/web/api/test ": "api/ infra/ personal/ web/",
	}
	cases["w"] = "web/"
	for line, want := range cases {
		ta.run(nil, append([]string{"__complete"}, strings.Split(line, " ")...)...)
		if got := strings.Join(strings.Fields(ta.out.String()), " "); got != want {
			t.Errorf("%q: got %q, want %q", line, got, want)
		}
	}
}
