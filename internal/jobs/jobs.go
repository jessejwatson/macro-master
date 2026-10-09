// Package jobs runs macros detached from the terminal and lets mm attach to
// them later.
//
//	~/.config/mm/jobs/
//	  .seq                   last job number handed out
//	  <id>/meta.json         what runs, and how it ended
//	  <id>/output.log        everything the job printed
//	  <id>/script.sh         the macro as checked, removed when it ends
//	  <id>/sock              the helper's socket while the job runs
//
// Each job is run by a helper process (mm __job <dir>) that holds the job's
// pseudo-terminal, logs its output and serves attached clients.
package jobs

import (
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"macro-master/internal/store"
)

// Job states.
const (
	StateStarting = "starting"
	StateRunning  = "running"
	StateExited   = "exited"
	StateLost     = "lost" // the helper died without recording an end
)

// Meta describes one job. The creator writes it once; after that only the
// job's helper changes it.
type Meta struct {
	ID      string   `json:"id"`
	JobName string   `json:"job_name,omitempty"` // optional, set with -n or in mm attach
	Macro   string   `json:"macro"`              // library/folders/name
	Library string   `json:"library"`
	Name    string   `json:"name"`
	Path    string   `json:"path"`   // resolved interpreter
	Script  string   `json:"script"` // removed when the job ends
	Argv    []string `json:"argv"`
	Dir     string   `json:"dir"`
	Rows    int      `json:"rows"`
	Cols    int      `json:"cols"`
	Notify  string   `json:"notify"`

	Started   time.Time  `json:"started"`
	HelperPID int        `json:"helper_pid,omitempty"`
	PID       int        `json:"pid,omitempty"`
	Ended     *time.Time `json:"ended,omitempty"`
	ExitCode  int        `json:"exit_code"`
	Signal    int        `json:"signal,omitempty"`
	Error     string     `json:"error,omitempty"`

	dir     string
	modTime time.Time
}

// Handle is how to refer to the job: its name if it has one, else its
// number.
func (m *Meta) Handle() string {
	if m.JobName != "" {
		return m.JobName
	}
	return m.ID
}

// Title is "job 3", or "job 3 (web)" for a named job.
func (m *Meta) Title() string {
	if m.JobName != "" {
		return "job " + m.ID + " (" + m.JobName + ")"
	}
	return "job " + m.ID
}

// CheckName reports why name can't name a job under root: it must be short
// letters, digits, dots, dashes or underscores, not just digits (those are
// job numbers), and not used by another running job.
func CheckName(root, name, exceptID string) error {
	if name == "" || len(name) > 32 {
		return errors.New("a job name needs 1 to 32 characters")
	}
	digits := true
	for _, r := range name {
		switch {
		case r >= '0' && r <= '9':
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '-', r == '_', r == '.':
			digits = false
		default:
			return fmt.Errorf("a job name can only use letters, digits, - _ and ., not %q", r)
		}
	}
	if digits {
		return errors.New("a job name can't be just digits; those are job numbers")
	}
	if name[0] == '-' || name[0] == '.' {
		return errors.New("a job name can't start with - or .")
	}
	for _, m := range List(root) {
		if m.ID != exceptID && m.JobName == name && m.Running() {
			return fmt.Errorf("job %s is already called %s", m.ID, name)
		}
	}
	return nil
}

// JobDir is the folder holding the job's files.
func (m *Meta) JobDir() string { return m.dir }

// LogPath is the job's output log.
func (m *Meta) LogPath() string { return filepath.Join(m.dir, "output.log") }

// ScriptPath is where the creator puts the macro to run.
func (m *Meta) ScriptPath(ext string) string { return filepath.Join(m.dir, "script"+ext) }

// Reload reads the job back from disk, to see changes the helper made.
func (m *Meta) Reload() (*Meta, error) { return Load(m.dir) }

// SocketPath is the helper's socket. Unix socket paths are limited to about
// 100 bytes, so a long home folder falls back to the temp folder.
func (m *Meta) SocketPath() string { return socketPath(m.dir) }

func socketPath(dir string) string {
	p := filepath.Join(dir, "sock")
	if len(p) < 100 {
		return p
	}
	h := fnv.New64a()
	h.Write([]byte(dir))
	return filepath.Join(os.TempDir(), fmt.Sprintf("mm-%d-%x.sock", os.Getuid(), h.Sum64()))
}

// State says whether the job is starting, running, exited or lost.
func (m *Meta) State() string {
	switch {
	case m.Ended != nil:
		return StateExited
	case m.HelperPID == 0:
		if time.Since(m.modTime) > time.Minute {
			return StateLost
		}
		return StateStarting
	case !alive(m.HelperPID):
		return StateLost
	}
	return StateRunning
}

