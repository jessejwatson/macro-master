package store

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"macro-master/internal/macro"
)

// MaxDepth is how many folder levels a library may nest macros in.
const MaxDepth = 5

// IgnoreFile lists paths in a library that aren't macros, one pattern a
// line, like a simplified .gitignore.
const IgnoreFile = ".mmignore"

// Library is a folder under libraries/. A git repo is a synced library.
type Library struct {
	Name   string
	Path   string
	Synced bool
}

// Ref points at one macro file. Name is its path within the library,
// without the extension, e.g. "web/deploy".
type Ref struct {
	Library string
	Name    string
	Path    string
}

// ID is the full "library/folder/name" path used in state and output.
func (r Ref) ID() string { return r.Library + "/" + r.Name }

// Leaf is the macro's own name, without folders.
func (r Ref) Leaf() string { return path.Base(r.Name) }

// Folder is a folder inside a library. Path is relative, e.g. "web/api".
type Folder struct {
	Library string
	Path    string
}

// ID is the full "library/folder" path.
func (f Folder) ID() string { return f.Library + "/" + f.Path }

// Dir is the folder on disk.
func (s *Store) Dir(f Folder) string {
	return filepath.Join(s.LibraryPath(f.Library), filepath.FromSlash(f.Path))
}

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

// Scan is the result of walking one library.
type Scan struct {
	Macros  []Ref
	Folders []string // relative paths of folders that hold macros
	// Skipped lists .sh files (and folders holding them) that mm can't use,
	// with the reason.
	Skipped []string
}

// ScanLibrary walks a library for macros: .sh files with valid names, in
// valid folders up to MaxDepth deep. Hidden folders and paths matched by
// .mmignore are skipped silently.
func (s *Store) ScanLibrary(lib string) Scan {
	root := s.LibraryPath(lib)
	ignore := readIgnore(filepath.Join(root, IgnoreFile))
	var sc Scan
	folders := map[string]bool{}
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == root {
			return nil
		}
		rel := filepath.ToSlash(strings.TrimPrefix(p, root+string(filepath.Separator)))
		if strings.HasPrefix(d.Name(), ".") || ignore.match(rel, d.IsDir()) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		depth := strings.Count(rel, "/")
		if d.IsDir() {
			switch {
			case macro.ValidateName(d.Name()) != nil:
				if hasScripts(p) {
					sc.Skipped = append(sc.Skipped, rel+"/ (folder names must be lowercase letters, digits, - and _)")
				}
				return filepath.SkipDir
			case depth+1 > MaxDepth:
				if hasScripts(p) {
					sc.Skipped = append(sc.Skipped, fmt.Sprintf("%s/ (folders can only nest %d deep)", rel, MaxDepth))
				}
				return filepath.SkipDir
			}
			return nil
		}
		name, ok := strings.CutSuffix(rel, macro.Ext)
		if !ok || !d.Type().IsRegular() {
			return nil
		}
		if err := macro.ValidateMacroName(path.Base(name)); err != nil {
			sc.Skipped = append(sc.Skipped, rel+" (macro names must be lowercase letters, digits, - and _, and not an mm command)")
			return nil
		}
		sc.Macros = append(sc.Macros, Ref{Library: lib, Name: name, Path: p})
		for dir := path.Dir(name); dir != "."; dir = path.Dir(dir) {
			folders[dir] = true
		}
		return nil
	})
	for f := range folders {
		sc.Folders = append(sc.Folders, f)
	}
	sort.Strings(sc.Folders)
	sort.Slice(sc.Macros, func(i, j int) bool { return sc.Macros[i].Name < sc.Macros[j].Name })
	return sc
}

// hasScripts reports whether a folder contains any .sh file.
func hasScripts(dir string) bool {
	found := false
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(p, macro.Ext) {
			found = true
			return filepath.SkipAll
		}
		return nil
	})
	return found
}

type ignoreList []string

