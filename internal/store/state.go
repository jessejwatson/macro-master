package store

import (
	"slices"
	"strings"
	"time"
)

// State is per-machine data, kept in state.json and never committed.
type State struct {
	Favourites []string                 `json:"favourites,omitempty"`
	LastRun    map[string]time.Time     `json:"last_run,omitempty"`
	Trust      map[string]TrustEntry    `json:"trust,omitempty"`
	Libraries  map[string]*LibraryState `json:"libraries,omitempty"`
	// Notices are messages from background syncs, shown once by the next
	// interactive command.
	Notices []string `json:"notices,omitempty"`
	// GitMissingNoticed stops the "git isn't installed" notice repeating.
	GitMissingNoticed bool `json:"git_missing_noticed,omitempty"`
}

// TrustEntry records the version of a shared macro the user trusted.
type TrustEntry struct {
	Blob   string `json:"blob"`             // git blob hash of the content
	Commit string `json:"commit,omitempty"` // commit it came from
}

// Access is what the current user may do with a synced library.
type Access string

const (
	AccessUnknown  Access = ""
	AccessWritable Access = "writable"
	AccessReadOnly Access = "read-only"
)

// LibraryState is per-library sync and permission state.
type LibraryState struct {
	LastPull      time.Time `json:"last_pull,omitzero"`       // last successful pull
	LastAttempt   time.Time `json:"last_attempt,omitzero"`    // last background sync start
	LastPermCheck time.Time `json:"last_perm_check,omitzero"` // last API permission check
	Access        Access    `json:"access,omitempty"`
	AccessReason  string    `json:"access_reason,omitempty"`
	// AccessFromPush is set when read-only was learned from a rejected push.
	AccessFromPush bool   `json:"access_from_push,omitempty"`
	SyncError      string `json:"sync_error,omitempty"`
}

func (st *State) init() {
	if st.LastRun == nil {
		st.LastRun = map[string]time.Time{}
	}
	if st.Trust == nil {
		st.Trust = map[string]TrustEntry{}
	}
	if st.Libraries == nil {
		st.Libraries = map[string]*LibraryState{}
	}
}

// Lib returns the state for a library, creating it if needed.
func (st *State) Lib(name string) *LibraryState {
	st.init()
	ls := st.Libraries[name]
	if ls == nil {
		ls = &LibraryState{}
		st.Libraries[name] = ls
	}
	return ls
}

// IsFavourite reports whether the macro id ("lib/name") is a favourite.
func (st *State) IsFavourite(id string) bool { return slices.Contains(st.Favourites, id) }

// ToggleFavourite flips the favourite flag and reports the new value.
func (st *State) ToggleFavourite(id string) bool {
	if i := slices.Index(st.Favourites, id); i >= 0 {
		st.Favourites = slices.Delete(st.Favourites, i, i+1)
		return false
	}
	st.Favourites = append(st.Favourites, id)
	slices.Sort(st.Favourites)
	return true
}

// Rename carries favourite, last-run and trust entries from one id to
// another.
func (st *State) Rename(from, to string) {
	st.init()
	if i := slices.Index(st.Favourites, from); i >= 0 {
		st.Favourites[i] = to
		slices.Sort(st.Favourites)
	}
	if t, ok := st.LastRun[from]; ok {
		delete(st.LastRun, from)
		st.LastRun[to] = t
	}
	if t, ok := st.Trust[from]; ok {
		delete(st.Trust, from)
		st.Trust[to] = t
	}
}

// Forget drops every entry for id.
func (st *State) Forget(id string) {
	st.Favourites = slices.DeleteFunc(st.Favourites, func(f string) bool { return f == id })
	delete(st.LastRun, id)
	delete(st.Trust, id)
}

// prune drops entries for missing macros and libraries and reports whether
// anything was (or, with dryRun, would be) removed.
func (st *State) prune(macros, libs map[string]bool, dryRun bool) bool {
	changed := false
	gone := func(id string) bool {
		if macros[id] {
			return false
		}
		changed = true
		return true
	}
	if dryRun {
		for _, id := range st.Favourites {
			gone(id)
		}
		for id := range st.LastRun {
			gone(id)
		}
		for id := range st.Trust {
			gone(id)
		}
		for l := range st.Libraries {
			if !libs[l] {
				changed = true
			}
		}
		return changed
	}
	st.Favourites = slices.DeleteFunc(st.Favourites, gone)
	for id := range st.LastRun {
		if gone(id) {
			delete(st.LastRun, id)
		}
	}
	for id := range st.Trust {
		if gone(id) {
			delete(st.Trust, id)
		}
	}
	for l := range st.Libraries {
		if !libs[l] {
			delete(st.Libraries, l)
			changed = true
		}
	}
	return changed
}

// RenamePrefix moves every entry under from ("lib" or "lib/folder") to
// sit under to instead, for folder and library renames.
func (st *State) RenamePrefix(from, to string) {
	st.init()
	move := func(id string) (string, bool) {
		if rest, ok := strings.CutPrefix(id, from+"/"); ok {
			return to + "/" + rest, true
		}
		return id, false
	}
	for i, id := range st.Favourites {
		st.Favourites[i], _ = move(id)
	}
	slices.Sort(st.Favourites)
	for id, t := range st.LastRun {
		if n, ok := move(id); ok {
			delete(st.LastRun, id)
			st.LastRun[n] = t
		}
	}
	for id, t := range st.Trust {
		if n, ok := move(id); ok {
			delete(st.Trust, id)
			st.Trust[n] = t
		}
	}
	if !strings.Contains(from, "/") && !strings.Contains(to, "/") {
		if ls, ok := st.Libraries[from]; ok {
			delete(st.Libraries, from)
			st.Libraries[to] = ls
		}
	}
}
