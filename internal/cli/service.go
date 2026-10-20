package cli

import (
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"html"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"macro-master/internal/hotkey"
)

// serviceLabel is the hotkey daemon's launchd label. A home other than
// the default gets its own, so a test home can't replace the real one.
const serviceLabel = "com.github.jessejwatson.mm.hotkeys"

func (a *App) serviceLabel() string {
	if a.Home == "" && a.Getenv("MM_HOME") == "" {
		return serviceLabel
	}
	sum := sha1.Sum([]byte(a.store.Home))
	return serviceLabel + "." + hex.EncodeToString(sum[:4])
}

func (a *App) launchAgentPath() (string, error) {
	home := a.Getenv("HOME")
	if home == "" {
		var err error
		if home, err = os.UserHomeDir(); err != nil {
			return "", err
		}
	}
	return filepath.Join(home, "Library", "LaunchAgents", a.serviceLabel()+".plist"), nil
}

func (a *App) cmdBindService(args []string) error {
	if len(args) == 0 {
		return a.serviceStatus()
	}
	if len(args) > 1 {
		return errors.New("usage: mm bind service [install|uninstall|status]")
	}
	switch args[0] {
	case "install":
		return a.serviceInstall()
	case "uninstall":
		return a.serviceUninstall()
	case "status":
		return a.serviceStatus()
	}
	return fmt.Errorf("unknown service command %q; use install, uninstall or status", args[0])
}

// stableExe is the path launchd should run mm by: the one on PATH when it
// leads to this binary, as Homebrew's /opt/homebrew/bin/mm does, so the
// service survives upgrades that replace the versioned file it links to.
func (a *App) stableExe() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if real, err := filepath.EvalSymlinks(exe); err == nil {
		exe = real
	}
	if p, err := a.lookPath("mm"); err == nil && filepath.IsAbs(p) {
		if real, err := filepath.EvalSymlinks(p); err == nil && real == exe {
			return p, nil
		}
	}
	return exe, nil
}

