package jobs

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"runtime"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
)

// Summary is a one-line account of how a finished job went, e.g.
// "finished after 3m12s" or "failed (exit 1) after 4s".
func (m *Meta) Summary() string {
	took := m.Duration()
	switch {
	case m.Error != "":
		return "failed to start: " + m.Error
	case m.State() == StateLost:
		return "lost: its helper stopped without recording an end"
	case m.Signal != 0:
		return fmt.Sprintf("%s after %s", m.Outcome(), took)
	case m.ExitCode == 0:
		return fmt.Sprintf("finished after %s", took)
	}
	return fmt.Sprintf("failed (exit %d) after %s", m.ExitCode, took)
}

// notification is what a desktop notification about an ended job says.
type notification struct {
	title    string // ✓ or ✗, the job and how it ended
	subtitle string // the job number and macro
	body     string // the last line of output
	ok       bool
	sound    bool
}

// notification describes how m ended, with the last line it printed.
func (m *Meta) notification() notification {
	ok := m.Error == "" && m.State() == StateExited && m.Signal == 0 && m.ExitCode == 0
	name := m.JobName
	if name == "" {
		name = path.Base(m.Name)
	}
	if name == "" || name == "." {
		name = m.Macro
	}
	mark := "✗"
	if ok {
		mark = "✓"
	}
	body := lastLine(m.LogPath())
	if body == "" {
		body = "No output"
	}
	sound := m.Sound
	if sound == "" {
		sound = "both"
	}
	return notification{
		title:    mark + " " + name + " " + m.Summary(),
		subtitle: m.Title() + " · " + m.Macro,
		body:     body,
		ok:       ok,
		sound:    sound == "both" || (ok && sound == "success") || (!ok && sound == "failure"),
	}
}

// Notify shows a desktop notification that m has ended. Missing tools are
// ignored: notifications are a nicety.
func Notify(m *Meta) {
	cmd := notifyCommand(runtime.GOOS, m.notification())
	if cmd == nil || cmd.Start() != nil {
		return
	}
	done := make(chan struct{})
	go func() { cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		cmd.Process.Kill()
	}
}

// notifyCommand is the command that shows n on goos, or nil when there's
// no way to.
func notifyCommand(goos string, n notification) *exec.Cmd {
	if goos == "darwin" {
		script := fmt.Sprintf("display notification %s with title %s subtitle %s",
			appleString(n.body), appleString(n.title), appleString(n.subtitle))
		if n.sound {
			sound := "Basso"
			if n.ok {
				sound = "Glass"
			}
			script += " sound name " + appleString(sound)
		}
		return exec.Command("osascript", "-e", script)
	}
	bin, err := exec.LookPath("notify-send")
	if err != nil {
		return nil
	}
	// Failures stay until dismissed. Icon and sound names are from the
	// freedesktop themes; servers without them show and play nothing.
	icon, urgency, sound := "dialog-error", "critical", "dialog-error"
	if n.ok {
		icon, urgency, sound = "utilities-terminal", "normal", "complete"
	}
	args := []string{"--app-name=mm", "--icon=" + icon, "--urgency=" + urgency}
	if n.sound {
		args = append(args, "--hint=string:sound-name:"+sound)
	}
	// Bodies may be read as markup, so output can't add any.
	body := markupEscaper.Replace(n.subtitle + "\n" + n.body)
	return exec.Command(bin, append(args, n.title, body)...)
}

var markupEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

// lastLine is the last line of output in the log at p that has any text,
// as it showed on screen: escape sequences dropped, and only what followed
// the last carriage return, as a progress bar redraws. Long lines are cut.
func lastLine(p string) string {
	f, err := os.Open(p)
	if err != nil {
		return ""
	}
	defer f.Close()
	const tail = 8 << 10
	if fi, err := f.Stat(); err == nil && fi.Size() > tail {
		f.Seek(-tail, io.SeekEnd)
	}
	b, _ := io.ReadAll(f)
	lines := strings.Split(string(b), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimRight(lines[i], "\r")
		if j := strings.LastIndexByte(l, '\r'); j >= 0 {
			l = l[j+1:]
		}
		l = strings.TrimSpace(strings.Map(func(r rune) rune {
			if unicode.IsControl(r) || r == utf8.RuneError {
				return -1
			}
			return r
		}, ansi.Strip(l)))
		if l == "" {
			continue
		}
		if r := []rune(l); len(r) > 200 {
			l = string(r[:199]) + "…"
		}
		return l
	}
	return ""
}

func appleString(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}
