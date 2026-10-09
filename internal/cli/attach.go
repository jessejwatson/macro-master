package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/term"

	"macro-master/internal/jobs"
)

// Keys in mm attach.
const (
	keyCtrlC         = 0x03
	keyCtrlBackslash = 0x1c
)

// cmdAttach connects the terminal to a job. A finished job's last output is
// printed instead.
func (a *App) cmdAttach(args []string) error {
	m, err := a.findJob(args, "attach")
	if err != nil {
		return err
	}
	return a.attachJob(m)
}

func (a *App) attachJob(m *jobs.Meta) error {
	if !m.Running() {
		return a.showFinished(m)
	}
	if !a.Interactive {
		return errors.New("mm attach needs a terminal")
	}
	conn, err := jobs.Dial(m)
	if err != nil {
		// It may have ended between the check and now.
		if fresh, rerr := m.Reload(); rerr == nil && !fresh.Running() {
			return a.showFinished(fresh)
		}
		return fmt.Errorf("can't reach job %s: %w", m.ID, err)
	}
	defer conn.Close()
	at := &attachment{app: a, job: m, conn: conn, in: os.Stdin, out: os.Stdout, u: a.newUI(a.StyleOut)}
	return at.run()
}

// showFinished prints the end of a finished job's output and how it ended,
// and exits with the job's status.
func (a *App) showFinished(m *jobs.Meta) error {
	if b := tailBytes(m.LogPath(), 64<<10); len(b) > 0 {
		a.Stdout.Write(b)
		if b[len(b)-1] != '\n' {
			fmt.Fprintln(a.Stdout)
		}
	}
	a.jobEndLine(m.ID, m.Macro, m.Summary(), m.ExitCode == 0 && m.Error == "" && m.Signal == 0 && m.State() == jobs.StateExited)
	if m.ExitCode != 0 {
		return exitCode(m.ExitCode)
	}
	if m.State() == jobs.StateLost {
		return exitCode(1)
	}
	return nil
}

func (a *App) jobEndLine(id, macro, summary string, ok bool) {
	u := a.errUI()
	icon, c := u.icon(u.icons.ok, colOK), colOK
	if !ok {
		icon, c = u.icon(u.icons.err, colBad), colBad
	}
	fmt.Fprintf(a.Stderr, "%s%s %s\n", icon, u.fg(c, "Job "+id+" "+summary), u.faint("("+macro+")"))
}

// tailBytes reads the last n bytes of a file, from the start of a line.
func tailBytes(path string, n int64) []byte {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil
	}
	off := max(fi.Size()-n, 0)
	b, _ := io.ReadAll(io.NewSectionReader(f, off, fi.Size()-off))
	if off > 0 {
		if i := bytes.IndexByte(b, '\n'); i >= 0 {
			b = b[i+1:]
		}
	}
	return b
}

// attachment is one mm attach session: the job's output fills the screen
// above a one-line toolbar, and keys either control mm (watching) or go to
// the job (typing).
type attachment struct {
	app  *App
	job  *jobs.Meta
	conn *jobs.Conn
	in   *os.File
	out  *os.File
	u    ui

	rows, cols int
	typing     bool
	armed      bool // ctrl+c was pressed once; again kills
	note       string
	alt        bool // the job switched to the alternate screen
	pending    bool // the screen needs fixing once an escape sequence ends
}

type attachEvent struct {
	output []byte
	exit   *jobs.Exit
	keys   []byte
	err    error
	resize bool
	disarm bool
}

