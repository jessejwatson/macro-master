// Package store manages mm's folder layout, config, state and libraries.
//
//	~/.config/mm/            $XDG_CONFIG_HOME/mm, or $MM_HOME if set
//	  config.json
//	  state.json
//	  auth.json              tokens on Linux, mode 0600
//	  sync.log               background sync output
//	  jobs/<id>/             detached jobs (see package jobs)
//	  conflicts/<lib>/       local versions set aside by sync
//	  libraries/<lib>/<name>.sh
package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"macro-master/internal/hosts"
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
	Jobs           JobsConfig            `json:"jobs,omitzero"`
}

// JobsConfig controls detached jobs. Empty fields take the defaults.
type JobsConfig struct {
	// Notify is how a finished job is announced: "desktop" (the default)
	// when nobody is attached, "bell" in attached terminals, "both" or
	// "off".
	Notify string `json:"notify,omitempty"`
	// KeepFor is how long finished jobs are kept, e.g. "7d" or "12h"; "0"
	// keeps them until KeepMax pushes them out.
	KeepFor string `json:"keep_for,omitempty"`
	// KeepMax caps how many finished jobs are kept; 0 means no cap.
	KeepMax *int `json:"keep_max,omitempty"`
}

// Job defaults.
const (
	DefaultJobNotify  = "desktop"
	DefaultJobKeepFor = 7 * 24 * time.Hour
	DefaultJobKeepMax = 50
)

// JobNotifyModes are the values jobs.notify may take.
var JobNotifyModes = []string{"desktop", "bell", "both", "off"}

// NotifyMode is Notify, or the default when unset or unknown.
func (j JobsConfig) NotifyMode() string {
	if slices.Contains(JobNotifyModes, j.Notify) {
		return j.Notify
	}
	return DefaultJobNotify
}

// KeepDuration parses KeepFor, falling back to the default. Zero means no
// age limit.
func (j JobsConfig) KeepDuration() time.Duration {
	if d, err := ParseKeepFor(j.KeepFor); err == nil {
		return d
	}
	return DefaultJobKeepFor
}

// ParseKeepFor parses a jobs.keep_for value: a duration such as "12h", days
// such as "7d", or "0" for no limit. Empty is the default.
func ParseKeepFor(v string) (time.Duration, error) {
	v = strings.TrimSpace(v)
	switch v {
	case "":
		return DefaultJobKeepFor, nil
	case "0":
		return 0, nil
	}
	if days, ok := strings.CutSuffix(v, "d"); ok {
		if n, err := strconv.ParseFloat(days, 64); err == nil && n >= 0 {
			return time.Duration(n * float64(24*time.Hour)), nil
		}
	} else if d, err := time.ParseDuration(v); err == nil && d >= 0 {
		return d, nil
	}
	return 0, fmt.Errorf("%q isn't a length of time like 7d, 12h or 0", v)
}

// ParseSyncInterval parses a sync_interval value such as "5m" or "1h".
// Empty is the default.
func ParseSyncInterval(v string) (time.Duration, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return DefaultSyncInterval, nil
	}
	if d, err := time.ParseDuration(v); err == nil && d > 0 {
		return d, nil
	}
	return 0, fmt.Errorf("%q isn't a length of time like 5m or 1h", v)
}

// HostTypes are the values a host's type may be set to.
var HostTypes = []string{hosts.GitHub, hosts.Gitea, hosts.GitLab, hosts.Bitbucket, hosts.Generic}

// CheckConfig reports the first setting in c that mm can't use.
func (s *Store) CheckConfig(c Config) error {
	if c.DefaultLibrary == "" {
		return errors.New("default_library is empty")
	}
	if _, err := s.Library(c.DefaultLibrary); err != nil {
		return fmt.Errorf("default_library: there is no library called %q", c.DefaultLibrary)
	}
	if _, err := ParseSyncInterval(c.SyncInterval); err != nil {
		return fmt.Errorf("sync_interval: %w", err)
	}
	if n := c.Jobs.Notify; n != "" && !slices.Contains(JobNotifyModes, n) {
		return fmt.Errorf("jobs.notify: %q isn't one of %s", n, strings.Join(JobNotifyModes, ", "))
	}
	if _, err := ParseKeepFor(c.Jobs.KeepFor); err != nil {
		return fmt.Errorf("jobs.keep_for: %w", err)
	}
	if c.Jobs.KeepMax != nil && *c.Jobs.KeepMax < 0 {
		return errors.New("jobs.keep_max can't be negative")
	}
	for name, h := range c.Hosts {
		if h.Type != "" && !slices.Contains(HostTypes, h.Type) {
			return fmt.Errorf("hosts.%s.type: %q isn't one of %s", name, h.Type, strings.Join(HostTypes, ", "))
		}
		if h.API != "" && !strings.HasPrefix(h.API, "https://") && !strings.HasPrefix(h.API, "http://") {
			return fmt.Errorf("hosts.%s.api: %q should be a URL starting with https://", name, h.API)
		}
	}
	return nil
}

// ParseConfig reads config.json content strictly: unknown settings are an
// error rather than silently dropped on the next save.
func ParseConfig(data []byte) (Config, error) {
	var c Config
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return Config{}, err
	}
	if dec.More() {
		return Config{}, errors.New("there's more after the closing }")
	}
	return c, nil
}

// ReplaceConfig saves hand-edited config.json content as written, after
// checking it.
func (s *Store) ReplaceConfig(data []byte) error {
	c, err := ParseConfig(data)
	if err != nil {
		return err
	}
	if err := s.CheckConfig(c); err != nil {
		return err
	}
	unlock, err := Lock(s.configPath()+".lock", 3*time.Second, 10*time.Second)
	if err != nil {
		return fmt.Errorf("config.json is busy: %w", err)
	}
	defer unlock()
	if err := WriteFileAtomic(s.configPath(), data, 0o644); err != nil {
		return err
	}
	s.Config = c
	return nil
}

// KeepCount is KeepMax, or the default when unset.
func (j JobsConfig) KeepCount() int {
	if j.KeepMax == nil || *j.KeepMax < 0 {
		return DefaultJobKeepMax
	}
	return *j.KeepMax
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
	if d, err := ParseSyncInterval(c.SyncInterval); err == nil {
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
func (s *Store) JobsDir() string      { return filepath.Join(s.Home, "jobs") }
func (s *Store) AuthPath() string     { return filepath.Join(s.Home, "auth.json") }
func (s *Store) ConfigPath() string   { return s.configPath() }
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
