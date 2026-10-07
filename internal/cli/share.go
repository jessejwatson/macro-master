package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"macro-master/internal/gitx"
	"macro-master/internal/store"
)

// libShare turns a local library into a synced one backed by url. The
// shared copy is built in a temporary clone and only swapped in once the
// push succeeds, so a failure leaves the library exactly as it was.
func (a *App) libShare(name, url string) error {
	lib, err := a.store.Library(name)
	if err != nil {
		return err
	}
	if lib.Synced {
		return fmt.Errorf("%s is already a synced library", name)
	}
	if !a.gitOK() {
		return errors.New("git isn't installed; install it to share a library")
	}
	work := filepath.Join(a.store.LibrariesDir(), "."+name+".sharing")
	os.RemoveAll(work)
	defer os.RemoveAll(work)

	fmt.Fprintf(a.Stderr, "Cloning %s...\n", url)
	ctx, cancel := gitx.WithTimeout(5 * time.Minute)
	defer cancel()
	if err := gitx.Clone(ctx, url, work); err != nil {
		return fmt.Errorf("couldn't clone %s, so %s is unchanged: %v", url, name, err)
	}

	// Copy the library in. Its own version wins over a file of the same
	// name already in the repo; the repo's history keeps the old one.
	var copied, replaced []string
	err = filepath.WalkDir(lib.Path, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !d.Type().IsRegular() {
			return err
		}
		rel, _ := filepath.Rel(lib.Path, p)
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		info, _ := d.Info()
		dest := filepath.Join(work, rel)
		if old, err := os.ReadFile(dest); err == nil && !bytes.Equal(old, data) {
			replaced = append(replaced, filepath.ToSlash(rel))
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(dest, data, info.Mode().Perm()); err != nil {
			return err
		}
		copied = append(copied, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return fmt.Errorf("couldn't copy %s: %v", name, err)
	}
	r := gitx.Repo{Dir: work}
	if _, err := r.Commit("mm: share " + name); err != nil {
		return fmt.Errorf("couldn't commit %s: %v", name, err)
	}
	if r.Ahead() > 0 {
		fmt.Fprintln(a.Stderr, "Pushing...")
		pctx, pcancel := gitx.WithTimeout(2 * time.Minute)
		defer pcancel()
		if err := r.Push(pctx); err != nil {
			return fmt.Errorf("couldn't push to %s, so %s is unchanged: %v", url, name, err)
		}
	}

	// Swap the shared copy in.
	old := filepath.Join(a.store.LibrariesDir(), "."+name+".old")
	os.RemoveAll(old)
	if err := os.Rename(lib.Path, old); err != nil {
		return err
	}
	if err := os.Rename(work, lib.Path); err != nil {
		os.Rename(old, lib.Path)
		return err
	}
	os.RemoveAll(old)

	// Your own macros stay trusted; anything already in the repo doesn't.
	mine := map[string]bool{}
	for _, c := range copied {
		mine[strings.TrimSuffix(c, ".sh")] = true
	}
	for _, ref := range a.store.Macros(name) {
		if mine[ref.Name] {
			if content, err := ref.Read(); err == nil {
				a.trust(ref, content)
			}
		}
	}
	lib, _ = a.store.Library(name)
	a.refreshAccess(lib, true)
	a.store.UpdateState(func(st *store.State) { st.Lib(name).LastPull = time.Now().UTC() })

	fmt.Fprintf(a.Stderr, "Shared %s: it now syncs with %s.\n", name, url)
	for _, f := range replaced {
		fmt.Fprintf(a.Stderr, "Note: the repo already had %s; your version replaced it (git history keeps the old one).\n", f)
	}
	return nil
}
