package cli

import (
	"bytes"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"macro-master/internal/store"
)

// setting is one value in config.json as the settings panel shows it.
type setting struct {
	section string
	key     string // as written in config.json, e.g. jobs.notify
	label   string
	help    string
	choices []string // nil for free text
	def     string   // shown when unset
	get     func(store.Config) string
	// set applies v to c; "" resets it to the default.
	set func(c *store.Config, v string)
}

// settings lists every setting, hosts last, one pair of rows per host.
func (a *App) settings() []setting {
	var libs []string
	for _, l := range a.store.Libraries() {
		libs = append(libs, l.Name)
	}
	list := []setting{
		{
			section: "General", key: "default_library", label: "Default library",
			help:    "The library mm add saves into.",
			choices: libs,
			get:     func(c store.Config) string { return c.DefaultLibrary },
			set: func(c *store.Config, v string) {
				if v == "" {
					v = store.DefaultLibrary
				}
				c.DefaultLibrary = v
			},
			def: store.DefaultLibrary,
		},
		{
			section: "General", key: "sync_interval", label: "Sync interval",
			help: "How stale a synced library may get before mm pulls in the background, e.g. 5m or 1h.",
			def:  "5m",
			get:  func(c store.Config) string { return c.SyncInterval },
			set:  func(c *store.Config, v string) { c.SyncInterval = v },
		},
		{
			section: "Detached jobs", key: "jobs.notify", label: "Notify when done",
			help:    "desktop: a notification when nobody's attached · bell: a bell in attached terminals · both · off",
			choices: store.JobNotifyModes,
			def:     store.DefaultJobNotify,
			get:     func(c store.Config) string { return c.Jobs.Notify },
			set:     func(c *store.Config, v string) { c.Jobs.Notify = v },
		},
		{
			section: "Detached jobs", key: "jobs.keep_for", label: "Keep finished for",
			help: "Finished jobs older than this are deleted, e.g. 7d or 12h; 0 for no age limit.",
			def:  "7d",
			get:  func(c store.Config) string { return c.Jobs.KeepFor },
			set:  func(c *store.Config, v string) { c.Jobs.KeepFor = v },
		},
		{
			section: "Detached jobs", key: "jobs.keep_max", label: "Keep at most",
			help: "How many finished jobs to keep; 0 for no limit.",
			def:  strconv.Itoa(store.DefaultJobKeepMax),
			get: func(c store.Config) string {
				if c.Jobs.KeepMax == nil {
					return ""
				}
				return strconv.Itoa(*c.Jobs.KeepMax)
			},
			set: func(c *store.Config, v string) {
				if v == "" {
					c.Jobs.KeepMax = nil
					return
				}
				n, err := strconv.Atoi(v)
				if err != nil {
					n = -1 // rejected by CheckConfig
				}
				c.Jobs.KeepMax = &n
			},
		},
	}
	for _, host := range slices.Sorted(maps.Keys(a.store.Config.Hosts)) {
		list = append(list,
			setting{
				section: "Hosts", key: "hosts." + host + ".type", label: host + " type",
				help:    "Which API mm uses to check your access. Reset to detect it again.",
				choices: store.HostTypes,
				def:     "detect",
				get:     func(c store.Config) string { return c.Hosts[host].Type },
				set:     func(c *store.Config, v string) { setHost(c, host, func(h *store.HostConfig) { h.Type = v }) },
			},
			setting{
				section: "Hosts", key: "hosts." + host + ".api", label: host + " API",
				help: "API base URL, for self-hosted servers such as GitHub Enterprise.",
				def:  "standard",
				get:  func(c store.Config) string { return c.Hosts[host].API },
				set:  func(c *store.Config, v string) { setHost(c, host, func(h *store.HostConfig) { h.API = v }) },
			},
		)
	}
	return list
}

// setHost changes one host's settings on a copy of the map, so the
// config being edited doesn't share it with the one in use.
func setHost(c *store.Config, host string, fn func(*store.HostConfig)) {
	hs := maps.Clone(c.Hosts)
	if hs == nil {
		hs = map[string]store.HostConfig{}
	}
	h := hs[host]
	fn(&h)
	hs[host] = h
	c.Hosts = hs
}

// applySetting validates and saves one change.
func (a *App) applySetting(s setting, v string) error {
	v = strings.TrimSpace(v)
	c := a.store.Config
	s.set(&c, v)
	if err := a.store.CheckConfig(c); err != nil {
		return err
	}
	return a.store.UpdateConfig(func(c *store.Config) { s.set(c, v) })
}