// launchAgent renders the launchd property list that runs the daemon at
// login and restarts it if it stops unexpectedly. launchd starts agents
// with almost no environment, so PATH is the one mm bind service install
// ran with, for macros to find the same commands as in a terminal.
func (a *App) launchAgent(exe string) string {
	var env strings.Builder
	add := func(k, v string) {
		fmt.Fprintf(&env, "\t\t<key>%s</key>\n\t\t<string>%s</string>\n", k, html.EscapeString(v))
	}
	add("MM_HOME", a.store.Home)
	for _, k := range []string{"PATH", "LANG", "LC_ALL", "LC_CTYPE"} {
		if v := a.Getenv(k); v != "" {
			add(k, v)
		}
	}
	str := func(s string) string { return "<string>" + html.EscapeString(s) + "</string>" }
	log := a.store.HotkeyLogPath()
	home := a.Getenv("HOME")
	if home == "" {
		home = "/"
	}
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	` + str(a.serviceLabel()) + `
	<key>ProgramArguments</key>
	<array>
		` + str(exe) + `
		` + str(hotkey.DaemonArg) + `
	</array>
	<key>EnvironmentVariables</key>
	<dict>
` + env.String() + `	</dict>
	<key>WorkingDirectory</key>
	` + str(home) + `
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<dict>
		<key>SuccessfulExit</key>
		<false/>
	</dict>
	<key>ProcessType</key>
	<string>Interactive</string>
	<key>LimitLoadToSessionType</key>
	<string>Aqua</string>
	<key>StandardOutPath</key>
	` + str(log) + `
	<key>StandardErrorPath</key>
	` + str(log) + `
</dict>
</plist>
`
}

func (a *App) launchctl(args ...string) error {
	if a.Launchctl != nil {
		return a.Launchctl(args...)
	}
	out, err := exec.Command("launchctl", args...).CombinedOutput()
	if err != nil {
		if msg := strings.TrimSpace(string(out)); msg != "" {
			return fmt.Errorf("launchctl %s: %s", args[0], msg)
		}
		return fmt.Errorf("launchctl %s: %w", args[0], err)
	}
	return nil
}

func (a *App) launchDomain() string { return fmt.Sprintf("gui/%d", os.Getuid()) }

// serviceInstall writes the launch agent and (re)starts the daemon, which
// also refreshes the PATH it runs macros with.
func (a *App) serviceInstall() error {
	if !hotkey.Supported {
		return hotkey.ErrUnsupported
	}
	exe, err := a.stableExe()
	if err != nil {
		return err
	}
	path, err := a.launchAgentPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(a.launchAgent(exe)), 0o644); err != nil {
		return err
	}
	old := a.hotkeyStatus()
	target := a.launchDomain() + "/" + a.serviceLabel()
	a.launchctl("bootout", target) // not loaded yet is fine
	// bootout returns before the old daemon is gone, and bootstrap fails
	// until it is, so try for a few seconds.
	deadline := time.Now().Add(5 * time.Second)
	for {
		err = a.launchctl("bootstrap", a.launchDomain(), path)
		if err == nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if err != nil {
		return fmt.Errorf("can't start the hotkey service: %w", err)
	}

	st, ok := a.waitHotkeyd(old.PID)
	u := a.errUI()
	if !ok {
		return fmt.Errorf("the hotkey service didn't start; see %s", a.store.HotkeyLogPath())
	}
	a.done("Hotkey service running (pid %d); it starts again at login.", st.PID)
	a.reportHotkeys(st)
	fmt.Fprintln(a.Stderr, u.faint("  Macros run with the PATH from this terminal; run mm bind service install again after changing it."))
	return nil
}

// waitHotkeyd waits for a daemon other than pid old to report what it
// registered.
func (a *App) waitHotkeyd(old int) (hotkeyStatus, bool) {
	deadline := time.Now().Add(5 * time.Second)
	for {
		st := a.hotkeyStatus()
		if st.PID != 0 && st.PID != old && st.running() {
			return st, true
		}
		if time.Now().After(deadline) {
			return st, false
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// reportHotkeys says how many bindings the daemon has, and which failed.
func (a *App) reportHotkeys(st hotkeyStatus) {
	if len(a.store.Config.Bindings) == 0 {
		a.notef("No key bindings yet. Add one with: mm bind add <keys> <macro>")
		return
	}
	active := 0
	for keys, e := range st.Keys {
		if e == "" {
			active++
			continue
		}
		a.warnf("%s: %s", comboOf(keys).Symbols(), e)
	}
	fmt.Fprintf(a.Stderr, "  %d of %d %s active · mm bind ls lists them\n", active, len(a.store.Config.Bindings), plural(len(a.store.Config.Bindings), "binding", "bindings"))
}

func (a *App) serviceUninstall() error {
	path, err := a.launchAgentPath()
	if err != nil {
		return err
	}
	_, statErr := os.Stat(path)
	running := a.hotkeyStatus().running()
	if statErr != nil && !running {
		a.nothing("The hotkey service isn't installed.")
		return nil
	}
	a.launchctl("bootout", a.launchDomain()+"/"+a.serviceLabel())
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	os.Remove(a.store.HotkeyStatusPath())
	a.done("Hotkey service stopped and removed; key bindings are kept for next time.")
	return nil
}

func (a *App) serviceStatus() error {
	path, err := a.launchAgentPath()
	if err != nil {
		return err
	}
	_, statErr := os.Stat(path)
	st := a.hotkeyStatus()
	u := a.outUI()
	switch {
	case st.running():
		fmt.Fprintf(a.Stdout, "%s running (pid %d, %s)\n", u.fg(colOK, "Hotkey service"), st.PID, st.Exe)
	case statErr == nil:
		fmt.Fprintf(a.Stdout, "%s installed but not running; see %s\n", u.fg(colWarn, "Hotkey service"), a.store.HotkeyLogPath())
	default:
		fmt.Fprintf(a.Stdout, "%s not installed; start it with mm bind service install\n", u.fg(colWarn, "Hotkey service"))
		return nil
	}
	fmt.Fprintf(a.Stdout, "Launch agent: %s\nLog:          %s\n", path, a.store.HotkeyLogPath())
	if st.running() {
		a.reportHotkeys(st)
	}
	return nil
}