func readIgnore(file string) ignoreList {
	f, err := os.Open(file)
	if err != nil {
		return nil
	}
	defer f.Close()
	var pats ignoreList
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if l := strings.TrimSpace(sc.Text()); l != "" && !strings.HasPrefix(l, "#") {
			pats = append(pats, l)
		}
	}
	return pats
}

// match applies patterns gitignore-style: "docs/" matches folders only, a
// pattern with a "/" is matched against the whole path from the library
// root, and one without against each name.
func (ig ignoreList) match(rel string, isDir bool) bool {
	for _, p := range ig {
		dirOnly := strings.HasSuffix(p, "/")
		p = strings.TrimSuffix(p, "/")
		if dirOnly && !isDir {
			continue
		}
		if strings.Contains(p, "/") {
			if ok, _ := path.Match(strings.TrimPrefix(p, "/"), rel); ok {
				return true
			}
		} else if ok, _ := path.Match(p, path.Base(rel)); ok {
			return true
		}
	}
	return false
}

// Macros lists the macros in one library, sorted by path.
func (s *Store) Macros(lib string) []Ref { return s.ScanLibrary(lib).Macros }

// AllMacros lists every macro, sorted by library then path.
func (s *Store) AllMacros() []Ref {
	var refs []Ref
	for _, l := range s.Libraries() {
		refs = append(refs, s.Macros(l.Name)...)
	}
	return refs
}

// AllFolders lists every folder that holds macros.
func (s *Store) AllFolders() []Folder {
	var out []Folder
	for _, l := range s.Libraries() {
		for _, f := range s.ScanLibrary(l.Name).Folders {
			out = append(out, Folder{Library: l.Name, Path: f})
		}
	}
	return out
}

// segments splits an address into valid path segments, or nil.
func segments(addr string) []string {
	segs := strings.Split(addr, "/")
	for _, s := range segs {
		if macro.ValidateName(s) != nil {
			return nil
		}
	}
	return segs
}

// matchPath reports whether id equals addr (exact) or ends with it on
// whole segments (suffix).
func matchPath(id string, addr []string) (exact, suffix bool) {
	full := strings.Split(id, "/")
	if len(full) < len(addr) {
		return false, false
	}
	tail := full[len(full)-len(addr):]
	for i := range addr {
		if tail[i] != addr[i] {
			return false, false
		}
	}
	return len(full) == len(addr), true
}

// Find returns the macros an address points at. Libraries and folders form
// one path, "library/folder/name"; an address matches any macro whose path
// ends with it, and an exact full path wins over those.
func (s *Store) Find(addr string) []Ref {
	segs := segments(addr)
	if segs == nil {
		return nil
	}
	var exact, suffix []Ref
	for _, r := range s.AllMacros() {
		e, ok := matchPath(r.ID(), segs)
		switch {
		case e:
			exact = append(exact, r)
		case ok:
			suffix = append(suffix, r)
		}
	}
	if len(exact) > 0 {
		return exact
	}
	return suffix
}

// FindFolder returns the folders an address points at, matched like Find.
func (s *Store) FindFolder(addr string) []Folder {
	segs := segments(strings.TrimSuffix(addr, "/"))
	if segs == nil {
		return nil
	}
	var exact, suffix []Folder
	for _, f := range s.AllFolders() {
		e, ok := matchPath(f.ID(), segs)
		switch {
		case e:
			exact = append(exact, f)
		case ok:
			suffix = append(suffix, f)
		}
	}
	if len(exact) > 0 {
		return exact
	}
	return suffix
}

// ShortAddr is the shortest address that finds only this macro.
func (s *Store) ShortAddr(r Ref) string {
	full := strings.Split(r.ID(), "/")
	for n := 1; n < len(full); n++ {
		addr := strings.Join(full[len(full)-n:], "/")
		if m := s.Find(addr); len(m) == 1 && m[0].ID() == r.ID() {
			return addr
		}
	}
	return r.ID()
}