// cmdConfig opens the settings panel, or edits or locates config.json.
func (a *App) cmdConfig(args []string) error {
	switch {
	case len(args) == 1 && args[0] == "edit":
		return a.editConfig(true)
	case len(args) == 1 && args[0] == "path":
		fmt.Fprintln(a.Stdout, a.store.ConfigPath())
		return nil
	case len(args) > 0:
		return errors.New("usage: mm config [edit|path]")
	}
	if !a.Interactive {
		return a.listConfig()
	}
	cursor, note, bad := 0, "", false
	for {
		m, err := a.runConfigPanel(cursor, note, bad)
		if err != nil {
			return err
		}
		if !m.openEditor {
			return nil
		}
		before, _ := os.ReadFile(a.store.ConfigPath())
		err = a.editConfig(false)
		after, _ := os.ReadFile(a.store.ConfigPath())
		cursor, bad = m.cursor, err != nil
		switch {
		case errors.Is(err, errCancelled):
			note = "Your edits weren't saved."
		case err != nil:
			note = "Not saved: " + err.Error()
		case bytes.Equal(before, after):
			note = "No changes."
		default:
			note = "Saved your changes to config.json."
		}
	}
}

// listConfig prints every setting, for when there's no terminal.
func (a *App) listConfig() error {
	u := a.outUI()
	for _, s := range a.settings() {
		v := s.get(a.store.Config)
		if v == "" {
			v = s.def + u.faint(" (default)")
		}
		fmt.Fprintf(a.Stdout, "%s = %s\n", s.key, v)
	}
	return nil
}

// editConfig opens config.json in $EDITOR and saves it if it checks out,
// offering another go when it doesn't. report says whether to print the
// outcome; the settings panel shows it itself.
func (a *App) editConfig(report bool) error {
	path := a.store.ConfigPath()
	orig, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp("", "mm-config-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_, werr := tmp.Write(orig)
	if cerr := tmp.Close(); werr != nil || cerr != nil {
		return errors.Join(werr, cerr)
	}
	for {
		if err := a.runEditor(tmp.Name()); err != nil {
			return fmt.Errorf("the editor exited with an error (%v), so nothing was saved", err)
		}
		edited, err := os.ReadFile(tmp.Name())
		if err != nil {
			return err
		}
		if bytes.Equal(edited, orig) {
			if report {
				a.nothing("No changes; nothing saved.")
			}
			return nil
		}
		err = a.store.ReplaceConfig(edited)
		if err == nil {
			if report {
				a.done("Saved %s.", a.errUI().bold(path))
			}
			return nil
		}
		a.warnf("config.json has a problem: %v", err)
		if a.Prompts == nil {
			return errors.New("config.json wasn't saved")
		}
		again, err := a.confirm("Edit it again?", true)
		if err != nil {
			return err
		}
		if !again {
			a.nothing("Nothing saved.")
			return errCancelled
		}
	}
}

// runEditor opens path in $EDITOR, through sh so EDITOR can carry
// arguments, e.g. "code -w".
func (a *App) runEditor(path string) error {
	editor := a.Getenv("EDITOR")
	if editor == "" {
		editor = "vi"
	}
	cmd := exec.Command("sh", "-c", editor+` "$1"`, "mm-edit", path)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = a.Stdin, a.Stdout, a.Stderr
	if f, ok := a.Stdin.(*os.File); !ok || !isTerminal(f) {
		// The editor needs the terminal even when stdin is busy.
		if tty, err := os.Open("/dev/tty"); err == nil {
			defer tty.Close()
			cmd.Stdin = tty
		}
	}
	return cmd.Run()
}

func (a *App) runConfigPanel(cursor int, note string, bad bool) (configModel, error) {
	m := newConfigModel(a, a.newUI(a.StyleOut))
	m.cursor = min(cursor, len(m.items)-1)
	m.note, m.noteBad = note, bad
	final, err := tea.NewProgram(m).Run()
	if err != nil {
		return m, err
	}
	return final.(configModel), nil
}

// tildePath shortens a path in the home folder to ~/...
func tildePath(p string) string {
	if home, err := os.UserHomeDir(); err == nil && strings.HasPrefix(p, home+"/") {
		return "~" + p[len(home):]
	}
	return p
}

// configModel is the settings panel.
type configModel struct {
	app    *App
	u      ui
	items  []setting
	cursor int
	width  int

	editing bool
	input   textinput.Model

	note       string // what just happened
	noteBad    bool
	openEditor bool // quit to edit config.json by hand
	quitting   bool
}

func newConfigModel(a *App, u ui) configModel {
	in := textinput.New()
	in.Prompt = ""
	in.CharLimit = 200
	return configModel{app: a, u: u, items: a.settings(), input: in, width: a.Width}
}

func (m configModel) Init() tea.Cmd { return nil }

func (m configModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		return m, nil
	case tea.KeyPressMsg:
		if m.editing {
			return m.updateEditing(msg)
		}
		return m.updateBrowsing(msg)
	}
	if m.editing {
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		return m, cmd
	}
	return m, nil
}

