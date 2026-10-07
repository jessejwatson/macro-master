package cli

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"macro-master/internal/gitx"
	"macro-master/internal/macro"
	"macro-master/internal/store"
)

func (a *App) cmdMv(args []string) error {
	if len(args) != 2 {
		return errors.New("usage: mm mv <macro or folder/> <new-name, folder/ or library/path>")
	}
	src, dst := args[0], args[1]
	if strings.HasSuffix(src, "/") || len(a.store.Find(src)) == 0 && len(a.store.FindFolder(src)) > 0 {
		return a.moveFolder(src, dst)
	}
	from, err := a.resolve(src)
	if err != nil {
		return err
	}
	to, err := a.macroDest(from, dst)
	if err != nil {
		return err
	}
	return a.moveMacro(from, to)
}

// cmdRename renames a macro or folder in place.
func (a *App) cmdRename(args []string) error {
	if len(args) != 2 {
		return errors.New("usage: mm rename <macro or folder> <new-name>")
	}
	if strings.Contains(strings.TrimSuffix(args[1], "/"), "/") {
		return errors.New("mm rename takes a plain new name; use mm mv to move things between folders or libraries")
	}
	if l, err := a.store.Library(args[0]); err == nil && len(a.store.Find(args[0])) == 0 && len(a.store.FindFolder(args[0])) == 0 {
		return fmt.Errorf("%s is a library; rename it with mm lib rename %s %s", l.Name, l.Name, args[1])
	}
	return a.cmdMv([]string{args[0], strings.TrimSuffix(args[1], "/")})
}

// splitDir resolves a destination folder: "lib/sub" when the first segment
// is a library, otherwise a folder in base.
func (a *App) splitDir(base, dir string) (lib, folder string) {
	segs := strings.Split(dir, "/")
	if _, err := a.store.Library(segs[0]); err == nil {
		return segs[0], strings.Join(segs[1:], "/")
	}
	return base, dir
}

// macroDest works out where mm mv puts a macro. A plain name renames it in
// place; "folder/" or "lib/" moves it there keeping its name; any other
// path is relative to its own library unless it starts with a library.
func (a *App) macroDest(from store.Ref, dst string) (store.Ref, error) {
	switch {
	case strings.HasSuffix(dst, "/"):
		lib, folder := a.splitDir(from.Library, strings.TrimSuffix(dst, "/"))
		return a.store.RefIn(lib, path.Join(folder, from.Leaf()))
	case !strings.Contains(dst, "/"):
		return a.store.RefIn(from.Library, path.Join(path.Dir(from.Name), dst))
	}
	return a.store.TargetIn(from.Library, dst)
}

// moved is one macro's old and new place, with whether it was trusted.
type moved struct {
	from, to store.Ref
	content  string
	trusted  bool
}

func (a *App) moveMacro(from, to store.Ref) error {
	if to.ID() == from.ID() {
		return errors.New("that's the same place; nothing to do")
	}
	if err := a.checkWritable(from.Library, to.Library); err != nil {
		return err
	}
	a.beforeChange(from.Library, to.Library)
	if !from.Exists() {
		return fmt.Errorf("%s was deleted elsewhere", from.ID())
	}
	if to.Exists() {
		return fmt.Errorf("%s already exists; remove or rename it first", to.ID())
	}
	content, _ := from.Read()
	m := moved{from: from, to: to, content: content, trusted: a.trustStatus(from, content) == ""}
	if err := os.MkdirAll(filepath.Dir(to.Path), 0o755); err != nil {
		return err
	}
	if err := os.Rename(from.Path, to.Path); err != nil {
		return err
	}
	a.store.RemoveEmptyDirs(from.Library, filepath.Dir(from.Path))
	a.commitMove(from.Library, to.Library, from.Name, to.Name, from.Path, to.Path)
	if err := a.store.UpdateState(func(st *store.State) { st.Rename(from.ID(), to.ID()) }); err != nil {
		return err
	}
	a.keepTrust([]moved{m})
	fmt.Fprintf(a.Stderr, "Moved %s to %s.\n", from.ID(), to.ID())
	return nil
}

