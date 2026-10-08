package cli

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"macro-master/internal/gitx"
	"macro-master/internal/hosts"
	"macro-master/internal/macro"
	"macro-master/internal/store"
)

const (
	preChangeTimeout = 5 * time.Second
	networkTimeout   = 60 * time.Second
	syncLockStale    = 2 * time.Minute
	permRefreshEvery = time.Hour
	syncLogMax       = 1 << 20
)

// gitOK reports whether git is installed, caching the answer.
func (a *App) gitOK() bool {
	if a.gitAvailable == nil {
		ok := gitx.Available()
		a.gitAvailable = &ok
	}
	return *a.gitAvailable
}

func (a *App) syncedLibraries() []store.Library {
	var out []store.Library
	for _, l := range a.store.Libraries() {
		if l.Synced {
			out = append(out, l)
		}
	}
	return out
}

// showNotices prints messages left by background syncs, once.
func (a *App) showNotices() {
	if len(a.store.Libraries()) > 0 && len(a.syncedLibraries()) > 0 && !a.gitOK() && !a.store.State.GitMissingNoticed {
		a.notef("git isn't installed, so synced libraries won't sync; local libraries still work")
		a.store.UpdateState(func(st *store.State) { st.GitMissingNoticed = true })
	}
	if len(a.store.State.Notices) == 0 {
		return
	}
	var notices []string
	a.store.UpdateState(func(st *store.State) {
		notices, st.Notices = st.Notices, nil
	})
	for _, n := range notices {
		a.notef("%s", n)
	}
}

// maybeBackgroundSync starts a detached sync for synced libraries whose
// last pull is older than the sync interval. The current command carries on
// with the local copy.
func (a *App) maybeBackgroundSync() {
	if !a.gitOK() {
		return
	}
	interval := a.store.Config.Interval()
	var due []string
	for _, l := range a.syncedLibraries() {
		ls := a.store.State.Libraries[l.Name]
		var last time.Time
		if ls != nil {
			last = ls.LastPull
			if ls.LastAttempt.After(last) {
				last = ls.LastAttempt
			}
		}
		if time.Since(last) > interval {
			due = append(due, l.Name)
		}
	}
	if len(due) == 0 {
		return
	}
	a.store.UpdateState(func(st *store.State) {
		for _, n := range due {
			st.Lib(n).LastAttempt = time.Now().UTC()
		}
	})
	a.spawnSync(due...)
}

// spawnSync runs "mm sync --background libs..." as a detached child.
func (a *App) spawnSync(libs ...string) {
	args := append([]string{"sync", "--background"}, libs...)
	spawn := a.Spawn
	if spawn == nil {
		spawn = a.spawnReal
	}
	if err := spawn(args); err != nil {
		a.warnf("can't start a background sync: %v", err)
	}
}

func (a *App) spawnReal(args []string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	logPath := a.store.SyncLogPath()
	if fi, err := os.Stat(logPath); err == nil && fi.Size() > syncLogMax {
		os.Rename(logPath, logPath+".1")
	}
	logf, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer logf.Close()
	cmd := exec.Command(exe, args...)
	cmd.Env = childEnv("MM_HOME=" + a.store.Home)
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

// childEnv is the environment for processes mm starts, without the shell
// hook's source file so a nested mm can't write into it.
func childEnv(extra ...string) []string {
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "MM_SOURCE_FILE=") && !strings.HasPrefix(kv, "MM_SHELL=") {
			env = append(env, kv)
		}
	}
	return append(env, extra...)
}

// checkWritable refuses changes to libraries the user can't push to.
func (a *App) checkWritable(libNames ...string) error {
	for _, libName := range libNames {
		ls := a.store.State.Libraries[libName]
		if ls == nil || ls.Access != store.AccessReadOnly {
			continue
		}
		reason := ls.AccessReason
		if reason == "" {
			reason = "no write permission"
		}
		return fmt.Errorf("%s is read-only for you (%s); you can still run, fav and trust its macros", libName, reason)
	}
	return nil
}

// beforeChange brings synced libraries up to date before add, edit, rm or
// mv, waiting at most a few seconds each before carrying on offline.
func (a *App) beforeChange(libNames ...string) {
	seen := map[string]bool{}
	for _, n := range libNames {
		if !seen[n] {
			seen[n] = true
			a.pullBeforeChange(n)
		}
	}
}

