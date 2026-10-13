package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"macro-master/internal/jobs"
)

// liveEvery is how often mm jobs --live looks at the jobs again.
const liveEvery = time.Second

// cmdJobsLive shows running jobs, and those that end while it's open, until
// esc, q or ctrl+c. a picks one to attach to; detaching comes back here.
func (a *App) cmdJobsLive() error {
	if !a.Interactive {
		return errors.New("mm jobs --live needs a terminal")
	}
	a.cleanJobs()
	_, err := tea.NewProgram(newLiveModel(a, a.outUI(), time.Now())).Run()
	return err
}

// liveModel is the mm jobs --live view.
type liveModel struct {
	app   *App
	u     ui
	since time.Time // jobs that end after this stay listed

	list    []*jobs.Meta
	running map[string]bool // at the last look, to see which ended

	selecting bool
	cursor    string // the chosen job's ID while selecting

	note   string // what just happened
	noteOK bool

	width, height int
	quitting      bool
}

type liveTickMsg struct{}

// liveAttachedMsg is sent when an attach started from the view returns.
type liveAttachedMsg struct {
	outcome string
	ok      bool
	err     error
}

func newLiveModel(a *App, u ui, since time.Time) liveModel {
	m := liveModel{app: a, u: u, since: since, width: a.Width, height: a.Height}
	m.refresh()
	return m
}

func (m liveModel) Init() tea.Cmd { return liveTick() }

func liveTick() tea.Cmd {
	return tea.Tick(liveEvery, func(time.Time) tea.Msg { return liveTickMsg{} })
}

// refresh looks at the jobs again, noting any that ended since last time.
func (m *liveModel) refresh() {
	var list []*jobs.Meta
	running := map[string]bool{}
	for _, j := range jobs.List(m.app.store.JobsDir()) {
		switch {
		case j.Running():
			running[j.ID] = true
		case !j.EndTime().After(m.since):
			continue
		case m.running[j.ID]:
			m.note, m.noteOK = upperFirst(j.Title())+" "+j.Summary(), j.ExitCode == 0 && j.Signal == 0 && j.Error == ""
		}
		list = append(list, j)
	}
	m.list, m.running = list, running
	if m.selecting && !running[m.cursor] {
		m.cursor = m.firstRunning()
		if m.cursor == "" {
			m.selecting = false
		}
	}
}

// selectable is the running jobs in the order they're listed.
func (m liveModel) selectable() []string {
	var ids []string
	for _, j := range m.list {
		if m.running[j.ID] {
			ids = append(ids, j.ID)
		}
	}
	return ids
}

func (m liveModel) firstRunning() string {
	if ids := m.selectable(); len(ids) > 0 {
		return ids[0]
	}
	return ""
}

func (m liveModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	case liveTickMsg:
		m.refresh()
		return m, liveTick()
	case liveAttachedMsg:
		m.selecting = false
		m.note, m.noteOK = msg.outcome, msg.ok
		if msg.err != nil {
			m.note, m.noteOK = msg.err.Error(), false
		}
		m.refresh()
	case tea.KeyPressMsg:
		return m.key(msg.String())
	}
	return m, nil
}

func (m liveModel) key(k string) (tea.Model, tea.Cmd) {
	if k == "ctrl+c" || k == "q" || (k == "esc" && !m.selecting) {
		m.quitting = true
		return m, tea.Quit
	}
	if !m.selecting {
		if k == "a" {
			if m.cursor = m.firstRunning(); m.cursor == "" {
				m.note, m.noteOK = "No running jobs to attach to", false
			} else {
				m.selecting, m.note = true, ""
			}
		}
		return m, nil
	}
	ids := m.selectable()
	i := 0
	for n, id := range ids {
		if id == m.cursor {
			i = n
		}
	}
	switch k {
	case "esc":
		m.selecting = false
	case "up", "k":
		m.cursor = ids[max(i-1, 0)]
	case "down", "j":
		m.cursor = ids[min(i+1, len(ids)-1)]
	case "enter", "a":
		for _, j := range m.list {
			if j.ID == m.cursor {
				c := &liveAttach{app: m.app, job: j}
				return m, tea.Exec(c, func(err error) tea.Msg {
					return liveAttachedMsg{outcome: c.outcome, ok: c.ok, err: err}
				})
			}
		}
	}
	return m, nil
}

func (m liveModel) View() tea.View {
	if m.quitting {
		return tea.NewView("") // leave nothing behind
	}
	v := tea.NewView(m.render())
	v.AltScreen = true
	return v
}

func (m liveModel) render() string {
	u := m.u
	var b strings.Builder
	n := len(m.running)
	b.WriteString(u.bold("Live jobs") + u.faint(fmt.Sprintf(" · %d running", n)) + "\n\n")

	// Title, blank, header, blank, note and keys around the rows.
	list := m.list
	if m.height > 0 {
		list, _ = newestJobs(list, max(m.height-6, 1))
	}
	if len(list) == 0 {
		b.WriteString(u.faint("No jobs running. Jobs started with mm -d show up here.") + "\n")
	} else {
		pointer := func(j *jobs.Meta) string {
			if m.selecting && j.ID == m.cursor {
				return u.fg(colAccent, "›") + " "
			}
			return "  "
		}
		b.WriteString(jobsTable(u, list, m.width, pointer) + "\n")
	}
	b.WriteString("\n")
	if m.note != "" {
		c := colOK
		if !m.noteOK {
			c = colBad
		}
		b.WriteString(u.fg(c, m.note))
	}
	b.WriteString("\n")
	hints := "a attach · esc/q quit"
	if m.selecting {
		hints = "↑/↓ choose · enter attach · esc back"
	}
	b.WriteString(u.faint(hints))
	return b.String()
}

func upperFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// liveAttach attaches to a job from the live view, which has given up the
// terminal meanwhile, and keeps how it went for the view to show.
type liveAttach struct {
	app     *App
	job     *jobs.Meta
	outcome string
	ok      bool
}

func (c *liveAttach) Run() error {
	m, err := c.job.Reload()
	if err != nil {
		return err
	}
	if !m.Running() {
		c.outcome, c.ok = upperFirst(m.Title())+" "+m.Summary(), m.ExitCode == 0 && m.Signal == 0 && m.Error == ""
		return nil
	}
	conn, err := jobs.Dial(m)
	if err != nil {
		return fmt.Errorf("can't reach job %s: %w", m.ID, err)
	}
	defer conn.Close()
	at := &attachment{app: c.app, job: m, conn: conn, in: os.Stdin, out: os.Stdout, u: c.app.newUI(c.app.StyleOut), live: true}
	if err := at.run(); err != nil {
		return err
	}
	c.outcome, c.ok = at.outcome, at.outcomeOK
	return nil
}

// The attachment uses the terminal directly.
func (c *liveAttach) SetStdin(io.Reader)  {}
func (c *liveAttach) SetStdout(io.Writer) {}
func (c *liveAttach) SetStderr(io.Writer) {}
