package store

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestFirstRun(t *testing.T) {
	s := open(t)
	if s.Config.DefaultLibrary != "personal" {
		t.Errorf("default library = %q", s.Config.DefaultLibrary)
	}
	libs := s.Libraries()
	if len(libs) != 1 || libs[0].Name != "personal" || libs[0].Synced {
		t.Errorf("libraries = %+v", libs)
	}
	// Reopening keeps the user's chosen default.
	s.Config.DefaultLibrary = "other"
	if err := s.SaveConfig(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(s.Home)
	if err != nil {
		t.Fatal(err)
	}
	if s2.Config.DefaultLibrary != "other" {
		t.Errorf("default after reopen = %q", s2.Config.DefaultLibrary)
	}
}

func TestCorruptStateIsBackedUp(t *testing.T) {
	home := t.TempDir()
	if _, err := Open(home); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "state.json"), []byte("{nope"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Open(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Warnings) != 1 || !strings.Contains(s.Warnings[0], "state.json") {
		t.Errorf("warnings = %q", s.Warnings)
	}
	if _, err := os.Stat(filepath.Join(home, "state.json.bak")); err != nil {
		t.Errorf("backup missing: %v", err)
	}
}

func TestFindAndTarget(t *testing.T) {
	s := open(t)
	if err := s.CreateLibrary("infra"); err != nil {
		t.Fatal(err)
	}
	for _, addr := range []string{"deploy", "infra/deploy", "infra/backup"} {
		r, err := s.Target(addr)
		if err != nil {
			t.Fatal(err)
		}
		if err := r.Write("echo " + addr + "\n"); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(s.Find("deploy")); got != 2 {
		t.Errorf("Find(deploy) = %d matches", got)
	}
	if got := s.Find("infra/deploy"); len(got) != 1 || got[0].Library != "infra" {
		t.Errorf("Find(infra/deploy) = %+v", got)
	}
	if got := s.Find("missing"); len(got) != 0 {
		t.Errorf("Find(missing) = %+v", got)
	}
	if r, err := s.Target("nolib/x"); err != nil || r.ID() != "personal/nolib/x" {
		t.Errorf("Target with an unknown first segment should be a folder in the default library: %+v %v", r, err)
	}
	for _, bad := range []string{"../x", "a/../../x", "/x", "a//x", "A/x", "a/b/c/d/e/f/x"} {
		if _, err := s.Target(bad); err == nil {
			t.Errorf("Target(%q) should fail", bad)
		}
	}
	if _, err := s.Target("ls"); err == nil {
		t.Error("Target with reserved name should fail")
	}
	if got := s.Suggest("deplyo"); !reflect.DeepEqual(got, []string{"infra/deploy", "personal/deploy"}) {
		t.Errorf("Suggest = %q", got)
	}
}

func TestStateHelpersAndPrune(t *testing.T) {
	s := open(t)
	r, _ := s.Target("a")
	if err := r.Write("echo a\n"); err != nil {
		t.Fatal(err)
	}
	var on bool
	s.UpdateState(func(st *State) {
		on = st.ToggleFavourite("personal/a")
		st.ToggleFavourite("personal/gone")
		st.Trust["personal/a"] = TrustEntry{Blob: "x"}
		st.Lib("gonelib").SyncError = "x"
	})
	if !on || !s.IsFavourite("personal/a") {
		t.Fatal("toggle on failed")
	}
	s.UpdateState(func(st *State) { st.Rename("personal/a", "personal/b") })
	if s.IsFavourite("personal/a") || !s.IsFavourite("personal/b") || s.State.Trust["personal/b"].Blob != "x" {
		t.Errorf("rename: %+v", s.State)
	}
	s.UpdateState(func(st *State) { st.Rename("personal/b", "personal/a") })
	if err := s.Prune(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(s.State.Favourites, []string{"personal/a"}) || s.State.Libraries["gonelib"] != nil {
		t.Errorf("after prune: %+v", s.State)
	}
	// Another process's write is not lost by our update.
	other, _ := Open(s.Home)
	other.UpdateState(func(st *State) { st.Notices = append(st.Notices, "hello") })
	s.UpdateState(func(st *State) { st.ToggleFavourite("personal/a") })
	if len(s.State.Notices) != 1 || s.IsFavourite("personal/a") {
		t.Errorf("merge: %+v", s.State)
	}
}

func TestLock(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.lock")
	unlock, err := Lock(p, 0, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Lock(p, 100*time.Millisecond, time.Minute); err != ErrLocked {
		t.Errorf("second lock: %v", err)
	}
	unlock()
	if u, err := Lock(p, 0, time.Minute); err != nil {
		t.Errorf("after unlock: %v", err)
	} else {
		u()
	}
	// Stale locks are broken.
	os.WriteFile(p, nil, 0o644)
	old := time.Now().Add(-time.Hour)
	os.Chtimes(p, old, old)
	if u, err := Lock(p, 0, time.Minute); err != nil {
		t.Errorf("stale lock: %v", err)
	} else {
		u()
	}
}

func TestRemoveLibraryPrunes(t *testing.T) {
	s := open(t)
	if err := s.CreateLibrary("tmp"); err != nil {
		t.Fatal(err)
	}
	r, _ := s.Target("tmp/x")
	r.Write("echo\n")
	s.UpdateState(func(st *State) { st.ToggleFavourite(r.ID()) })
	if err := s.RemoveLibrary("tmp"); err != nil {
		t.Fatal(err)
	}
	if len(s.State.Favourites) != 0 {
		t.Errorf("favourites = %q", s.State.Favourites)
	}
	if err := s.CreateLibrary("Bad Name"); err == nil {
		t.Error("invalid library name accepted")
	}
}

func TestJobsConfig(t *testing.T) {
	var c JobsConfig
	if c.NotifyMode() != "desktop" || c.KeepDuration() != 7*24*time.Hour || c.KeepCount() != 50 {
		t.Errorf("defaults: %s %s %d", c.NotifyMode(), c.KeepDuration(), c.KeepCount())
	}
	zero := 0
	c = JobsConfig{Notify: "bell", KeepFor: "1.5d", KeepMax: &zero}
	if c.NotifyMode() != "bell" || c.KeepDuration() != 36*time.Hour || c.KeepCount() != 0 {
		t.Errorf("set: %s %s %d", c.NotifyMode(), c.KeepDuration(), c.KeepCount())
	}
	for in, want := range map[string]time.Duration{"12h": 12 * time.Hour, "0": 0, "junk": 7 * 24 * time.Hour, "-1h": 7 * 24 * time.Hour} {
		if got := (JobsConfig{KeepFor: in}).KeepDuration(); got != want {
			t.Errorf("KeepFor %q = %s, want %s", in, got, want)
		}
	}
	if (JobsConfig{Notify: "loud"}).NotifyMode() != "desktop" {
		t.Error("unknown notify mode not defaulted")
	}

	s := open(t)
	if b, _ := os.ReadFile(filepath.Join(s.Home, "config.json")); strings.Contains(string(b), "jobs") {
		t.Errorf("empty jobs config written: %s", b)
	}
}