func (a *App) pullBeforeChange(libName string) {
	lib, err := a.store.Library(libName)
	if err != nil || !lib.Synced || !a.gitOK() {
		return
	}
	r := gitx.Repo{Dir: lib.Path}
	unlock, err := store.Lock(r.LockPath(), preChangeTimeout, syncLockStale)
	if err != nil {
		a.notef("%s is busy syncing; working with the local copy", libName)
		return
	}
	defer unlock()
	if _, err := r.Commit("mm: sync local changes"); err != nil {
		a.warnf("%s: %v", libName, err)
	}
	ctx, cancel := gitx.WithTimeout(preChangeTimeout)
	defer cancel()
	if _, err := r.Fetch(ctx); err != nil {
		a.notef("couldn't reach %s's remote; working offline", libName)
		return
	}
	conflicts, err := r.Rebase(a.conflictSaver(libName))
	for _, n := range conflictNotices(libName, conflicts) {
		a.notef("%s", n)
	}
	if err != nil {
		a.warnf("%s: %v", libName, err)
	}
}

// afterChange commits a change to a synced library and pushes it in the
// background.
func (a *App) afterChange(libName, msg string, paths ...string) {
	lib, err := a.store.Library(libName)
	if err != nil || !lib.Synced || !a.gitOK() {
		return
	}
	r := gitx.Repo{Dir: lib.Path}
	rel := make([]string, len(paths))
	for i, p := range paths {
		rel[i], _ = filepath.Rel(lib.Path, p)
	}
	unlock, err := store.Lock(r.LockPath(), 10*time.Second, syncLockStale)
	if err != nil {
		a.notef("%s is busy; your change will be committed on the next sync", libName)
		return
	}
	_, err = r.Commit(msg, rel...)
	unlock()
	if err != nil {
		a.warnf("couldn't commit to %s (%v); it will be retried on the next sync", libName, err)
		return
	}
	a.spawnSync(libName)
}

// conflictSaver stores local versions outside the repo, so they're never
// pushed: conflicts/<lib>/<name>.<timestamp>.sh
func (a *App) conflictSaver(libName string) gitx.SaveFunc {
	stamp := time.Now().Format("20060102-150405")
	return func(path string, content []byte) (string, error) {
		ext := filepath.Ext(path)
		dest := filepath.Join(a.store.ConflictsDir(), libName, strings.TrimSuffix(path, ext)+"."+stamp+ext)
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return "", err
		}
		return dest, os.WriteFile(dest, content, 0o644)
	}
}

func conflictNotices(libName string, cs []gitx.Conflict) []string {
	var out []string
	for _, c := range cs {
		out = append(out, fmt.Sprintf("%s/%s was changed elsewhere; your version is saved at %s",
			libName, strings.TrimSuffix(c.Path, ".sh"), c.SavedAt))
	}
	return out
}

// saveLocal copies files that are about to be thrown away to the
// conflicts folder and returns where they went.
func (a *App) saveLocal(lib store.Library, files []string) []string {
	save := a.conflictSaver(lib.Name)
	var saved []string
	for _, f := range files {
		content, err := os.ReadFile(filepath.Join(lib.Path, f))
		if err != nil {
			continue // deleted locally; nothing to keep
		}
		if p, err := save(f, content); err == nil {
			saved = append(saved, p)
		}
	}
	return saved
}

func savedWhere(saved []string) string {
	switch len(saved) {
	case 0:
		return ""
	case 1:
		return "; your change is saved at " + saved[0]
	}
	return "; your changes are saved in " + filepath.Dir(saved[0])
}

