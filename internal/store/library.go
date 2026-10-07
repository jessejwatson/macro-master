package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"macro-master/internal/macro"
)

// Library is a folder under libraries/. A git repo is a synced library.
type Library struct {
	Name   string
	Path   string
	Synced bool
}

// Ref points at one macro file.
type Ref struct {
	Library string
	Name    string
	Path    string
}

// ID is the "library/name" form used in state and output.
func (r Ref) ID() string { return r.Library + "/" + r.Name }

func (s *Store) LibraryPath(name string) string { return filepath.Join(s.LibrariesDir(), name) }

// Libraries lists library folders, sorted by name.
func (s *Store) Libraries() []Library {
	entries, err := os.ReadDir(s.LibrariesDir())
	if err != nil {
		return nil
	}
	var libs []Library
	for _, e := range entries {
		if !e.IsDir() || macro.ValidateName(e.Name()) != nil {
			continue
		}
		p := s.LibraryPath(e.Name())
		_, gitErr := os.Stat(filepath.Join(p, ".git"))
		libs = append(libs, Library{Name: e.Name(), Path: p, Synced: gitErr == nil})
	}
	return libs
}

// Library returns the named library, or an error if it doesn't exist.
func (s *Store) Library(name string) (Library, error) {
	for _, l := range s.Libraries() {
		if l.Name == name {
			return l, nil
		}
	}
	return Library{}, fmt.Errorf("there is no library called %q; run mm lib ls to see them", name)
}

// CreateLibrary makes a new empty local library.
func (s *Store) CreateLibrary(name string) error {
	if err := macro.ValidateName(name); err != nil {
		return err
	}
	p := s.LibraryPath(name)
	if _, err := os.Stat(p); err == nil {
		return fmt.Errorf("library %q already exists", name)
	}
	return os.Mkdir(p, 0o755)
}

// RemoveLibrary deletes a library folder from this machine and forgets its
// state entries.
func (s *Store) RemoveLibrary(name string) error {
	lib, err := s.Library(name)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(lib.Path); err != nil {
		return err
	}
	return s.Prune()
}

// Macros lists the macros in one library, sorted by name.
func (s *Store) Macros(lib string) []Ref {
	entries, err := os.ReadDir(s.LibraryPath(lib))
	if err != nil {
		return nil
	}
	var refs []Ref
	for _, e := range entries {
		name, ok := strings.CutSuffix(e.Name(), macro.Ext)
		if !ok || e.IsDir() || macro.ValidateName(name) != nil {
			continue
		}
		refs = append(refs, Ref{Library: lib, Name: name, Path: filepath.Join(s.LibraryPath(lib), e.Name())})
	}
	return refs
}

// AllMacros lists every macro, sorted by library then name.
func (s *Store) AllMacros() []Ref {
	var refs []Ref
	for _, l := range s.Libraries() {
		refs = append(refs, s.Macros(l.Name)...)
	}
	return refs
}

// Find returns the macros matching a "name" or "library/name" address.
func (s *Store) Find(addr string) []Ref {
	lib, name, qualified := strings.Cut(addr, "/")
	var out []Ref
	for _, r := range s.AllMacros() {
		if qualified && r.Library == lib && r.Name == name || !qualified && r.Name == addr {
			out = append(out, r)
		}
	}
	return out
}

// Target resolves an address for a new or existing macro: "lib/name", or
// "name" in the default library. The macro need not exist.
func (s *Store) Target(addr string) (Ref, error) {
	lib, name, qualified := strings.Cut(addr, "/")
	if !qualified {
		lib, name = s.Config.DefaultLibrary, addr
		if lib == "" {
			return Ref{}, errors.New("no default library is set; run mm lib default <name>")
		}
	}
	if err := macro.ValidateMacroName(name); err != nil {
		return Ref{}, err
	}
	if _, err := s.Library(lib); err != nil {
		return Ref{}, err
	}
	return Ref{Library: lib, Name: name, Path: filepath.Join(s.LibraryPath(lib), name+macro.Ext)}, nil
}

// Exists reports whether the macro file is present.
func (r Ref) Exists() bool {
	_, err := os.Stat(r.Path)
	return err == nil
}

// Read returns the macro file content.
func (r Ref) Read() (string, error) {
	b, err := os.ReadFile(r.Path)
	return string(b), err
}

// Write saves the macro file, executable so it also runs outside mm.
func (r Ref) Write(content string) error {
	return WriteFileAtomic(r.Path, []byte(content), 0o755)
}

// Suggest returns macro ids whose names are close to name, best first.
func (s *Store) Suggest(name string) []string {
	if _, n, ok := strings.Cut(name, "/"); ok {
		name = n
	}
	type scored struct {
		id string
		d  int
	}
	var hits []scored
	limit := max(2, len(name)/3)
	for _, r := range s.AllMacros() {
		d := levenshtein(name, r.Name)
		if strings.Contains(r.Name, name) || strings.Contains(name, r.Name) {
			d = min(d, 1)
		}
		if d <= limit {
			hits = append(hits, scored{r.ID(), d})
		}
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].d < hits[j].d })
	var out []string
	for _, h := range hits {
		out = append(out, h.id)
		if len(out) == 5 {
			break
		}
	}
	return out
}

func levenshtein(a, b string) int {
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}

// IsFavourite reports whether the macro id ("lib/name") is a favourite.
func (s *Store) IsFavourite(id string) bool { return s.State.IsFavourite(id) }