// Running is true for a job that is starting or running.
func (m *Meta) Running() bool {
	s := m.State()
	return s == StateStarting || s == StateRunning
}

// EndTime is when the job ended, or for a lost job when it last changed.
func (m *Meta) EndTime() time.Time {
	if m.Ended != nil {
		return *m.Ended
	}
	return m.modTime
}

// Duration is how long the job ran, or has run so far.
func (m *Meta) Duration() time.Duration {
	end := time.Now()
	if !m.Running() {
		end = m.EndTime()
	}
	return end.Sub(m.Started).Round(time.Second)
}

// Outcome describes how a finished job ended, e.g. "exit 0" or "killed".
func (m *Meta) Outcome() string {
	switch {
	case m.Error != "":
		return "failed to start"
	case m.State() == StateLost:
		return "lost"
	case m.Signal != 0:
		return strings.ToLower(signalName(syscall.Signal(m.Signal)))
	}
	return "exit " + strconv.Itoa(m.ExitCode)
}

func signalName(s syscall.Signal) string {
	switch s {
	case syscall.SIGINT:
		return "interrupted"
	case syscall.SIGKILL:
		return "killed"
	case syscall.SIGTERM:
		return "terminated"
	}
	return s.String()
}

// Save writes meta.json atomically.
func (m *Meta) Save() error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return store.WriteFileAtomic(filepath.Join(m.dir, "meta.json"), append(data, '\n'), 0o600)
}

func alive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// Load reads the job in dir.
func Load(dir string) (*Meta, error) {
	p := filepath.Join(dir, "meta.json")
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	m := &Meta{dir: dir}
	if err := json.Unmarshal(data, m); err != nil {
		return nil, fmt.Errorf("%s is unreadable: %w", p, err)
	}
	if fi, err := os.Stat(p); err == nil {
		m.modTime = fi.ModTime()
	}
	return m, nil
}

// Create makes a folder for a new job with the next free number.
func Create(root string) (*Meta, error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	seq := filepath.Join(root, ".seq")
	unlock, err := store.Lock(seq+".lock", 3*time.Second, 10*time.Second)
	if err != nil {
		return nil, fmt.Errorf("the jobs folder is busy: %w", err)
	}
	defer unlock()
	n := 0
	if b, err := os.ReadFile(seq); err == nil {
		n, _ = strconv.Atoi(strings.TrimSpace(string(b)))
	}
	for {
		n++
		dir := filepath.Join(root, strconv.Itoa(n))
		if err := os.Mkdir(dir, 0o700); errors.Is(err, os.ErrExist) {
			continue
		} else if err != nil {
			return nil, err
		}
		if err := store.WriteFileAtomic(seq, []byte(strconv.Itoa(n)+"\n"), 0o600); err != nil {
			return nil, err
		}
		return &Meta{ID: strconv.Itoa(n), dir: dir, Started: time.Now().UTC()}, nil
	}
}

// List returns every job under root, newest first. Unreadable ones are
// skipped.
func List(root string) []*Meta {
	entries, _ := os.ReadDir(root)
	var out []*Meta
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if m, err := Load(filepath.Join(root, e.Name())); err == nil {
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, _ := strconv.Atoi(out[i].ID)
		b, _ := strconv.Atoi(out[j].ID)
		return a > b
	})
	return out
}

// Get returns job id under root.
func Get(root, id string) (*Meta, error) {
	if id == "" || strings.ContainsAny(id, `/\.`) {
		return nil, fmt.Errorf("there is no job %q", id)
	}
	m, err := Load(filepath.Join(root, id))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("there is no job %s; run mm jobs to see them", id)
	}
	return m, err
}

// Clean deletes finished jobs that ended more than keepFor ago, then all but
// the newest keepMax of the rest. Zero means no limit. Running jobs, and
// folders too new to have their meta.json yet, are left alone. It returns
// how many were deleted.
func Clean(root string, keepFor time.Duration, keepMax int) int {
	var done []*Meta
	for _, m := range List(root) {
		if !m.Running() {
			done = append(done, m)
		}
	}
	sort.SliceStable(done, func(i, j int) bool { return done[i].EndTime().After(done[j].EndTime()) })
	removed := 0
	for i, m := range done {
		old := keepFor > 0 && time.Since(m.EndTime()) > keepFor
		over := keepMax > 0 && i >= keepMax
		if old || over {
			if Remove(m) == nil {
				removed++
			}
		}
	}
	return removed
}

// Remove deletes a finished job's files.
func Remove(m *Meta) error {
	if m.Running() {
		return fmt.Errorf("job %s is still running", m.ID)
	}
	os.Remove(socketPath(m.dir))
	return os.RemoveAll(m.dir)
}
