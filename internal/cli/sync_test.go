package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"macro-master/internal/store"
)

func TestMain(m *testing.M) {
	// Job tests re-run this binary as mm itself, for helpers and attach.
	if os.Getenv("MM_TEST_MAIN") == "1" {
		os.Exit(NewApp("test").Run(os.Args[1:]))
	}
	// Keep the user's git config (signing, hooks, identity) out of tests.
	os.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	os.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	os.Exit(m.Run())
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=seed", "GIT_AUTHOR_EMAIL=seed@x", "GIT_COMMITTER_NAME=seed", "GIT_COMMITTER_EMAIL=seed@x")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// newRemote makes a bare repo seeded with the given macro files.
func newRemote(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	bare := filepath.Join(dir, "remote.git")
	git(t, dir, "init", "--quiet", "--bare", "-b", "main", bare)
	if len(files) == 0 {
		return bare
	}
	seed := filepath.Join(dir, "seed")
	git(t, dir, "clone", "--quiet", bare, seed)
	for name, content := range files {
		os.WriteFile(filepath.Join(seed, name), []byte(content), 0o755)
	}
	git(t, seed, "add", "-A")
	git(t, seed, "commit", "--quiet", "-m", "seed")
	git(t, seed, "push", "--quiet", "origin", "HEAD:main")
	return bare
}

// clone adds the remote as library "infra" in a fresh app.
func clone(t *testing.T, remote string) *testApp {
	t.Helper()
	ta := newApp(t)
	if code := ta.run(nil, "lib", "add", "infra", remote); code != 0 {
		t.Fatalf("lib add: %s", ta.err)
	}
	return ta
}

func (ta *testApp) sync(t *testing.T) {
	t.Helper()
	if code := ta.run(nil, "sync"); code != 0 {
		t.Fatalf("sync: %s", ta.err)
	}
}

func (ta *testApp) libPath(name string) string { return filepath.Join(ta.Home, "libraries", name) }

func TestCloneTrustAndRun(t *testing.T) {
	remote := newRemote(t, map[string]string{"hello.sh": "#!/bin/sh\n# description: hi\necho shared\n"})
	ta := clone(t, remote)
	if !strings.Contains(ta.err.String(), "Added synced library infra with 1 macro(s)") {
		t.Errorf("lib add: %s", ta.err)
	}
	ta.run(nil, "lib", "ls")
	if !strings.Contains(ta.out.String(), "infra     synced  unknown   1       synced just now") {
		t.Errorf("lib ls:\n%s", ta.out)
	}

	// New shared macro: refused without a terminal, with a hint.
	if code := ta.run(nil, "hello"); code == 0 || !strings.Contains(ta.err.String(), "mm trust infra/hello") {
		t.Errorf("untrusted run: exit %d, %s", code, ta.err)
	}
	// Interactively it's shown in full; N cancels.
	if code := ta.run(ans("\n"), "hello"); code == 0 || ta.out.Len() != 0 {
		t.Errorf("declined: exit %d, out %q", code, ta.out)
	}
	for _, want := range []string{"infra/hello is new.", "Last commit: seed <seed@x>", "   3  echo shared"} {
		if !strings.Contains(ta.err.String(), want) {
			t.Errorf("prompt missing %q:\n%s", want, ta.err)
		}
	}
	// y runs and trusts.
	if code := ta.run(ans("y\n"), "hello"); code != 0 || ta.out.String() != "shared\n" {
		t.Errorf("trusted run: exit %d, out %q, err %s", code, ta.out, ta.err)
	}
	if code := ta.run(nil, "hello"); code != 0 {
		t.Errorf("second run asked again: %s", ta.err)
	}
}

func TestTrustCommandAndChangedDiff(t *testing.T) {
	remote := newRemote(t, map[string]string{"x.sh": "#!/bin/sh\necho one\n"})
	a, b := clone(t, remote), clone(t, remote)
	if code := b.run(nil, "trust", "x"); code != 0 || !strings.Contains(b.err.String(), "Trusted") {
		t.Fatalf("trust: %s", b.err)
	}

	// a edits; its own edit is trusted, and is pushed.
	a.env["EDITOR"] = writeEditor(t, `echo 'echo two' >> "$1"`)
	a.run(ans("y\n"), "trust", "x")
	if code := a.run(nil, "edit", "x"); code != 0 {
		t.Fatalf("edit: %s", a.err)
	}
	if len(a.spawned) == 0 || a.spawned[len(a.spawned)-1][2] != "infra" {
		t.Errorf("edit didn't start a background push: %v", a.spawned)
	}
	if !strings.Contains(git(t, a.libPath("infra"), "log", "-1", "--format=%s %an"), "mm: edit x mm") {
		t.Errorf("commit: %s", git(t, a.libPath("infra"), "log", "-1", "--format=%s %an"))
	}
	if code := a.run(nil, "x"); code != 0 {
		t.Errorf("own edit not trusted: %s", a.err)
	}
	a.sync(t)

	// b pulls and is shown a diff.
	b.sync(t)
	if code := b.run(ans("n\n"), "x"); code == 0 {
		t.Error("changed macro ran without asking")
	}
	if !strings.Contains(b.err.String(), "changed since you last trusted it") || !strings.Contains(b.err.String(), "+echo two") {
		t.Errorf("diff prompt:\n%s", b.err)
	}
	// The picker marks it.
	b.Interactive = true
	b.run(ans("\n"))
	if !strings.Contains(b.err.String(), "infra/x  [changed]") {
		t.Errorf("picker:\n%s", b.err)
	}
}

func writeEditor(t *testing.T, script string) string {
	p := filepath.Join(t.TempDir(), "editor")
	os.WriteFile(p, []byte("#!/bin/sh\n"+script+"\n"), 0o755)
	return p
}

func TestAddPushPullAndHandEdits(t *testing.T) {
	remote := newRemote(t, nil) // empty remote
	a, b := clone(t, remote), clone(t, remote)

	a.clip = "echo from-a"
	if code := a.run(ans("\n\n"), "add", "infra/new"); code != 0 {
		t.Fatalf("add: %s", a.err)
	}
	a.run(nil, "lib", "ls")
	if !strings.Contains(a.out.String(), "1 unpushed") {
		t.Errorf("lib ls before push:\n%s", a.out)
	}
	a.sync(t)
	if !strings.Contains(a.err.String(), "pushed 1 commit") {
		t.Errorf("sync output: %s", a.err)
	}

	b.sync(t)
	if _, err := os.Stat(filepath.Join(b.libPath("infra"), "new.sh")); err != nil {
		t.Fatal("b didn't get the new macro")
	}

	// A file edited by hand is committed on the next sync.
	os.WriteFile(filepath.Join(b.libPath("infra"), "hand.sh"), []byte("echo hand\n"), 0o755)
	b.sync(t)
	if !strings.Contains(b.err.String(), "committed changes made outside mm") {
		t.Errorf("hand edit: %s", b.err)
	}
	a.sync(t)
	if _, err := os.Stat(filepath.Join(a.libPath("infra"), "hand.sh")); err != nil {
		t.Error("hand edit didn't reach a")
	}
}

func TestConflictKeepsRemoteAndSavesLocal(t *testing.T) {
	remote := newRemote(t, map[string]string{"deploy.sh": "echo base\n", "gone.sh": "echo gone\n"})
	a, b := clone(t, remote), clone(t, remote)

	write := func(ta *testApp, name, content string) {
		os.WriteFile(filepath.Join(ta.libPath("infra"), name), []byte(content), 0o755)
	}
	write(a, "deploy.sh", "echo from-a\n")
	os.Remove(filepath.Join(a.libPath("infra"), "gone.sh"))
	a.sync(t)

	write(b, "deploy.sh", "echo from-b\n")
	write(b, "gone.sh", "echo edited-by-b\n") // edit beats a's delete
	b.sync(t)

	got, _ := os.ReadFile(filepath.Join(b.libPath("infra"), "deploy.sh"))
	if string(got) != "echo from-a\n" {
		t.Errorf("deploy.sh = %q, want the remote version", got)
	}
	if !strings.Contains(b.err.String(), "infra/deploy was changed elsewhere; your version is saved at ") {
		t.Fatalf("conflict notice: %s", b.err)
	}
	saved, _ := filepath.Glob(filepath.Join(b.Home, "conflicts", "infra", "deploy.*.sh"))
	if len(saved) != 1 {
		t.Fatalf("saved copies: %v", saved)
	}
	if c, _ := os.ReadFile(saved[0]); string(c) != "echo from-b\n" {
		t.Errorf("saved copy = %q", c)
	}
	if got, _ := os.ReadFile(filepath.Join(b.libPath("infra"), "gone.sh")); string(got) != "echo edited-by-b\n" {
		t.Errorf("gone.sh = %q, want b's edit kept", got)
	}
	// b's result was pushed, so a gets the kept edit.
	a.sync(t)
	if got, _ := os.ReadFile(filepath.Join(a.libPath("infra"), "gone.sh")); string(got) != "echo edited-by-b\n" {
		t.Errorf("a's gone.sh = %q", got)
	}
}

func TestOfflineCommitsArePushedLater(t *testing.T) {
	remote := newRemote(t, map[string]string{"a.sh": "echo a\n"})
	ta := clone(t, remote)
	hidden := remote + ".away"
	os.Rename(remote, hidden)

	ta.clip = "echo offline"
	if code := ta.run(ans("\n\n"), "add", "infra/off"); code != 0 {
		t.Fatalf("offline add: %s", ta.err)
	}
	if !strings.Contains(ta.err.String(), "working offline") {
		t.Errorf("no offline note: %s", ta.err)
	}
	if code := ta.run(nil, "sync"); code == 0 {
		t.Error("sync with no remote succeeded")
	}
	ta.run(nil, "lib", "ls")
	if !strings.Contains(ta.out.String(), "sync failed, run mm sync, 1 unpushed") {
		t.Errorf("lib ls offline:\n%s", ta.out)
	}

	// A background failure leaves a one-line notice for the next command.
	ta.store.UpdateState(func(st *store.State) { st.Lib("infra").SyncError = "" })
	ta.run(nil, "sync", "--background")
	ta.run(nil, "ls")
	if !strings.Contains(ta.err.String(), "infra: sync failed, run mm sync for details") {
		t.Errorf("notice: %s", ta.err)
	}
	ta.run(nil, "ls")
	if strings.Contains(ta.err.String(), "sync failed") {
		t.Error("notice shown twice")
	}

	os.Rename(hidden, remote)
	ta.sync(t)
	if !strings.Contains(git(t, remote, "log", "--format=%s", "main"), "mm: add off") {
		t.Error("offline commit never reached the remote")
	}
}

func TestPushRejectionMarksReadOnly(t *testing.T) {
	remote := newRemote(t, map[string]string{"a.sh": "echo a\n"})
	hook := filepath.Join(remote, "hooks", "pre-receive")
	os.WriteFile(hook, []byte("#!/bin/sh\necho 'no pushes here' >&2\nexit 1\n"), 0o755)
	ta := clone(t, remote)

	ta.clip = "echo mine"
	ta.run(ans("\n\n"), "add", "infra/mine")
	ta.run(nil, "sync")
	if !strings.Contains(ta.err.String(), "infra is read-only for you (pre-receive hook declined); your change is saved at ") {
		t.Fatalf("rejection notice: %s", ta.err)
	}
	if _, err := os.Stat(filepath.Join(ta.libPath("infra"), "mine.sh")); err == nil {
		t.Error("rejected change still in the library")
	}
	if saved, _ := filepath.Glob(filepath.Join(ta.Home, "conflicts", "infra", "mine.*.sh")); len(saved) != 1 {
		t.Errorf("saved copies: %v", saved)
	}
	// Further changes are refused up front; favourites still work.
	if code := ta.run(ans("\n\n"), "add", "infra/again"); code == 0 || !strings.Contains(ta.err.String(), "read-only for you") {
		t.Errorf("add on read-only: exit %d, %s", code, ta.err)
	}
	if code := ta.run(nil, "fav", "infra/a"); code != 0 {
		t.Errorf("fav on read-only: %s", ta.err)
	}
	ta.run(nil, "lib", "ls")
	if !strings.Contains(ta.out.String(), "read-only (pre-receive hook declined)") {
		t.Errorf("lib ls:\n%s", ta.out)
	}
}

func TestReadOnlyLibraryResetsLocalEdits(t *testing.T) {
	remote := newRemote(t, map[string]string{"a.sh": "echo a\n"})
	ta := clone(t, remote)
	ta.store.UpdateState(func(st *store.State) {
		ls := st.Lib("infra")
		ls.Access, ls.AccessFromPush = store.AccessReadOnly, true
	})
	os.WriteFile(filepath.Join(ta.libPath("infra"), "a.sh"), []byte("echo hacked\n"), 0o755)
	ta.sync(t)
	if got, _ := os.ReadFile(filepath.Join(ta.libPath("infra"), "a.sh")); string(got) != "echo a\n" {
		t.Errorf("a.sh = %q, want reset", got)
	}
	if !strings.Contains(ta.err.String(), "local changes were undone; your change is saved at") {
		t.Errorf("notice: %s", ta.err)
	}
}

func TestBackgroundSyncScheduling(t *testing.T) {
	remote := newRemote(t, map[string]string{"a.sh": "echo a\n"})
	ta := clone(t, remote)
	ta.spawned = nil
	ta.run(nil, "ls")
	if len(ta.spawned) != 0 {
		t.Errorf("fresh library synced again: %v", ta.spawned)
	}
	ta.store.UpdateState(func(st *store.State) {
		st.Lib("infra").LastPull = time.Now().Add(-time.Hour)
		st.Lib("infra").LastAttempt = time.Time{}
	})
	ta.run(nil, "__complete", "")
	if len(ta.spawned) != 0 {
		t.Error("completion started a sync")
	}
	ta.run(nil, "ls")
	if len(ta.spawned) != 1 || strings.Join(ta.spawned[0], " ") != "sync --background infra" {
		t.Errorf("spawned: %v", ta.spawned)
	}
	ta.run(nil, "ls")
	if len(ta.spawned) != 1 {
		t.Error("a second sync started while the first was pending")
	}
}
