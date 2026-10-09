package cli

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"charm.land/lipgloss/v2"
	"charm.land/lipgloss/v2/table"
	"github.com/charmbracelet/x/term"

	"macro-master/internal/jobs"
	"macro-master/internal/macro"
	"macro-master/internal/store"
)

// runDetached starts the macro as a background job and returns once its
// helper is up.
func (a *App) runDetached(ref store.Ref, args []string, name string) error {
	m, rest, _, err := a.prepare(ref, args)
	if err != nil {
		return err
	}
	if m.Mode == "source" {
		return fmt.Errorf("%s can't run detached: it's a \"# mode: source\" macro, which changes your current shell", ref.ID())
	}
	interp := m.Interpreter()
	path, err := a.lookPath(interp[0])
	if err != nil {
		return fmt.Errorf("can't find %s to run %s; check the macro's #! line", interp[0], ref.ID())
	}

	root := a.store.JobsDir()
	a.cleanJobs()
	job, err := jobs.Create(root)
	if err != nil {
		return err
	}
	// The job always runs its own copy, so what runs is exactly what was
	// checked, whatever happens to the library meanwhile.
	script := job.ScriptPath(macro.Ext)
	if err := os.WriteFile(script, []byte(m.String()), 0o700); err != nil {
		os.RemoveAll(job.JobDir())
		return err
	}
	cwd, _ := os.Getwd()
	rows, cols := jobTermSize()
	job.Macro, job.Library, job.Name, job.JobName = ref.ID(), ref.Library, ref.Name, name
	job.Path, job.Script, job.Dir = path, script, cwd
	job.Argv = append(append(append([]string{}, interp...), script), rest...)
	job.Rows, job.Cols = rows, cols
	job.Notify = a.store.Config.Jobs.NotifyMode()
	if err := job.Save(); err != nil {
		os.RemoveAll(job.JobDir())
		return err
	}

	start := a.StartJob
	if start == nil {
		start = a.startJobReal
	}
	if err := start(job.JobDir(), "MM_MACRO="+ref.Name, "MM_LIBRARY="+ref.Library); err != nil {
		os.RemoveAll(job.JobDir())
		return fmt.Errorf("can't start job: %w", err)
	}
	job, err = waitStarted(job)
	if err != nil {
		return err
	}
	a.recordRun(ref)

	u := a.errUI()
	a.done("Started %s: %s", u.bold(job.Title()), ref.ID())
	fmt.Fprintln(a.Stderr, u.faint(fmt.Sprintf("  mm attach %s to watch or type into it · mm jobs to list jobs", job.Handle())))
	return nil
}

