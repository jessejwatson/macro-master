package jobs

import (
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"time"
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

// Notify shows a desktop notification that m has ended. Missing tools are
// ignored: notifications are a nicety.
func Notify(m *Meta) {
	title := "mm: " + m.Macro
	body := fmt.Sprintf("Job %s %s", m.ID, m.Summary())
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("osascript", "-e",
			fmt.Sprintf("display notification %s with title %s", appleString(body), appleString(title)))
	default:
		path, err := exec.LookPath("notify-send")
		if err != nil {
			return
		}
		cmd = exec.Command(path, "--app-name=mm", title, body)
	}
	if cmd.Start() != nil {
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

func appleString(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}
