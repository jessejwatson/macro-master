package cli

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"macro-master/internal/store"
)

func TestParseVersion(t *testing.T) {
	for in, want := range map[string]bool{"0.1.11": true, "v0.1.12": true, "dev": false, "test": false, "0.1.11-2-gabc1234": false, "0.1.11-dirty": false, "": false} {
		if _, ok := parseVersion(in); ok != want {
			t.Errorf("%q read: %v, want %v", in, ok, want)
		}
	}
	a, _ := parseVersion("0.1.10")
	b, _ := parseVersion("0.1.9")
	if !newer(a, b) || newer(b, a) || newer(a, a) {
		t.Error("0.1.10 should be newer than 0.1.9, and only that")
	}
}

func TestLatestIn(t *testing.T) {
	formula := `  url "https://github.com/jessejwatson/macro-master/archive/refs/tags/v0.1.12.tar.gz"`
	if got := latestIn(tapFormulaURL, []byte(formula)); got != "0.1.12" {
		t.Errorf("formula: %q", got)
	}
	tags := `[{"name":"v0.1.9"},{"name":"v0.1.10"},{"name":"v0.1.2"},{"name":"nightly"}]`
	if got := latestIn(tagsURL, []byte(tags)); got != "0.1.10" {
		t.Errorf("tags: %q", got)
	}
}

func writeUpdateFile(t *testing.T, ta *testApp, f updateFile) {
	t.Helper()
	b, _ := json.Marshal(f)
	if err := store.WriteFileAtomic(updatePath(ta.store.Home), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestAvailableUpdate(t *testing.T) {
	ta := newApp(t)
	ta.run(nil, "lib", "ls") // opens the store
	ta.Version = "0.1.11"
	if latest, stale := ta.availableUpdate(tapFormulaURL); latest != "" || !stale {
		t.Fatalf("never looked: %q stale %v", latest, stale)
	}
	writeUpdateFile(t, ta, updateFile{Checked: time.Now(), Source: tapFormulaURL, Latest: "0.1.12"})
	if latest, stale := ta.availableUpdate(tapFormulaURL); latest != "0.1.12" || stale {
		t.Fatalf("just looked: %q stale %v", latest, stale)
	}
	if latest, stale := ta.availableUpdate(tagsURL); latest != "" || !stale {
		t.Fatalf("looked elsewhere: %q stale %v", latest, stale)
	}
	ta.Version = "0.1.12"
	if latest, _ := ta.availableUpdate(tapFormulaURL); latest != "" {
		t.Fatalf("up to date, but offered %q", latest)
	}
	writeUpdateFile(t, ta, updateFile{Checked: time.Now().Add(-25 * time.Hour), Source: tapFormulaURL, Latest: "0.1.13"})
	if latest, stale := ta.availableUpdate(tapFormulaURL); latest != "0.1.13" || !stale {
		t.Fatalf("a day old: %q stale %v", latest, stale)
	}
}

func TestUpdateCheckWanted(t *testing.T) {
	ta := newApp(t)
	ta.run(nil, "lib", "ls")
	if ta.updateCheckWanted() {
		t.Error("a test build looks for updates")
	}
	ta.Version = "0.1.11"
	ta.env["MM_NO_UPDATE_CHECK"] = "1"
	if ta.updateCheckWanted() {
		t.Error("MM_NO_UPDATE_CHECK is ignored")
	}
	delete(ta.env, "MM_NO_UPDATE_CHECK")
	if !ta.updateCheckWanted() {
		t.Error("a release doesn't look for updates")
	}
}

func TestUpdateShowsInThePicker(t *testing.T) {
	ta := newApp(t)
	ta.Interactive = true
	ta.save(t, "personal/alpha", "#!/bin/sh\necho alpha\n")
	ta.Version = "0.1.11"
	ta.run(nil, "lib", "ls")
	// Tests don't run from Homebrew's Cellar, so the look is at the tags. A
	// fresh look keeps the picker from looking again over the network.
	writeUpdateFile(t, ta, updateFile{Checked: time.Now(), Source: tagsURL, Latest: "0.1.11"})
	ta.run(ans("\n"))
	if strings.Contains(ta.err.String(), "is out") {
		t.Fatalf("shows an update when up to date:\n%s", ta.err)
	}
	writeUpdateFile(t, ta, updateFile{Checked: time.Now(), Source: tagsURL, Latest: "0.1.12"})
	ta.run(ans("\n"))
	if !strings.Contains(ta.err.String(), "● mm 0.1.12 is out: git pull && make install") {
		t.Fatalf("picker:\n%s", ta.err)
	}
	// Running a macro by name doesn't mention it.
	ta.run(nil, "alpha")
	if strings.Contains(ta.err.String(), "is out") {
		t.Fatalf("mm alpha:\n%s", ta.err)
	}
}
