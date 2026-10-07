// Package store manages mm's folder layout, config, state and libraries.
//
//	~/.config/mm/            $XDG_CONFIG_HOME/mm, or $MM_HOME if set
//	  config.json
//	  state.json
//	  auth.json              tokens on Linux, mode 0600
//	  sync.log               background sync output
//	  conflicts/<lib>/       local versions set aside by sync
//	  libraries/<lib>/<name>.sh
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// DefaultLibrary is created on first run.
const DefaultLibrary = "personal"

// DefaultSyncInterval is how stale a synced library may get before a
// command starts a background pull.
const DefaultSyncInterval = 5 * time.Minute

// Config is user settings, kept in config.json.
type Config struct {
	DefaultLibrary string                `json:"default_library"`
	SyncInterval   string                `json:"sync_interval,omitempty"`
	Hosts          map[string]HostConfig `json:"hosts,omitempty"`
}

// HostConfig holds per-host settings. Type is detected and cached, and can
// be set by hand ("github", "gitea", "gitlab", "bitbucket" or "generic").
// API overrides the API base URL, e.g. for GitHub Enterprise.
type HostConfig struct {
	Type string `json:"type,omitempty"`
	API  string `json:"api,omitempty"`
}

// Interval parses SyncInterval, falling back to the default.
func (c Config) Interval() time.Duration {
	if d, err := time.ParseDuration(c.SyncInterval); err == nil && d > 0 {
		return d
	}
	return DefaultSyncInterval
}

// Store is an opened mm home folder.
type Store struct {
	Home   string
	Config Config
	State  State
	// Warnings collects problems found while opening, such as a corrupt
	// config file that was backed up and reset.
	Warnings []string
}

// HomeDir returns the mm home folder from the environment.
func HomeDir() (string, error) {
	if h := os.Getenv("MM_HOME"); h != "" {
		return h, nil
	}
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "mm"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("can't find your home folder; set MM_HOME: %w", err)
	}
	return filepath.Join(home, ".config", "mm"), nil
}

// Open loads the store at home, creating the layout and the personal
// library on first run.
func Open(home string) (*Store, error) {
	s := &Store{Home: home}
	if err := os.MkdirAll(s.LibrariesDir(), 0o755); err != nil {
		return nil, fmt.Errorf("can't create %s: %w", s.LibrariesDir(), err)
	}
	_, statErr := os.Stat(s.configPath())
	firstRun := errors.Is(statErr, os.ErrNotExist)

	s.loadJSON(s.configPath(), &s.Config, true)
	s.loadJSON(s.statePath(), &s.State, true)
	s.State.init()

	if firstRun {
		if err := os.MkdirAll(s.LibraryPath(DefaultLibrary), 0o755); err != nil {
			return nil, err
		}
		s.Config.DefaultLibrary = DefaultLibrary
		if err := s.SaveConfig(); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func (s *Store) LibrariesDir() string { return filepath.Join(s.Home, "libraries") }
func (s *Store) ConflictsDir() string { return filepath.Join(s.Home, "conflicts") }
func (s *Store) SyncLogPath() string  { return filepath.Join(s.Home, "sync.log") }
func (s *Store) AuthPath() string     { return filepath.Join(s.Home, "auth.json") }
func (s *Store) configPath() string   { return filepath.Join(s.Home, "config.json") }
func (s *Store) statePath() string    { return filepath.Join(s.Home, "state.json") }

// loadJSON reads path into v. A missing file leaves v as is. An unreadable
// one is, when recover is set, backed up with a .bak suffix so v starts
// fresh, with a warning.
func (s *Store) loadJSON(path string, v any, recover bool) error {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err == nil {
		if err = json.Unmarshal(data, v); err == nil {
			return nil
		}
	}
	if !recover {
		return err
	}
	bak := path + ".bak"
	if rerr := os.Rename(path, bak); rerr != nil {
		s.Warnings = append(s.Warnings, fmt.Sprintf("%s is unreadable (%v) and couldn't be backed up: %v", filepath.Base(path), err, rerr))
		return nil
	}
	s.Warnings = append(s.Warnings, fmt.Sprintf("%s was unreadable, so it was moved to %s and mm started fresh", filepath.Base(path), bak))
	return nil
}

// SaveConfig writes config.json atomically.
func (s *Store) SaveConfig() error { return writeJSON(s.configPath(), s.Config) }

// UpdateConfig reloads config.json under a lock, applies fn and saves, so
// a background sync and a foreground command can't lose each other's
// changes.
func (s *Store) UpdateConfig(fn func(*Config)) error {
	unlock, err := Lock(s.configPath()+".lock", 3*time.Second, 10*time.Second)
	if err != nil {
		return fmt.Errorf("config.json is busy: %w", err)
	}
	defer unlock()
	c := s.Config
	var fresh Config
	if s.loadJSON(s.configPath(), &fresh, false) == nil && fresh.DefaultLibrary != "" {
		c = fresh
	}
	fn(&c)
	if err := writeJSON(s.configPath(), c); err != nil {
		return err
	}
	s.Config = c
	return nil
}

// UpdateState reloads state.json under a lock, applies fn and saves.
func (s *Store) UpdateState(fn func(*State)) error {
	unlock, err := Lock(s.statePath()+".lock", 3*time.Second, 10*time.Second)
	if err != nil {
		return fmt.Errorf("state.json is busy: %w", err)
	}
	defer unlock()
	st := s.State
	var fresh State
	if s.loadJSON(s.statePath(), &fresh, false) == nil {
		st = fresh
	}
	st.init()
	fn(&st)
	if err := writeJSON(s.statePath(), st); err != nil {
		return err
	}
	s.State = st
	return nil
}

func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return WriteFileAtomic(path, append(data, '\n'), 0o644)
}

// WriteFileAtomic writes to a temp file in the same folder, then renames it
// over path, so a crash never leaves a half-written file.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // no-op after a successful rename
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(perm); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Prune drops state entries for macros and libraries that no longer exist,
// such as after a library folder was deleted by hand.
func (s *Store) Prune() error {
	macros := map[string]bool{}
	for _, r := range s.AllMacros() {
		macros[r.ID()] = true
	}
	libs := map[string]bool{}
	for _, l := range s.Libraries() {
		libs[l.Name] = true
	}
	if !s.State.prune(macros, libs, true) {
		return nil
	}
	return s.UpdateState(func(st *State) { st.prune(macros, libs, false) })
}