func (at *attachment) run() error {
	fd := at.in.Fd()
	state, err := term.MakeRaw(fd)
	if err != nil {
		return err
	}
	defer term.Restore(fd, state)

	at.measure()
	// Push what's on screen into scrollback, then keep the bottom row.
	at.write(strings.Repeat("\n", at.rows) + "\x1b[H")
	at.fixScreen()
	if err := at.conn.Hello(at.rows-1, at.cols); err != nil {
		at.teardown()
		return err
	}

	events := make(chan attachEvent, 64)
	go func() {
		for {
			out, exit, err := at.conn.Next()
			events <- attachEvent{output: out, exit: exit, err: err}
			if err != nil || exit != nil {
				return
			}
		}
	}()
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := at.in.Read(buf)
			if n > 0 {
				events <- attachEvent{keys: append([]byte(nil), buf[:n]...)}
			}
			if err != nil {
				return
			}
		}
	}()
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)
	go func() {
		for range winch {
			events <- attachEvent{resize: true}
		}
	}()
	var disarm *time.Timer

	for ev := range events {
		switch {
		case ev.err != nil:
			at.teardown()
			if fresh, err := at.job.Reload(); err == nil && !fresh.Running() {
				term.Restore(fd, state)
				return at.app.showEnd(fresh)
			}
			return fmt.Errorf("lost the connection to job %s: %v", at.job.ID, ev.err)
		case ev.exit != nil:
			if ev.exit.Bell {
				at.write("\a")
			}
			at.teardown()
			term.Restore(fd, state)
			at.app.jobEndLine(at.job.ID, at.job.Macro, at.endSummary(ev.exit), ev.exit.Code == 0)
			if ev.exit.Code != 0 {
				return exitCode(ev.exit.Code)
			}
			return nil
		case ev.output != nil:
			at.output(ev.output)
		case ev.resize:
			at.measure()
			at.fixScreen()
			at.conn.Resize(at.rows-1, at.cols)
		case ev.disarm:
			at.armed, at.note = false, ""
			at.drawToolbar()
		case ev.keys != nil:
			switch at.keys(ev.keys) {
			case "detach":
				at.teardown()
				term.Restore(fd, state)
				u := at.app.errUI()
				fmt.Fprintf(at.app.Stderr, "Detached from job %s %s\n", at.job.ID, u.faint("· mm attach "+at.job.ID+" to return"))
				return nil
			case "arm":
				if disarm != nil {
					disarm.Stop()
				}
				disarm = time.AfterFunc(5*time.Second, func() { events <- attachEvent{disarm: true} })
			}
		}
	}
	return nil
}

func (at *attachment) endSummary(e *jobs.Exit) string {
	if fresh, err := at.job.Reload(); err == nil && !fresh.Running() {
		return fresh.Summary()
	}
	return e.Outcome
}

// showEnd reports a job that ended while the connection dropped.
func (a *App) showEnd(m *jobs.Meta) error {
	a.jobEndLine(m.ID, m.Macro, m.Summary(), m.ExitCode == 0 && m.Signal == 0 && m.Error == "")
	if m.ExitCode != 0 {
		return exitCode(m.ExitCode)
	}
	return nil
}

// keys handles input. Watching, d detaches, ctrl+c interrupts then kills
// and i starts typing; typing, everything goes to the job until ctrl-\.
func (at *attachment) keys(b []byte) string {
	result := ""
	for len(b) > 0 {
		if at.typing {
			i := bytes.IndexByte(b, keyCtrlBackslash)
			if i < 0 {
				at.conn.Input(b)
				return result
			}
			if i > 0 {
				at.conn.Input(b[:i])
			}
			at.typing, at.note = false, ""
			at.drawToolbar()
			b = b[i+1:]
			continue
		}
		k := b[0]
		b = b[1:]
		switch k {
		case 'd', 'D':
			return "detach"
		case 'i', 'I':
			at.typing, at.armed, at.note = true, false, ""
			at.drawToolbar()
		case keyCtrlC:
			if at.armed {
				at.conn.Kill()
				at.note = "killing the job…"
			} else {
				// As if pressed in the job's own terminal.
				at.conn.Input([]byte{keyCtrlC})
				at.armed = true
				at.note = "interrupt sent · ctrl+c again to kill"
				result = "arm"
			}
			at.drawToolbar()
		}
	}
	return result
}

func (at *attachment) measure() {
	w, h, err := term.GetSize(at.out.Fd())
	if err != nil || w <= 0 || h < 3 {
		w, h = 80, 24
	}
	at.cols, at.rows = w, h
}

func (at *attachment) write(s string) { io.WriteString(at.out, s) }

