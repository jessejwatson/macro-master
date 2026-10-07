package store

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func write(t *testing.T, s *Store, rel, content string) {
	t.Helper()
	p := filepath.Join(s.LibrariesDir(), filepath.FromSlash(rel))
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}

func ids(refs []Ref) []string {
	var out []string
	for _, r := range refs {
		out = append(out, r.ID())
	}
	return out
}

func TestScanLibrary(t *testing.T) {
	s := open(t)
	for _, rel := range []string{
		"personal/top.sh",
		"personal/web/deploy.sh",
		"personal/web/api/test.sh",
		"personal/.github/ci.sh",       // hidden: silent
		"personal/docs/build.sh",       // .mmignore'd: silent
		"personal/tools/vendor/x.sh",   // .mmignore'd by path
		"personal/Scripts/Run.sh",      // bad folder name: reported
		"personal/web/Bad.sh",          // bad macro name: reported
		"personal/web/ls.sh",           // reserved: reported
		"personal/a/b/c/d/e/deep.sh",   // depth 5: fine
		"personal/a/b/c/d/e/f/deep.sh", // depth 6: reported
		"personal/notes.txt",           // not a macro: silent
	} {
		write(t, s, rel, "echo\n")
	}
	write(t, s, "personal/.mmignore", "# not macros\ndocs/\ntools/vendor\n")
	sc := s.ScanLibrary("personal")
	want := []string{"personal/a/b/c/d/e/deep", "personal/top", "personal/web/api/test", "personal/web/deploy"}
	if got := ids(sc.Macros); !reflect.DeepEqual(got, want) {
		t.Errorf("macros = %q", got)
	}
	if want := []string{"a", "a/b", "a/b/c", "a/b/c/d", "a/b/c/d/e", "web", "web/api"}; !reflect.DeepEqual(sc.Folders, want) {
		t.Errorf("folders = %q", sc.Folders)
	}
	skipped := strings.Join(sc.Skipped, "\n")
	for _, w := range []string{"Scripts/ (folder names", "web/Bad.sh", "web/ls.sh", "a/b/c/d/e/f/ (folders can only nest 5 deep)"} {
		if !strings.Contains(skipped, w) {
			t.Errorf("skipped missing %q:\n%s", w, skipped)
		}
	}
	if strings.Contains(skipped, "github") || strings.Contains(skipped, "docs") || strings.Contains(skipped, "vendor") {
		t.Errorf("hidden or ignored paths reported:\n%s", skipped)
	}
}

func TestFindSuffixAndExact(t *testing.T) {
	s := open(t)
	s.CreateLibrary("infra")
	write(t, s, "personal/infra/deploy.sh", "echo\n") // folder named like a library
	write(t, s, "infra/deploy.sh", "echo\n")
	write(t, s, "infra/web/deploy.sh", "echo\n")
	write(t, s, "personal/web/build.sh", "echo\n")

	cases := map[string][]string{
		"deploy":                {"infra/deploy", "infra/web/deploy", "personal/infra/deploy"},
		"infra/deploy":          {"infra/deploy"}, // exact full path wins
		"web/deploy":            {"infra/web/deploy"},
		"build":                 {"personal/web/build"},
		"personal/infra/deploy": {"personal/infra/deploy"},
		"eploy":                 nil, // whole segments only
		"../deploy":             nil,
		"web/":                  nil,
	}
	for addr, want := range cases {
		if got := ids(s.Find(addr)); !reflect.DeepEqual(got, want) {
			t.Errorf("Find(%q) = %q, want %q", addr, got, want)
		}
	}
	if got := s.FindFolder("web"); len(got) != 2 {
		t.Errorf("FindFolder(web) = %+v", got)
	}
	if got := s.FindFolder("infra/web/"); len(got) != 1 || got[0].ID() != "infra/web" {
		t.Errorf("FindFolder(infra/web/) = %+v", got)
	}
	short := map[string]string{
		"personal/web/build":    "build",
		"infra/web/deploy":      "web/deploy",
		"infra/deploy":          "infra/deploy",
		"personal/infra/deploy": "personal/infra/deploy",
	}
	for id, want := range short {
		lib, name, _ := strings.Cut(id, "/")
		if got := s.ShortAddr(Ref{Library: lib, Name: name}); got != want {
			t.Errorf("ShortAddr(%s) = %q, want %q", id, got, want)
		}
	}
	// A macro can't take a folder's name.
	if _, err := s.Target("personal/web"); err == nil {
		t.Error("macro named like a folder accepted")
	}
}

func TestRenamePrefix(t *testing.T) {
	st := State{}
	st.init()
	st.Favourites = []string{"lib/web/a", "lib/webx/b", "other/web/a"}
	st.LastRun["lib/web/sub/c"] = st.LastRun["x"]
	st.Trust["lib/web/a"] = TrustEntry{Blob: "1"}
	st.Lib("lib").SyncError = "e"
	st.RenamePrefix("lib/web", "lib/site")
	if !reflect.DeepEqual(st.Favourites, []string{"lib/site/a", "lib/webx/b", "other/web/a"}) {
		t.Errorf("favourites = %q", st.Favourites)
	}
	if _, ok := st.LastRun["lib/site/sub/c"]; !ok {
		t.Errorf("last run = %v", st.LastRun)
	}
	if st.Trust["lib/site/a"].Blob != "1" {
		t.Errorf("trust = %v", st.Trust)
	}
	st.RenamePrefix("lib", "renamed")
	if st.Libraries["renamed"] == nil || st.Libraries["lib"] != nil || !st.IsFavourite("renamed/site/a") {
		t.Errorf("library rename: %+v", st)
	}
}