// Target resolves the address of a new macro, for mm add: "lib/folders/name"
// when the first segment is a library, otherwise "folders/name" in the
// default library. The macro and its folders need not exist.
func (s *Store) Target(addr string) (Ref, error) {
	return s.TargetIn(s.Config.DefaultLibrary, addr)
}

// TargetIn is Target relative to the library base instead of the default
// library, for mm mv.
func (s *Store) TargetIn(base, addr string) (Ref, error) {
	lib, rel, err := s.SplitTarget(base, addr)
	if err != nil {
		return Ref{}, err
	}
	return s.RefIn(lib, rel)
}

// SplitTarget splits addr into a library and a path within it: the first
// segment when it names a library, otherwise base.
func (s *Store) SplitTarget(base, addr string) (lib, rel string, err error) {
	segs := strings.Split(addr, "/")
	if len(segs) > 1 {
		if _, err := s.Library(segs[0]); err == nil {
			return segs[0], strings.Join(segs[1:], "/"), nil
		}
	}
	if base == "" {
		return "", "", errors.New("no default library is set; run mm lib default <name>")
	}
	return base, addr, nil
}

// RefIn builds a Ref for rel in lib, checking every segment.
func (s *Store) RefIn(lib, rel string) (Ref, error) {
	if _, err := s.Library(lib); err != nil {
		return Ref{}, err
	}
	segs := strings.Split(rel, "/")
	if slices.Contains(segs[:len(segs)-1], "") {
		return Ref{}, fmt.Errorf("%q is not a valid path; use folder/name", rel)
	}
	if err := ValidateFolder(strings.Join(segs[:len(segs)-1], "/")); err != nil {
		return Ref{}, err
	}
	if err := macro.ValidateMacroName(segs[len(segs)-1]); err != nil {
		return Ref{}, err
	}
	p := filepath.Join(s.LibraryPath(lib), filepath.FromSlash(rel)+macro.Ext)
	if fi, err := os.Stat(strings.TrimSuffix(p, macro.Ext)); err == nil && fi.IsDir() {
		return Ref{}, fmt.Errorf("%s/%s is a folder; pick another name", lib, rel)
	}
	return Ref{Library: lib, Name: rel, Path: p}, nil
}

// ValidateFolder checks a relative folder path ("" is the library root).
func ValidateFolder(rel string) error {
	if rel == "" {
		return nil
	}
	segs := strings.Split(rel, "/")
	for _, seg := range segs {
		if err := macro.ValidateName(seg); err != nil {
			return fmt.Errorf("folder %v", err)
		}
	}
	if len(segs) > MaxDepth {
		return fmt.Errorf("folders can only nest %d deep", MaxDepth)
	}
	return nil
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

// Write saves the macro file, executable so it also runs outside mm,
// creating its folders.
func (r Ref) Write(content string) error {
	if err := os.MkdirAll(filepath.Dir(r.Path), 0o755); err != nil {
		return err
	}
	return WriteFileAtomic(r.Path, []byte(content), 0o755)
}

// RemoveEmptyDirs deletes dir and its parents, up to the library root, while
// they're empty. Git doesn't track empty folders, so leaving them would
// make machines disagree.
func (s *Store) RemoveEmptyDirs(lib, dir string) {
	root := s.LibraryPath(lib)
	for dir != root && strings.HasPrefix(dir, root+string(filepath.Separator)) {
		if os.Remove(dir) != nil { // fails unless empty
			return
		}
		dir = filepath.Dir(dir)
	}
}

// Suggest returns macro ids whose names are close to name, best first.
func (s *Store) Suggest(name string) []string {
	name = path.Base(name)
	type scored struct {
		id string
		d  int
	}
	var hits []scored
	limit := max(2, len(name)/3)
	for _, r := range s.AllMacros() {
		leaf := r.Leaf()
		d := levenshtein(name, leaf)
		if strings.Contains(leaf, name) || strings.Contains(name, leaf) {
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

// IsFavourite reports whether the macro id is a favourite.
func (s *Store) IsFavourite(id string) bool { return s.State.IsFavourite(id) }

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