func (a *App) cmdSync(args []string) error {
	flags, names, err := splitFlags(args, "--background")
	if err != nil {
		return err
	}
	bg := flags["--background"]
	if !a.gitOK() {
		return errors.New("git isn't installed; install it to sync libraries")
	}
	var libs []store.Library
	if len(names) == 0 {
		libs = a.syncedLibraries()
		if len(libs) == 0 && !bg {
			fmt.Fprintln(a.Stderr, "No synced libraries. Add one with: mm lib add <name> <git-url>")
		}
	}
	for _, n := range names {
		l, err := a.store.Library(n)
		if err != nil {
			return err
		}
		if !l.Synced {
			if bg {
				continue
			}
			return fmt.Errorf("%s is a local library, so there's nothing to sync", n)
		}
		libs = append(libs, l)
	}
	failed := false
	for _, l := range libs {
		if bg {
			fmt.Fprintf(a.Stderr, "%s %s: sync started\n", time.Now().Format(time.RFC3339), l.Name)
		}
		if err := a.syncLibrary(l, !bg); err != nil {
			failed = true
			if bg {
				fmt.Fprintf(a.Stderr, "%s %s: %v\n", time.Now().Format(time.RFC3339), l.Name, err)
			} else {
				u := a.errUI()
				fmt.Fprintf(a.Stderr, "%s%s: %s %v\n", u.icon(u.icons.err, colBad), u.bold(l.Name), u.fg(colBad, "sync failed:"), err)
			}
		}
	}
	if failed && !bg {
		return exitCode(1)
	}
	return nil
}

// syncLibrary pulls, pushes and refreshes permissions for one library.
// In the foreground it reports progress; in the background, problems are
// left as notices for the next command.
func (a *App) syncLibrary(lib store.Library, fg bool) (err error) {
	r := gitx.Repo{Dir: lib.Path}
	wait := time.Duration(0)
	if fg {
		wait = 30 * time.Second
	}
	unlock, lerr := store.Lock(r.LockPath(), wait, syncLockStale)
	if lerr != nil {
		if fg {
			return errors.New("another sync is running; try again shortly")
		}
		return nil
	}
	defer unlock()

	var notices []string
	report := func(format string, args ...any) {
		msg := fmt.Sprintf(format, args...)
		if fg {
			fmt.Fprintf(a.Stderr, "%s: %s\n", a.errUI().bold(lib.Name), msg)
		}
	}
	defer func() {
		a.store.UpdateState(func(st *store.State) {
			ls := st.Lib(lib.Name)
			if err != nil {
				if ls.SyncError == "" && !fg {
					st.Notices = append(st.Notices, lib.Name+": sync failed, run mm sync for details")
				}
				ls.SyncError = err.Error()
			} else {
				ls.SyncError = ""
				ls.LastPull = time.Now().UTC()
			}
			if !fg {
				st.Notices = append(st.Notices, notices...)
			}
		})
		if fg {
			for _, n := range notices {
				a.notef("%s", n)
			}
		}
	}()

	if fg || time.Since(a.libState(lib.Name).LastPermCheck) > permRefreshEvery {
		a.refreshAccess(lib, fg)
	}

	ctx, cancel := gitx.WithTimeout(networkTimeout)
	defer cancel()

	if a.libState(lib.Name).Access == store.AccessReadOnly {
		if _, err := r.Fetch(ctx); err != nil {
			return err
		}
		if changed := r.LocalChanges(); len(changed) > 0 {
			saved := a.saveLocal(lib, changed)
			if err := r.ResetToUpstream(); err != nil {
				return err
			}
			notices = append(notices, fmt.Sprintf("%s is read-only for you, so local changes were undone%s", lib.Name, savedWhere(saved)))
		} else if err := r.ResetToUpstream(); err != nil {
			if _, ok := r.Upstream(); ok {
				return err
			}
		}
		report("up to date (read-only)")
		return nil
	}

	if made, err := r.Commit("mm: sync local changes"); err != nil {
		return err
	} else if made {
		report("committed changes made outside mm")
	}
	if _, err := r.Fetch(ctx); err != nil {
		return err
	}
	conflicts, err := r.Rebase(a.conflictSaver(lib.Name))
	notices = append(notices, conflictNotices(lib.Name, conflicts)...)
	if err != nil {
		return err
	}
	ahead := r.Ahead()
	if ahead == 0 {
		report("up to date")
		return nil
	}
	err = r.Push(ctx)
	var rej *gitx.Rejected
	if errors.As(err, &rej) {
		saved := a.saveLocal(lib, r.LocalChanges())
		r.ResetToUpstream()
		a.store.UpdateState(func(st *store.State) {
			ls := st.Lib(lib.Name)
			ls.Access, ls.AccessReason, ls.AccessFromPush = store.AccessReadOnly, rej.Reason, true
		})
		notices = append(notices, fmt.Sprintf("%s is read-only for you (%s)%s", lib.Name, rej.Reason, savedWhere(saved)))
		return nil
	}
	if err != nil {
		return err
	}
	report("pushed %d commit(s)", ahead)
	return nil
}