// output shows the job's output, repairing the scroll region and toolbar
// when the job may have reset or cleared them.
func (at *attachment) output(b []byte) {
	at.out.Write(b)
	touched, alt := scanScreen(b)
	if alt != 0 {
		at.alt = alt > 0
	}
	if touched {
		at.pending = true
	}
	if at.pending && !endsMidEscape(b) {
		at.pending = false
		at.fixScreen()
	}
}

// scanScreen looks for sequences that may reset the scroll region or clear
// the toolbar row: a full reset, a scroll region, erasing the display or
// switching screens. alt is 1 or -1 for the last switch to or from the
// alternate screen, otherwise 0.
func scanScreen(b []byte) (touched bool, alt int) {
	touched = bytes.Contains(b, []byte("\x1bc"))
	for i := 0; i+1 < len(b); i++ {
		if b[i] != 0x1b || b[i+1] != '[' {
			continue
		}
		j := i + 2
		for j < len(b) && b[j] >= 0x30 && b[j] <= 0x3f {
			j++
		}
		if j >= len(b) {
			return true, alt
		}
		switch params := string(b[i+2 : j]); b[j] {
		case 'r', 'J':
			touched = true
		case 'h', 'l':
			if params == "?1049" || params == "?1047" || params == "?47" {
				touched, alt = true, 1
				if b[j] == 'l' {
					alt = -1
				}
			}
		}
	}
	return touched, alt
}

// endsMidEscape is true when b stops partway through an escape sequence,
// where writing mm's own sequences would garble the job's.
func endsMidEscape(b []byte) bool {
	i := bytes.LastIndexByte(b, 0x1b)
	if i < 0 {
		return false
	}
	rest := b[i+1:]
	if len(rest) == 0 {
		return true
	}
	switch rest[0] {
	case '[':
		for _, c := range rest[1:] {
			if c >= 0x40 && c <= 0x7e {
				return false
			}
		}
		return true
	case ']', 'P', '_', '^':
		return !bytes.ContainsAny(rest, "\a") && !bytes.Contains(rest, []byte("\x1b\\"))
	}
	return false
}

// fixScreen keeps the job to the rows above the toolbar and redraws it. The
// cursor and attributes are saved around it so the job doesn't notice.
func (at *attachment) fixScreen() {
	at.write(fmt.Sprintf("\x1b7\x1b[1;%dr\x1b8", at.rows-1))
	at.drawToolbar()
}

func (at *attachment) drawToolbar() {
	at.write(fmt.Sprintf("\x1b7\x1b[%d;1H\x1b[0m\x1b[2K%s\x1b8", at.rows, at.toolbar()))
}

func (at *attachment) toolbar() string {
	u := at.u
	badge := lipgloss.NewStyle().Bold(true).Reverse(true)
	mode, hints := " WATCHING ", "d detach · ctrl+c stop job · i type"
	modeStyle := badge.Foreground(colAccent)
	if at.typing {
		mode, hints = " TYPING ", `keys go to the job · ctrl-\ back to watching`
		modeStyle = badge.Foreground(colOK)
	}
	if at.note != "" {
		hints = at.note
	}
	left := u.render(modeStyle, mode) + "  " + hints
	if !u.on {
		left = "[" + strings.TrimSpace(mode) + "]  " + hints
	}
	right := "job " + at.job.ID + " · " + at.job.Macro + " "
	gap := at.cols - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 2 {
		return ansi.Truncate(left, at.cols, "…")
	}
	return left + strings.Repeat(" ", gap) + u.faint(right)
}

// teardown gives the terminal back: full scroll region, no toolbar, and
// modes a full-screen job may have left on turned off.
func (at *attachment) teardown() {
	var b strings.Builder
	if at.alt {
		b.WriteString("\x1b[?1049l")
	}
	b.WriteString("\x1b[r")
	// Show the cursor; normal cursor keys and keypad; no bracketed paste or
	// mouse reporting; plain attributes.
	b.WriteString("\x1b[?25h\x1b[?1l\x1b>\x1b[?2004l\x1b[?1000l\x1b[?1002l\x1b[?1003l\x1b[?1006l\x1b[0m")
	fmt.Fprintf(&b, "\x1b[%d;1H\x1b[2K", at.rows)
	at.write(b.String())
}