func (a *App) moveFolder(src, dst string) error {
	folders := a.store.FindFolder(src)
	switch len(folders) {
	case 0:
		if _, err := a.store.Library(strings.TrimSuffix(src, "/")); err == nil {
			return fmt.Errorf("%s is a library; rename it with mm lib rename", strings.TrimSuffix(src, "/"))
		}
		return fmt.Errorf("there is no folder called %q; run mm ls to see them", strings.TrimSuffix(src, "/"))
	case 1:
	default:
		ids := make([]string, len(folders))
		for i, f := range folders {
			ids[i] = f.ID() + "/"
		}
		return fmt.Errorf("%q matches more than one folder; use one of: %s", src, strings.Join(ids, ", "))
	}
	f := folders[0]

	var lib, rel string
	switch trimmed := strings.TrimSuffix(dst, "/"); {
	case strings.HasSuffix(dst, "/"):
		var folder string
		lib, folder = a.splitDir(f.Library, trimmed)
		rel = path.Join(folder, path.Base(f.Path))
	case !strings.Contains(dst, "/"):
		lib, rel = f.Library, path.Join(path.Dir(f.Path), dst)
	default:
		var err error
		if lib, rel, err = a.store.SplitTarget(f.Library, dst); err != nil {
			return err
		}
	}
	if _, err := a.store.Library(lib); err != nil {
		return err
	}
	if err := store.ValidateFolder(rel); err != nil {
		return err
	}
	to := store.Folder{Library: lib, Path: rel}
	if to.ID() == f.ID() {
		return errors.New("that's the same place; nothing to do")
	}
	if lib == f.Library && strings.HasPrefix(rel+"/", f.Path+"/") {
		return errors.New("a folder can't be moved inside itself")
	}

	var items []moved
	deepest := 0
	for _, r := range a.store.Macros(f.Library) {
		inner, ok := strings.CutPrefix(r.Name, f.Path+"/")
		if !ok {
			continue
		}
		deepest = max(deepest, strings.Count(inner, "/"))
		content, _ := r.Read()
		newRef := store.Ref{Library: lib, Name: rel + "/" + inner,
			Path: filepath.Join(a.store.Dir(to), filepath.FromSlash(inner)+macro.Ext)}
		items = append(items, moved{from: r, to: newRef, content: content, trusted: a.trustStatus(r, content) == ""})
	}
	if strings.Count(rel, "/")+1+deepest > store.MaxDepth {
		return fmt.Errorf("that would nest macros more than %d folders deep", store.MaxDepth)
	}
	if err := a.checkWritable(f.Library, lib); err != nil {
		return err
	}
	a.beforeChange(f.Library, lib)
	srcDir, dstDir := a.store.Dir(f), a.store.Dir(to)
	if _, err := os.Stat(srcDir); err != nil {
		return fmt.Errorf("%s/ was deleted elsewhere", f.ID())
	}
	if _, err := os.Stat(dstDir); err == nil {
		return fmt.Errorf("%s/ already exists; pick another name", to.ID())
	}
	if _, err := os.Stat(dstDir + macro.Ext); err == nil {
		return fmt.Errorf("%s is a macro; pick another name", to.ID())
	}
	if err := os.MkdirAll(filepath.Dir(dstDir), 0o755); err != nil {
		return err
	}
	if err := os.Rename(srcDir, dstDir); err != nil {
		return err
	}
	a.store.RemoveEmptyDirs(f.Library, filepath.Dir(srcDir))
	a.commitMove(f.Library, lib, f.Path+"/", rel+"/", srcDir, dstDir)
	if err := a.store.UpdateState(func(st *store.State) { st.RenamePrefix(f.ID(), to.ID()) }); err != nil {
		return err
	}
	a.keepTrust(items)
	fmt.Fprintf(a.Stderr, "Moved folder %s/ to %s/ (%d macro(s)).\n", f.ID(), to.ID(), len(items))
	return nil
}

// commitMove records a move in the synced libraries involved.
func (a *App) commitMove(fromLib, toLib, fromName, toName, fromPath, toPath string) {
	if fromLib == toLib {
		a.afterChange(fromLib, "mm: mv "+fromName+" to "+toName, fromPath, toPath)
		return
	}
	a.afterChange(fromLib, "mm: mv "+fromName+" to "+toLib+"/"+toName, fromPath)
	a.afterChange(toLib, "mm: add "+toName+" (moved from "+fromLib+"/"+fromName+")", toPath)
}

// keepTrust re-trusts moved macros that were trusted before the move, so
// moving your own macros into a shared library doesn't prompt.
func (a *App) keepTrust(items []moved) {
	for _, m := range items {
		if m.trusted {
			a.trust(m.to, m.content)
		}
	}
}

// libRename renames a library on this machine. For a synced library only
// the local name changes; the git repo is untouched.
func (a *App) libRename(oldName, newName string) error {
	lib, err := a.store.Library(oldName)
	if err != nil {
		return err
	}
	if err := macro.ValidateName(newName); err != nil {
		return err
	}
	newPath := a.store.LibraryPath(newName)
	if _, err := os.Stat(newPath); err == nil {
		return fmt.Errorf("library %q already exists", newName)
	}
	if lib.Synced {
		// Wait out any sync, then move the library with its lock.
		r := gitx.Repo{Dir: lib.Path}
		if _, err := store.Lock(r.LockPath(), 10*time.Second, syncLockStale); err != nil {
			return fmt.Errorf("%s is busy syncing; try again shortly", oldName)
		}
		defer os.Remove(gitx.Repo{Dir: newPath}.LockPath())
	}
	if err := os.Rename(lib.Path, newPath); err != nil {
		return err
	}
	conflicts := filepath.Join(a.store.ConflictsDir(), oldName)
	if _, err := os.Stat(conflicts); err == nil {
		os.Rename(conflicts, filepath.Join(a.store.ConflictsDir(), newName))
	}
	if err := a.store.UpdateState(func(st *store.State) { st.RenamePrefix(oldName, newName) }); err != nil {
		return err
	}
	if a.store.Config.DefaultLibrary == oldName {
		if err := a.store.UpdateConfig(func(c *store.Config) { c.DefaultLibrary = newName }); err != nil {
			return err
		}
	}
	msg := fmt.Sprintf("Renamed library %s to %s.", oldName, newName)
	if lib.Synced {
		msg += " Only the name on this machine changed; the git repo is the same."
	}
	fmt.Fprintln(a.Stderr, msg)
	return nil
}
