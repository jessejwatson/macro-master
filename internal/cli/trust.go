package cli

import (
	"fmt"
	"path/filepath"
	"strings"

	"macro-master/internal/gitx"
	"macro-master/internal/macro"
	"macro-master/internal/store"
)

// Trust statuses for macros in synced libraries.
const (
	trustNew     = "new"
	trustChanged = "changed"
)

// trustStatus says whether a synced macro's content is new or changed
// since the user last trusted it. Local macros are always trusted ("").
func (a *App) trustStatus(ref store.Ref, content string) string {
	lib, err := a.store.Library(ref.Library)
	if err != nil || !lib.Synced {
		return ""
	}
	e, ok := a.store.State.Trust[ref.ID()]
	switch {
	case !ok:
		return trustNew
	case e.Blob != macro.BlobHash(content):
		return trustChanged
	}
	return ""
}

// checkTrust shows a new or changed shared macro and asks before it runs.
func (a *App) checkTrust(ref store.Ref, content string) error {
	status := a.trustStatus(ref, content)
	if status == "" {
		return nil
	}
	if a.Prompts == nil {
		what := "is new"
		if status == trustChanged {
			what = "has changed since you last trusted it"
		}
		return fmt.Errorf("%s %s; read it with mm show %s, then run mm trust %s", ref.ID(), what, ref.ID(), ref.ID())
	}

	lib, _ := a.store.Library(ref.Library)
	r := gitx.Repo{Dir: lib.Path}
	rel, _ := filepath.Rel(lib.Path, ref.Path)
	label := "new"
	if status == trustChanged {
		label = "changed since you last trusted it"
	}
	fmt.Fprintf(a.Stderr, "%s is %s.\n", ref.ID(), label)
	if log := r.FileLog(rel); log != "" && a.gitOK() {
		fmt.Fprintf(a.Stderr, "Last commit: %s\n", log)
	}
	fmt.Fprintln(a.Stderr)
	prev := a.store.State.Trust[ref.ID()]
	diff := ""
	if status == trustChanged && a.gitOK() && r.CommitExists(prev.Commit) {
		diff = r.Diff(prev.Commit, rel)
	}
	if diff != "" {
		fmt.Fprint(a.Stderr, diff)
	} else {
		for i, l := range strings.Split(strings.TrimRight(content, "\n"), "\n") {
			fmt.Fprintf(a.Stderr, "%4d  %s\n", i+1, l)
		}
	}
	fmt.Fprintln(a.Stderr)
	ok, err := a.confirm("Run it and trust this version?", false)
	if err != nil {
		return err
	}
	if !ok {
		fmt.Fprintln(a.Stderr, "Not run.")
		return errCancelled
	}
	return a.trust(ref, content)
}

// trust records content as the trusted version of a synced macro.
func (a *App) trust(ref store.Ref, content string) error {
	lib, err := a.store.Library(ref.Library)
	if err != nil || !lib.Synced {
		return nil
	}
	commit := ""
	if a.gitOK() {
		rel, _ := filepath.Rel(lib.Path, ref.Path)
		commit = gitx.Repo{Dir: lib.Path}.FileCommit(rel)
	}
	return a.store.UpdateState(func(st *store.State) {
		st.Trust[ref.ID()] = store.TrustEntry{Blob: macro.BlobHash(content), Commit: commit}
	})
}

func (a *App) cmdTrust(args []string) error {
	addr, err := oneArg("trust", args)
	if err != nil {
		return err
	}
	ref, err := a.resolve(addr)
	if err != nil {
		return err
	}
	content, err := ref.Read()
	if err != nil {
		return err
	}
	if lib, _ := a.store.Library(ref.Library); !lib.Synced {
		fmt.Fprintf(a.Stderr, "%s is in a local library, so it doesn't need trusting.\n", ref.ID())
		return nil
	}
	if a.trustStatus(ref, content) == "" {
		fmt.Fprintf(a.Stderr, "%s is already trusted.\n", ref.ID())
		return nil
	}
	if err := a.trust(ref, content); err != nil {
		return err
	}
	fmt.Fprintf(a.Stderr, "Trusted the current version of %s.\n", ref.ID())
	return nil
}