func (a *App) libState(name string) store.LibraryState {
	if ls := a.store.State.Libraries[name]; ls != nil {
		return *ls
	}
	return store.LibraryState{}
}

func (a *App) hostsClient() *hosts.Client {
	if a.Hosts == nil {
		a.Hosts = hosts.NewClient()
	}
	return a.Hosts
}

func (a *App) tokens() *hosts.Tokens {
	if a.Tokens == nil {
		a.Tokens = hosts.NewTokens(a.store.AuthPath())
	}
	return a.Tokens
}

// hostType returns the cached or detected type of host, caching new
// detections in config.json.
func (a *App) hostType(host string) (typ, api string) {
	hc := a.store.Config.Hosts[host]
	if hc.Type == "" {
		hc.Type = a.hostsClient().Detect(host)
		a.store.UpdateConfig(func(c *store.Config) {
			if c.Hosts == nil {
				c.Hosts = map[string]store.HostConfig{}
			}
			h := c.Hosts[host]
			h.Type = hc.Type
			c.Hosts[host] = h
		})
	}
	return hc.Type, a.hostsClient().APIBase(hc.Type, host, hc.API)
}

// refreshAccess asks the host whether the user can push, and records it.
// A read-only flag learned from a rejected push stays until the API says
// the user can write.
func (a *App) refreshAccess(lib store.Library, fg bool) {
	remote, err := hosts.ParseRemote(gitx.Repo{Dir: lib.Path}.RemoteURL())
	access := hosts.Unknown
	if err == nil {
		typ, api := a.hostType(remote.Host)
		token := ""
		if typ != hosts.Generic {
			token = a.tokens().Lookup(remote.Host, typ, remote.HTTPS)
		}
		access, err = a.hostsClient().Check(typ, api, remote, token)
		if errors.Is(err, hosts.ErrBadToken) && fg {
			a.notef("%s rejected your token; run mm auth %s to set a new one", remote.Host, remote.Host)
		}
	}
	a.store.UpdateState(func(st *store.State) {
		ls := st.Lib(lib.Name)
		ls.LastPermCheck = time.Now().UTC()
		switch access {
		case hosts.Writable:
			ls.Access, ls.AccessReason, ls.AccessFromPush = store.AccessWritable, "", false
		case hosts.ReadOnly:
			ls.Access, ls.AccessReason, ls.AccessFromPush = store.AccessReadOnly, "no write permission", false
		default:
			if !ls.AccessFromPush {
				ls.Access, ls.AccessReason = store.AccessUnknown, ""
			}
		}
	})
}

// addSyncedLibrary clones a git remote as a new library.
func (a *App) addSyncedLibrary(name, url string) error {
	if !a.gitOK() {
		return errors.New("git isn't installed; install it to add a synced library")
	}
	if err := macro.ValidateName(name); err != nil {
		return err
	}
	dest := a.store.LibraryPath(name)
	if _, err := os.Stat(dest); err == nil {
		return fmt.Errorf("library %q already exists", name)
	}
	fmt.Fprintf(a.Stderr, "Cloning %s...\n", url)
	ctx, cancel := gitx.WithTimeout(5 * time.Minute)
	defer cancel()
	if err := gitx.Clone(ctx, url, dest); err != nil {
		return fmt.Errorf("couldn't clone %s: %v", url, err)
	}
	lib, err := a.store.Library(name)
	if err != nil {
		return err
	}
	a.refreshAccess(lib, true)
	a.store.UpdateState(func(st *store.State) { st.Lib(name).LastPull = time.Now().UTC() })
	access := a.libState(name).Access
	if access == store.AccessUnknown {
		access = "permissions unknown"
	}
	a.done("Added synced library %s with %d macro(s) (%s).", name, len(a.store.Macros(name)), access)
	return nil
}