func (m configModel) updateBrowsing(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	s := m.items[m.cursor]
	switch msg.String() {
	case "q", "esc", "ctrl+c":
		m.quitting = true
		return m, tea.Quit
	case "up", "k":
		m.cursor = (m.cursor + len(m.items) - 1) % len(m.items)
		m.note = ""
	case "down", "j", "tab":
		m.cursor = (m.cursor + 1) % len(m.items)
		m.note = ""
	case "e":
		m.openEditor, m.quitting = true, true
		return m, tea.Quit
	case "backspace", "delete", "r":
		m.apply(s, "", "Reset "+s.key+" to the default.")
	case "enter", "space", "right", "l":
		if s.choices != nil {
			m.cycle(s, 1)
			return m, nil
		}
		if msg.String() == "enter" || msg.String() == "space" {
			m.editing = true
			m.input.SetValue(s.get(m.app.store.Config))
			m.input.Placeholder = s.def
			m.input.CursorEnd()
			m.note = ""
			return m, m.input.Focus()
		}
	case "left", "h":
		if s.choices != nil {
			m.cycle(s, -1)
		}
	}
	return m, nil
}

// cycle moves a choice setting to the next or previous value.
func (m *configModel) cycle(s setting, step int) {
	cur := s.get(m.app.store.Config)
	if cur == "" {
		cur = s.def
	}
	i := slices.Index(s.choices, cur)
	if i < 0 {
		i = 0
		if step < 0 {
			i = 1
		}
	}
	next := s.choices[(i+step+len(s.choices))%len(s.choices)]
	m.apply(s, next, fmt.Sprintf("Set %s to %s.", s.key, next))
}

func (m *configModel) apply(s setting, v, ok string) {
	if err := m.app.applySetting(s, v); err != nil {
		m.note, m.noteBad = err.Error(), true
		return
	}
	m.note, m.noteBad = ok, false
}

func (m configModel) updateEditing(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "ctrl+c":
		m.editing = false
		m.input.Blur()
		m.note = ""
		return m, nil
	case "enter":
		s := m.items[m.cursor]
		v := strings.TrimSpace(m.input.Value())
		msg := fmt.Sprintf("Set %s to %s.", s.key, v)
		if v == "" {
			msg = "Reset " + s.key + " to the default."
		}
		m.apply(s, v, msg)
		if !m.noteBad {
			m.editing = false
			m.input.Blur()
		}
		return m, nil
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

func (m configModel) View() tea.View {
	if m.quitting {
		return tea.NewView("") // leave nothing behind
	}
	return tea.NewView(m.render())
}

func (m configModel) render() string {
	u := m.u
	c := m.app.store.Config
	labelW := 0
	for _, s := range m.items {
		labelW = max(labelW, lipgloss.Width(s.label))
	}
	var b strings.Builder
	b.WriteString(u.accent("Settings") + "  " + u.faint(tildePath(m.app.store.ConfigPath())) + "\n")
	section := ""
	for i, s := range m.items {
		if s.section != section {
			section = s.section
			b.WriteString("\n" + u.bold(section) + "\n")
		}
		selected := i == m.cursor
		pointer := "  "
		label := s.label + strings.Repeat(" ", labelW-lipgloss.Width(s.label))
		if selected {
			pointer = u.fg(colAccent, "▸ ")
			label = u.bold(label)
		} else {
			label = u.faint(label)
		}
		var value string
		switch {
		case selected && m.editing:
			value = m.input.View()
		default:
			v := s.get(c)
			if v == "" {
				value = s.def + " " + u.faint("default")
			} else {
				value = u.fg(colCmd, v)
			}
			if selected && s.choices != nil {
				value = u.faint("‹ ") + value + u.faint(" ›")
			}
		}
		b.WriteString(pointer + label + "   " + value + "\n")
	}
	b.WriteString("\n" + u.faint(m.items[m.cursor].help) + "\n")
	if m.note != "" {
		if m.noteBad {
			b.WriteString(u.fg(colBad, m.note) + "\n")
		} else {
			b.WriteString(u.fg(colOK, m.note) + "\n")
		}
	}
	keys := "↑↓ move · enter change · ⌫ reset · e edit config.json · q quit"
	if m.items[m.cursor].choices != nil {
		keys = "↑↓ move · ←→ change · ⌫ reset · e edit config.json · q quit"
	}
	if m.editing {
		keys = "enter save · esc cancel · empty for the default"
	}
	b.WriteString(u.faint(keys))

	width := m.width
	if width <= 0 {
		width = 80
	}
	box := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(colBorder).Padding(0, 1)
	if !u.on {
		return b.String() + "\n"
	}
	if lipgloss.Width(b.String())+4 > width {
		box = box.Width(width)
	}
	return box.Render(b.String()) + "\n"
}