// startJobReal starts mm __job <dir> in its own session, so it outlives
// this terminal.
func (a *App) startJobReal(dir string, env ...string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	logf, err := os.OpenFile(filepath.Join(dir, "helper.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer logf.Close()
	cmd := exec.Command(exe, "__job", dir)
	cmd.Env = childEnv(append([]string{"MM_HOME=" + a.store.Home}, env...)...)
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

// waitStarted waits for the helper to start the macro, so a failure is
// reported here and mm attach works straight away.
func waitStarted(job *jobs.Meta) (*jobs.Meta, error) {
	deadline := time.Now().Add(5 * time.Second)
	for {
		m, err := job.Reload()
		if err == nil {
			if m.Error != "" {
				return m, fmt.Errorf("job %s %s", m.ID, m.Summary())
			}
			if m.HelperPID != 0 {
				return m, nil
			}
		}
		if time.Now().After(deadline) {
			return job, fmt.Errorf("job %s didn't start; see %s", job.ID, filepath.Join(job.JobDir(), "helper.log"))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// jobTermSize is the size a job's terminal starts at: this terminal's, less
// the row mm attach keeps for its toolbar.
func jobTermSize() (rows, cols int) {
	for _, f := range []*os.File{os.Stdout, os.Stderr, os.Stdin} {
		if w, h, err := term.GetSize(f.Fd()); err == nil && w > 0 && h > 1 {
			return h - 1, w
		}
	}
	return 23, 80
}

// cleanJobs deletes old finished jobs as config.json says.
func (a *App) cleanJobs() {
	c := a.store.Config.Jobs
	jobs.Clean(a.store.JobsDir(), c.KeepDuration(), c.KeepCount())
}

// cmdJobs lists jobs, or with "clear" deletes the finished ones.
func (a *App) cmdJobs(args []string) error {
	root := a.store.JobsDir()
	if len(args) == 1 && args[0] == "clear" {
		n := 0
		for _, m := range jobs.List(root) {
			if !m.Running() && jobs.Remove(m) == nil {
				n++
			}
		}
		a.done("Cleared %d finished %s", n, plural(n, "job", "jobs"))
		return nil
	}
	if len(args) > 0 {
		return errors.New("usage: mm jobs [clear]")
	}
	a.cleanJobs()
	list := jobs.List(root)
	if len(list) == 0 {
		fmt.Fprintln(a.Stderr, a.errUI().faint("No jobs. Run a macro detached with: mm -d <name>"))
		return nil
	}
	u := a.outUI()
	headers := []string{"ID", "NAME", "STATUS", "MACRO", "STARTED", "TOOK"}
	var rows [][]string
	for _, m := range list {
		name := u.fg(colAccent, m.JobName)
		if m.JobName == "" {
			name = u.faint("-")
		}
		rows = append(rows, []string{u.bold(m.ID), name, jobStatus(u, m), m.Macro, u.faint(ago(m.Started)), u.faint(m.Duration().String())})
	}
	if u.on {
		// lipgloss measures cells by what shows, not the colour codes in
		// them, so styled columns line up.
		t := table.New().
			Border(lipgloss.HiddenBorder()).
			BorderHeader(false).
			Headers(headers...).
			Rows(rows...).
			StyleFunc(func(row, col int) lipgloss.Style {
				s := lipgloss.NewStyle().PaddingRight(2)
				if row == table.HeaderRow {
					s = s.Faint(true)
				}
				return s
			})
		fmt.Fprintln(a.Stdout, trimTableEdges(t.String()))
		return nil
	}
	w := tabwriter.NewWriter(a.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, strings.Join(headers, "\t"))
	for _, r := range rows {
		fmt.Fprintln(w, strings.Join(r, "\t"))
	}
	return w.Flush()
}

// trimTableEdges drops a hidden-border table's blank first and last lines
// and its one-space left edge, so it sits flush like plain output.
func trimTableEdges(s string) string {
	lines := strings.Split(s, "\n")
	if len(lines) > 2 {
		lines = lines[1 : len(lines)-1]
	}
	for i, l := range lines {
		lines[i] = strings.TrimRight(strings.TrimPrefix(l, " "), " ")
	}
	return strings.Join(lines, "\n")
}

// jobStatus is a job's state for listings: running, or how it ended.
func jobStatus(u ui, m *jobs.Meta) string {
	switch {
	case m.Running():
		return u.fg(colCmd, "● running")
	case m.Error == "" && m.State() == jobs.StateExited && m.ExitCode == 0:
		return u.fg(colOK, "✓ "+m.Outcome())
	case m.State() == jobs.StateLost:
		return u.fg(colWarn, "? "+m.Outcome())
	}
	return u.fg(colBad, "✗ "+m.Outcome())
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// findJob turns an argument into a job: its number, its name, or a macro
// address. A name or macro picks the most recent such job, preferring
// running ones. With no argument it's the only running job.
func (a *App) findJob(args []string, verb string) (*jobs.Meta, error) {
	root := a.store.JobsDir()
	list := jobs.List(root)
	if len(args) > 1 {
		return nil, fmt.Errorf("usage: mm %s [job]", verb)
	}
	if len(args) == 0 {
		var running []*jobs.Meta
		for _, m := range list {
			if m.Running() {
				running = append(running, m)
			}
		}
		switch len(running) {
		case 0:
			return nil, errors.New("no jobs are running; run mm jobs to see finished ones")
		case 1:
			return running[0], nil
		}
		var ids []string
		for _, m := range running {
			ids = append(ids, m.Handle()+" ("+m.Macro+")")
		}
		return nil, fmt.Errorf("%d jobs are running; say which: mm %s <job>, one of %s", len(running), verb, strings.Join(ids, ", "))
	}
	arg := args[0]
	if m, err := jobs.Get(root, arg); err == nil {
		return m, nil
	}
	// Newest first, so the first match wins unless a later one is running.
	pick := func(ok func(*jobs.Meta) bool) *jobs.Meta {
		var match *jobs.Meta
		for _, m := range list {
			if ok(m) && (match == nil || (m.Running() && !match.Running())) {
				match = m
			}
		}
		return match
	}
	match := pick(func(m *jobs.Meta) bool { return m.JobName == arg })
	if match == nil {
		match = pick(func(m *jobs.Meta) bool { return m.Macro == arg || strings.HasSuffix(m.Macro, "/"+arg) })
	}
	if match == nil {
		return nil, fmt.Errorf("there is no job %q; run mm jobs to see them", arg)
	}
	return match, nil
}

// cmdKill stops a running job and waits for it to end.
func (a *App) cmdKill(args []string) error {
	m, err := a.findJob(args, "kill")
	if err != nil {
		return err
	}
	if !m.Running() {
		return fmt.Errorf("%s has already ended (%s)", m.Title(), m.Outcome())
	}
	if c, err := jobs.Dial(m); err == nil {
		err = c.Terminate()
		c.Close()
		if err != nil {
			return err
		}
	} else if m.PID != 0 {
		syscall.Kill(-m.PID, syscall.SIGTERM)
	}
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		if fresh, err := m.Reload(); err == nil && !fresh.Running() {
			a.done("Stopped %s: %s", m.Title(), m.Macro)
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("job %s is still running after being told to stop", m.ID)
}
