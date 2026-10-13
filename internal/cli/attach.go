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
	"github.com/muesli/cancelreader"

	"macro-master/internal/jobs"
	"macro-master/internal/macro"
	"macro-master/internal/store"
)

// Keys in mm attach.
const (
	keyCtrlC = 0x03
	keyEsc   = 0x1b
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

// runAttached runs the filled-in macro m as a job attached to this
// terminal, typing. Its output goes inline, into the terminal's own lines,
// so detaching leaves it there. If it ends before you detach, the job is
// forgotten, as if it had run there directly.
func (a *App) runAttached(ref store.Ref, m macro.Macro, rest []string) error {
	job, err := a.startJob(ref, m, rest, "", true)
	if err != nil {
		return err
	}
	conn, err := jobs.Dial(job)
	if err != nil {
		if fresh, rerr := job.Reload(); rerr == nil && !fresh.Running() {
			return a.endForeground(fresh, true)
		}
		return fmt.Errorf("can't reach job %s: %w", job.ID, err)
	}
	defer conn.Close()
	at := &attachment{app: a, job: job, conn: conn, in: os.Stdin, out: os.Stdout, u: a.newUI(a.StyleOut),
		foreground: true, typing: true}
	return at.run()
}

// onTerminal is true when mm's own stdin and stdout are in use, which
// attaching reads and draws on directly.
func (a *App) onTerminal() bool {
	return a.Stdin == io.Reader(os.Stdin) && a.Stdout == io.Writer(os.Stdout)
}

// endForeground finishes a run that ended while attached: it prints the
// job's output unless it's already shown, deletes the job, and exits with
// the job's status.
func (a *App) endForeground(m *jobs.Meta, show bool) error {
	if b := plainScreen(tailBytes(m.LogPath(), logShown)); show && len(b) > 0 {
		a.Stdout.Write(b)
		if b[len(b)-1] != '\n' {
			fmt.Fprintln(a.Stdout)
		}
	}
	jobs.Remove(m)
	switch {
	case m.Error != "":
		return fmt.Errorf("%s %s", m.Macro, m.Summary())
	case m.State() == jobs.StateLost:
		return exitCode(1)
	case m.ExitCode != 0:
		return exitCode(m.ExitCode)
	}
	return nil
}

// logShown is how much of a foreground run's output is left in the
// terminal when it ends.
const logShown = 4 << 20

// showFinished prints the end of a finished job's output and how it ended,
// and exits with the job's status.
func (a *App) showFinished(m *jobs.Meta) error {
	if b := tailBytes(m.LogPath(), 64<<10); len(b) > 0 {
		a.Stdout.Write(b)
		if b[len(b)-1] != '\n' {
			fmt.Fprintln(a.Stdout)
		}
	}
	a.jobEndLine(m, m.Summary(), m.ExitCode == 0 && m.Error == "" && m.Signal == 0 && m.State() == jobs.StateExited, "")
	if m.ExitCode != 0 {
		return exitCode(m.ExitCode)
	}
	if m.State() == jobs.StateLost {
		return exitCode(1)
	}
	return nil
}

// jobEndLine reports how a job ended, with an optional dimmed hint.
func (a *App) jobEndLine(m *jobs.Meta, summary string, ok bool, hint string) {
	u := a.errUI()
	icon, c := u.icon(u.icons.ok, colOK), colOK
	if !ok {
		icon, c = u.icon(u.icons.err, colBad), colBad
	}
	title := strings.ToUpper(m.Title()[:1]) + m.Title()[1:]
	extra := "(" + m.Macro + ")"
	if hint != "" {
		extra += " · " + hint
	}
	fmt.Fprintf(a.Stderr, "%s%s %s\n", icon, u.fg(c, title+" "+summary), u.faint(extra))
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

// attachment is one mm attach session. The job's output fills the
// terminal's alternate screen above a one-line toolbar, so leaving puts
// back what was there before. Keys control mm (watching), go to the job
// (typing), or name the job.
type attachment struct {
	app  *App
	job  *jobs.Meta
	conn *jobs.Conn
	in   *os.File
	out  *os.File
	u    ui

	rows, cols int
	foreground bool // started by mm <macro>, not mm -d: output goes inline
	alt        bool // inline, the job is on the alternate screen
	midLine    bool // inline, the last output didn't end a line
	typing     bool
	naming     bool
	// live is attached from mm jobs --live, which shows outcome when this
	// returns instead of it being printed.
	live      bool
	outcome   string
	outcomeOK bool
	name      []rune // the name being typed
	armed     bool   // ctrl+c was pressed once; again kills
	note      string
	carry     []byte // an escape sequence cut off at the end of the last output
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
	if at.foreground {
		// Make room for the toolbar on the bottom row, scrolling up if the
		// cursor is on it.
		at.write("\n\x1b[A")
	} else {
		at.write("\x1b[?1049h\x1b[H\x1b[2J")
	}
	at.fixScreen()
	if err := at.conn.Hello(at.rows-1, at.cols); err != nil {
		at.teardown()
		return err
	}

	// The readers stop when this returns, so whatever has the terminal
	// next, such as mm jobs --live, gets every key.
	events := make(chan attachEvent, 64)
	done := make(chan struct{})
	defer close(done)
	send := func(ev attachEvent) bool {
		select {
		case events <- ev:
			return true
		case <-done:
			return false
		}
	}
	go func() {
		for {
			out, exit, err := at.conn.Next()
			if !send(attachEvent{output: out, exit: exit, err: err}) || err != nil || exit != nil {
				return
			}
		}
	}()
	keys, err := at.keyReader()
	if err != nil {
		at.teardown()
		return err
	}
	defer keys.Close()
	defer keys.Cancel()
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := keys.Read(buf)
			if n > 0 && !send(attachEvent{keys: append([]byte(nil), buf[:n]...)}) {
				return
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
			if !send(attachEvent{resize: true}) {
				return
			}
		}
	}()
	var disarm *time.Timer

	for ev := range events {
		switch {
		case ev.err != nil:
			at.teardown()
			if fresh, err := at.job.Reload(); err == nil && !fresh.Running() {
				term.Restore(fd, state)
				if at.live {
					at.ended(fresh.Summary(), fresh.ExitCode == 0 && fresh.Signal == 0 && fresh.Error == "")
					return nil
				}
				return at.app.showEnd(fresh)
			}
			return fmt.Errorf("lost the connection to job %s: %v", at.job.ID, ev.err)
		case ev.exit != nil:
			if ev.exit.Bell {
				at.write("\a")
			}
			at.teardown()
			term.Restore(fd, state)
			if at.live {
				at.ended(at.endSummary(ev.exit), ev.exit.Code == 0)
				return nil
			}
			if at.foreground {
				if fresh, err := at.job.Reload(); err == nil && !fresh.Running() {
					return at.app.endForeground(fresh, false)
				}
			}
			at.app.jobEndLine(at.job, at.endSummary(ev.exit), ev.exit.Code == 0, "mm attach "+at.job.Handle()+" shows its output")
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
				if at.live {
					at.outcome, at.outcomeOK = "Detached from "+at.job.Title(), true
					return nil
				}
				u := at.app.errUI()
				fmt.Fprintf(at.app.Stderr, "Detached from %s %s\n", at.job.Title(), u.faint("· mm attach "+at.job.Handle()+" to return"))
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

// keyReader reads keys from the terminal in a way that can be stopped. On
// macOS that needs /dev/tty itself, which the stopper can wait on.
func (at *attachment) keyReader() (keyReader, error) {
	var in io.Reader = at.in
	closeIn := func() error { return nil }
	if isTerminal(at.in) {
		if tty, err := os.Open("/dev/tty"); err == nil {
			in, closeIn = tty, tty.Close
		}
	}
	r, err := cancelreader.NewReader(in)
	if err != nil {
		closeIn()
		return keyReader{}, err
	}
	return keyReader{r, closeIn}, nil
}

type keyReader struct {
	cancelreader.CancelReader
	closeIn func() error
}

func (k keyReader) Close() error {
	k.CancelReader.Close()
	return k.closeIn()
}

// ended records how the job ended, for mm jobs --live to show.
func (at *attachment) ended(summary string, ok bool) {
	at.outcome, at.outcomeOK = upperFirst(at.job.Title())+" "+summary, ok
}

func (at *attachment) endSummary(e *jobs.Exit) string {
	if fresh, err := at.job.Reload(); err == nil && !fresh.Running() {
		return fresh.Summary()
	}
	return e.Outcome
}

// showEnd reports a job that ended while the connection dropped.
func (a *App) showEnd(m *jobs.Meta) error {
	a.jobEndLine(m, m.Summary(), m.ExitCode == 0 && m.Signal == 0 && m.Error == "", "")
	if m.ExitCode != 0 {
		return exitCode(m.ExitCode)
	}
	return nil
}

// keys handles input. Watching, d detaches, ctrl+c interrupts then kills,
// i starts typing and n names the job; typing, everything goes to the job
// until esc. An esc with more keys straight after it, as arrow keys and
// alt+key send, goes to the job. Watching, escape sequences are skipped
// whole: they're special keys, or the terminal answering the job's output
// (focus, status and version reports), and their letters aren't keys.
func (at *attachment) keys(b []byte) string {
	result := ""
	for len(b) > 0 {
		if at.naming {
			b = at.nameKeys(b)
			continue
		}
		if at.typing {
			if b[len(b)-1] != keyEsc {
				at.conn.Input(b)
				return result
			}
			if len(b) > 1 {
				at.conn.Input(b[:len(b)-1])
			}
			at.typing, at.note = false, ""
			at.drawToolbar()
			return result
		}
		if b[0] == keyEsc {
			b = b[escapeLen(b):]
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
		case 'n', 'N':
			at.naming, at.armed, at.note = true, false, ""
			at.name = []rune(at.job.JobName)
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

// nameKeys edits the job's name and returns the keys left over: enter
// saves, esc or ctrl+c cancels, and an empty name removes it.
func (at *attachment) nameKeys(b []byte) []byte {
	for i, k := range b {
		switch {
		case k == '\r' || k == '\n':
			at.naming = false
			at.saveName(string(at.name))
			at.drawToolbar()
			return b[i+1:]
		case k == keyCtrlC || (k == keyEsc && i == len(b)-1):
			at.naming = false
			at.drawToolbar()
			return b[i+1:]
		case k == keyEsc:
			// An arrow or other special key, or the terminal answering:
			// skipped.
			return b[i+escapeLen(b[i:]):]
		case k == 0x7f || k == 0x08:
			if len(at.name) > 0 {
				at.name = at.name[:len(at.name)-1]
			}
		case k == 0x15: // ctrl+u
			at.name = nil
		case k >= 0x20 && len(at.name) < 32:
			at.name = append(at.name, rune(k))
		}
	}
	at.drawToolbar()
	return nil
}

// escapeLen is the length of the escape sequence at the start of b, or
// all of b when it's cut off: CSI (esc [), OSC, DCS and other strings
// ended by BEL or esc \, SS3 (esc O) and two-byte escapes.
func escapeLen(b []byte) int {
	if len(b) < 2 {
		return len(b)
	}
	switch b[1] {
	case '[':
		for i := 2; i < len(b); i++ {
			if b[i] >= 0x40 && b[i] <= 0x7e {
				return i + 1
			}
		}
	case ']', 'P', '_', '^', 'X':
		for i := 2; i < len(b); i++ {
			if b[i] == 0x07 {
				return i + 1
			}
			if b[i] == keyEsc && i+1 < len(b) && b[i+1] == '\\' {
				return i + 2
			}
		}
	case 'O':
		return min(3, len(b))
	default:
		return 2
	}
	return len(b)
}

func (at *attachment) saveName(name string) {
	if name == at.job.JobName {
		return
	}
	if name != "" {
		if err := jobs.CheckName(at.app.store.JobsDir(), name, at.job.ID); err != nil {
			at.note = "not renamed: " + err.Error()
			return
		}
	}
	if err := at.conn.SetName(name); err != nil {
		at.note = "not renamed: " + err.Error()
		return
	}
	// Only say so once the helper has saved it: one from before names
	// existed ignores the request.
	for deadline := time.Now().Add(time.Second); ; time.Sleep(20 * time.Millisecond) {
		if m, err := at.job.Reload(); err == nil && m.JobName == name {
			break
		}
		if time.Now().After(deadline) {
			at.note = "not renamed: this job was started by an older mm; jobs started from now on can be named"
			return
		}
	}
	at.job.JobName = name
	at.note = "named " + name
	if name == "" {
		at.note = "name removed"
	}
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
// when the job may have reset or cleared them. An escape sequence cut off
// at the end is held back until the rest arrives, so mm's own sequences
// never land in the middle of one.
func (at *attachment) output(b []byte) {
	b = append(at.carry, b...)
	at.carry = nil
	if endsMidEscape(b) {
		if i := bytes.LastIndexByte(b, 0x1b); len(b)-i < 4096 {
			at.carry = append([]byte(nil), b[i:]...)
			b = b[:i]
		}
	}
	switched := false
	if at.foreground {
		// Inline, a full-screen job uses the terminal's own alternate
		// screen, and the toolbar follows it there and back.
		if i, _, _ := nextScreenSwitch(b); i >= 0 {
			switched, at.alt = true, endsOnAlt(b)
		}
		if len(b) > 0 {
			at.midLine = b[len(b)-1] != '\n'
		}
	} else {
		b = ownScreen(b)
	}
	at.out.Write(b)
	if switched || scanScreen(b) {
		at.fixScreen()
	}
}

// ownScreen keeps a full-screen job on mm's alternate screen: switching
// screens would otherwise take the job's output, or the toolbar, away.
// Entering or leaving clears the screen instead.
func ownScreen(b []byte) []byte {
	if !bytes.Contains(b, []byte("\x1b[?")) {
		return b
	}
	for _, mode := range []string{"1049", "1047", "47"} {
		for _, end := range []string{"h", "l"} {
			b = bytes.ReplaceAll(b, []byte("\x1b[?"+mode+end), []byte("\x1b[H\x1b[2J"))
		}
	}
	return b
}

// plainScreen keeps what output would have left on the main screen: what
// a full-screen program drew on the alternate screen is dropped, as the
// terminal would have dropped it.
func plainScreen(b []byte) []byte {
	if !bytes.Contains(b, []byte("\x1b[?")) {
		return b
	}
	var out []byte
	alt := false
	for len(b) > 0 {
		i, n, enter := nextScreenSwitch(b)
		if i < 0 {
			if !alt {
				out = append(out, b...)
			}
			break
		}
		if !alt {
			out = append(out, b[:i]...)
		}
		alt, b = enter, b[i+n:]
	}
	return out
}

// endsOnAlt is true when the last screen switch in b enters the alternate
// screen.
func endsOnAlt(b []byte) bool {
	alt := false
	for {
		i, n, enter := nextScreenSwitch(b)
		if i < 0 {
			return alt
		}
		alt, b = enter, b[i+n:]
	}
}

// nextScreenSwitch finds the first switch to or from the alternate screen
// in b: where it is, how long, and whether it enters.
func nextScreenSwitch(b []byte) (at, n int, enter bool) {
	at = -1
	for _, mode := range []string{"1049", "1047", "47"} {
		for _, end := range []string{"h", "l"} {
			seq := "\x1b[?" + mode + end
			if i := bytes.Index(b, []byte(seq)); i >= 0 && (at < 0 || i < at) {
				at, n, enter = i, len(seq), end == "h"
			}
		}
	}
	return at, n, enter
}

// scanScreen is true when b may reset the scroll region or clear the
// toolbar row: a full reset, a scroll region or erasing the display.
func scanScreen(b []byte) bool {
	if bytes.Contains(b, []byte("\x1bc")) {
		return true
	}
	for i := 0; i+1 < len(b); i++ {
		if b[i] != 0x1b || b[i+1] != '[' {
			continue
		}
		j := i + 2
		for j < len(b) && b[j] >= 0x30 && b[j] <= 0x3f {
			j++
		}
		if j >= len(b) {
			return true
		}
		if b[j] == 'r' || b[j] == 'J' {
			return true
		}
	}
	return false
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
	mode, hints := " WATCHING ", "d detach · ctrl+c stop job · i type · n name"
	modeStyle := badge.Foreground(colAccent)
	switch {
	case at.naming:
		mode = " NAME "
		modeStyle = badge.Foreground(colWarn)
		hints = "job name: " + u.bold(string(at.name)) + "▏  " + u.faint("enter save · esc cancel · empty removes it")
	case at.typing:
		mode, hints = " TYPING ", "keys go to the job · esc to detach or name"
		if !at.foreground {
			hints = "keys go to the job · esc back to watching"
		}
		modeStyle = badge.Foreground(colOK)
	}
	if at.note != "" && !at.naming {
		hints = at.note
	}
	left := u.render(modeStyle, mode) + "  " + hints
	if !u.on {
		left = "[" + strings.TrimSpace(mode) + "]  " + hints
	}
	right := at.job.Title() + " · " + at.job.Macro + " "
	gap := at.cols - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 2 {
		return ansi.Truncate(left, at.cols, "…")
	}
	return left + strings.Repeat(" ", gap) + u.faint(right)
}

// teardown gives the terminal back as it was: full scroll region, modes a
// full-screen job may have left on turned off, and the main screen.
func (at *attachment) teardown() {
	var b strings.Builder
	if at.foreground {
		// Inline, the output stays: leave a full-screen job's screen, clear
		// the toolbar, and carry on below the output.
		if at.alt {
			b.WriteString("\x1b[?1049l")
		}
		fmt.Fprintf(&b, "\x1b7\x1b[r\x1b[%d;1H\x1b[2K\x1b8", at.rows)
		if at.midLine {
			b.WriteString("\r\n")
		}
	} else {
		b.WriteString("\x1b[r")
	}
	// Show the cursor; normal cursor keys and keypad; no bracketed paste or
	// mouse reporting; plain attributes.
	b.WriteString("\x1b[?25h\x1b[?1l\x1b>\x1b[?2004l\x1b[?1000l\x1b[?1002l\x1b[?1003l\x1b[?1006l\x1b[0m")
	if !at.foreground {
		b.WriteString("\x1b[?1049l")
	}
	at.write(b.String())
}
