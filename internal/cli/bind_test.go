package cli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"macro-master/internal/hotkey"
	"macro-master/internal/store"
)

func needHotkeys(t *testing.T) {
	t.Helper()
	if !hotkey.Supported {
		t.Skip("global hotkeys aren't supported here")
	}
}

// bindings reads the key bindings saved in config.json.
func (ta *testApp) bindings(t *testing.T) map[string]store.Binding {
	t.Helper()
	s, err := store.Open(ta.Home)
	if err != nil {
		t.Fatal(err)
	}
	return s.Config.Bindings
}

// fakeDaemon writes a hotkey status file as a running daemon (this test
// process) would.
func (ta *testApp) fakeDaemon(t *testing.T, keys map[string]string) {
	t.Helper()
	data, _ := json.Marshal(hotkeyStatus{PID: os.Getpid(), Exe: "/bin/mm", Keys: keys})
	if err := os.WriteFile(filepath.Join(ta.Home, "hotkeys.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestBindAddLsShowMvRm(t *testing.T) {
	needHotkeys(t)
	ta := newApp(t)
	ta.save(t, "personal/web/deploy", "echo deploy {{env:dev}}\n")

	if code := ta.run(nil, "bind", "ls"); code != 0 || !strings.Contains(ta.err.String(), "No key bindings yet") {
		t.Errorf("empty ls: %d %q", code, ta.err)
	}
	if code := ta.run(nil, "bind", "add", "cmd+shift+d", "deploy", "env=prod"); code != 0 {
		t.Fatalf("add: %s", ta.err)
	}
	if got := ta.bindings(t)["shift+cmd+d"]; got.Macro != "personal/web/deploy" || !slices.Equal(got.Args, []string{"env=prod"}) {
		t.Errorf("saved %+v", ta.bindings(t))
	}
	if !strings.Contains(ta.err.String(), "mm bind service install") {
		t.Errorf("no hint to start the service: %q", ta.err)
	}
	// Adding it again changes nothing.
	if code := ta.run(nil, "bind", "add", "shift+cmd+d", "deploy", "env=prod"); code != 0 || !strings.Contains(ta.err.String(), "already runs") {
		t.Errorf("re-add: %d %q", code, ta.err)
	}

	ta.run(nil, "bind", "ls")
	for _, want := range []string{"⇧⌘D", "shift+cmd+d", "personal/web/deploy", "env=prod", "service not running"} {
		if !strings.Contains(ta.out.String(), want) {
			t.Errorf("ls lacks %q: %q", want, ta.out)
		}
	}
	ta.fakeDaemon(t, map[string]string{"shift+cmd+d": ""})
	if ta.run(nil, "bind", "ls"); !strings.Contains(ta.out.String(), "active") {
		t.Errorf("ls with the service: %q", ta.out)
	}
	ta.fakeDaemon(t, map[string]string{"shift+cmd+d": "⇧⌘D is already taken by another app"})
	if ta.run(nil, "bind", "ls"); !strings.Contains(ta.out.String(), "already taken") {
		t.Errorf("ls with a failed key: %q", ta.out)
	}

	if ta.run(nil, "show", "deploy"); !strings.Contains(ta.out.String(), "⇧⌘D env=prod") {
		t.Errorf("show: %q", ta.out)
	}

	// Adding it again with --quiet only turns notifications off, without
	// asking; adding it without turns them back on.
	if code := ta.run(nil, "bind", "add", "cmd+shift+d", "deploy", "env=prod", "--quiet"); code != 0 || !ta.bindings(t)["shift+cmd+d"].Quiet {
		t.Errorf("quiet: %d %q %+v", code, ta.err, ta.bindings(t))
	}
	if !strings.Contains(ta.err.String(), "without notifications") {
		t.Errorf("quiet said %q", ta.err)
	}
	if ta.run(nil, "bind", "ls"); !strings.Contains(ta.out.String(), "quiet") {
		t.Errorf("ls doesn't show quiet: %q", ta.out)
	}
	if ta.run(nil, "show", "deploy"); !strings.Contains(ta.out.String(), "⇧⌘D env=prod (quiet)") {
		t.Errorf("show quiet: %q", ta.out)
	}
	if code := ta.run(nil, "bind", "add", "cmd+shift+d", "deploy", "env=prod"); code != 0 || ta.bindings(t)["shift+cmd+d"].Quiet {
		t.Errorf("unquiet: %d %q %+v", code, ta.err, ta.bindings(t))
	}

	// Moving the macro takes the binding along.
	if code := ta.run(nil, "mv", "deploy", "ops/"); code != 0 {
		t.Fatalf("mv: %s", ta.err)
	}
	if got := ta.bindings(t)["shift+cmd+d"].Macro; got != "personal/ops/deploy" {
		t.Errorf("after mv, binding runs %q", got)
	}

	if code := ta.run(nil, "bind", "rm", "cmd+shift+d"); code != 0 || len(ta.bindings(t)) != 0 {
		t.Errorf("rm: %d %s %v", code, ta.err, ta.bindings(t))
	}
	if code := ta.run(nil, "bind", "rm", "cmd+shift+d"); code == 0 || !strings.Contains(ta.err.String(), "nothing is bound") {
		t.Errorf("rm again: %d %q", code, ta.err)
	}

	// Deleting the macro removes its bindings.
	ta.run(nil, "bind", "add", "ctrl+alt+f5", "deploy")
	if code := ta.run(ans("y\n"), "rm", "deploy"); code != 0 {
		t.Fatalf("rm macro: %s", ta.err)
	}
	if len(ta.bindings(t)) != 0 || !strings.Contains(ta.err.String(), "⌃⌥F5") {
		t.Errorf("after rm, bindings %v, err %q", ta.bindings(t), ta.err)
	}
}

func TestBindAddChecks(t *testing.T) {
	needHotkeys(t)
	ta := newApp(t)
	ta.save(t, "personal/cdproj", "# mode: source\ncd ~/proj\n")
	ta.save(t, "personal/greet", "echo hi {{who}}\n")
	ta.save(t, "personal/other", "echo other\n")

	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"cmd+shift+c", "cdproj"}, "mode: source"},
		{[]string{"cmd+shift+g", "greet"}, "who=<value>"},
		{[]string{"shift+g", "greet", "who=x"}, "ordinary typing"},
		{[]string{"cmd+shift+g", "nope"}, "nope"},
		{[]string{"cmd+shift+g"}, "usage"},
		{[]string{"cmd+c", "other"}, "Copy in macOS"},
	} {
		if code := ta.run(nil, append([]string{"bind", "add"}, tc.args...)...); code == 0 || !strings.Contains(ta.err.String(), tc.want) {
			t.Errorf("%v: exit %d, err %q, want %q", tc.args, code, ta.err, tc.want)
		}
	}
	if len(ta.bindings(t)) != 0 {
		t.Errorf("refused bindings were saved: %v", ta.bindings(t))
	}

	// A well-known shortcut needs a yes.
	if code := ta.run(ans("n\n"), "bind", "add", "cmd+space", "other"); code == 0 || len(ta.bindings(t)) != 0 {
		t.Errorf("cmd+space declined: %d %v", code, ta.bindings(t))
	}
	if code := ta.run(ans("y\n"), "bind", "add", "cmd+space", "other"); code != 0 || ta.bindings(t)["cmd+space"].Macro != "personal/other" {
		t.Errorf("cmd+space accepted: %d %q %v", code, ta.err, ta.bindings(t))
	}
	ta.run(nil, "bind", "rm", "cmd+space")

	// An alias binds what it runs, with its presets.
	ta.run(nil, "alias", "add", "hi", "greet", "who=amy")
	if code := ta.run(nil, "bind", "add", "cmd+shift+g", "hi"); code != 0 {
		t.Fatalf("bind alias: %s", ta.err)
	}
	if got := ta.bindings(t)["shift+cmd+g"]; got.Macro != "personal/greet" || !slices.Equal(got.Args, []string{"who=amy"}) {
		t.Errorf("alias binding saved as %+v", got)
	}

	// Rebinding keys asks first, and refuses without a terminal.
	if code := ta.run(nil, "bind", "add", "cmd+shift+g", "other"); code == 0 || !strings.Contains(ta.err.String(), "mm bind rm") {
		t.Errorf("rebind without terminal: %d %q", code, ta.err)
	}
	if code := ta.run(ans("y\n"), "bind", "add", "cmd+shift+g", "other"); code != 0 || ta.bindings(t)["shift+cmd+g"].Macro != "personal/other" {
		t.Errorf("rebind: %d %q %v", code, ta.err, ta.bindings(t))
	}
}

func TestBindFire(t *testing.T) {
	needHotkeys(t)
	ta := newJobApp(t)
	var notes []string
	ta.Notify = func(title, subtitle, body string, ok bool) {
		notes = append(notes, title+" | "+subtitle+" | "+body)
	}
	ta.save(t, "personal/greet", "echo \"hi {{who:you}}\"\n")
	if code := ta.run(nil, "bind", "add", "ctrl+alt+g", "greet", "who=keys"); code != 0 {
		t.Fatalf("add: %s", ta.err)
	}
	if code := ta.run(nil, "__bind", "ctrl+alt+g"); code != 0 {
		t.Fatalf("__bind: %s", ta.err)
	}
	m := ta.waitEnd(t, "1")
	if m.ExitCode != 0 || m.Macro != "personal/greet" {
		t.Errorf("job: %s %s", m.Macro, m.Outcome())
	}
	ta.run(nil, "attach", "1")
	if !strings.Contains(ta.out.String(), "hi keys") {
		t.Errorf("job output %q", ta.out)
	}
	if len(notes) != 0 {
		t.Errorf("notified on success: %v", notes)
	}

	if m.Notify != "off" { // newJobApp turns them off in config.json
		t.Errorf("job notify = %q", m.Notify)
	}

	// A quiet binding's job doesn't notify, even when config.json would
	// (bell, so the test shows no real notification).
	os.WriteFile(filepath.Join(ta.Home, "config.json"), []byte(`{"default_library":"personal","jobs":{"notify":"bell"},
		"bindings":{"ctrl+alt+g":{"macro":"personal/greet"},"ctrl+alt+q":{"macro":"personal/greet","quiet":true}}}`), 0o644)
	ta.run(nil, "__bind", "ctrl+alt+q")
	if m := ta.waitEnd(t, "2"); m.Notify != "off" {
		t.Errorf("quiet job notify = %q", m.Notify)
	}
	ta.run(nil, "__bind", "ctrl+alt+g")
	if m := ta.waitEnd(t, "3"); m.Notify != "bell" {
		t.Errorf("loud job notify = %q", m.Notify)
	}

	// A binding whose macro has gone fails with a notification, unless
	// it's quiet.
	os.Remove(filepath.Join(ta.Home, "libraries", "personal", "greet.sh"))
	if code := ta.run(nil, "__bind", "ctrl+alt+g"); code == 0 {
		t.Error("__bind of a missing macro succeeded")
	}
	if code := ta.run(nil, "__bind", "ctrl+alt+q"); code == 0 {
		t.Error("quiet __bind of a missing macro succeeded")
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "⌃⌥G didn't run") || !strings.Contains(notes[0], "doesn't exist any more") {
		t.Errorf("notes = %v", notes)
	}
}

func TestHotkeyDaemonReload(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	var registered []string
	var fired []string
	d := &hotkeyDaemon{
		configPath: cfg,
		statusPath: filepath.Join(dir, "hotkeys.json"),
		exe:        "/bin/mm",
		register: func(id uint32, c hotkey.Combo) error {
			if c.Key == "x" {
				return errors.New("taken")
			}
			registered = append(registered, c.String())
			return nil
		},
		unregister: func(id uint32) {
			registered = slices.DeleteFunc(registered, func(s string) bool { return s == "shift+cmd+d" })
		},
		fire: func(keys string) { fired = append(fired, keys) },
	}
	write := func(s string) {
		t.Helper()
		if err := os.WriteFile(cfg, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	status := func() map[string]string {
		t.Helper()
		var st hotkeyStatus
		data, _ := os.ReadFile(d.statusPath)
		json.Unmarshal(data, &st)
		return st.Keys
	}

	write(`{"default_library":"personal","bindings":{
		"shift+cmd+d":{"macro":"personal/deploy"},
		"cmd+x":{"macro":"personal/cut"}}}`)
	d.reload()
	if !slices.Equal(registered, []string{"shift+cmd+d"}) {
		t.Errorf("registered %v", registered)
	}
	if st := status(); st["shift+cmd+d"] != "" || st["cmd+x"] != "taken" {
		t.Errorf("status %v", st)
	}

	// A press runs the binding; a second one straight after is dropped.
	now := time.Now()
	id := d.ids["shift+cmd+d"]
	d.pressed(id, now)
	d.pressed(id, now.Add(100*time.Millisecond))
	d.pressed(id, now.Add(time.Second))
	d.pressed(999, now)
	if !slices.Equal(fired, []string{"shift+cmd+d", "shift+cmd+d"}) {
		t.Errorf("fired %v", fired)
	}

	// A config mm can't read keeps the hotkeys; a removed binding goes.
	write(`{not json`)
	d.reload()
	if len(registered) != 1 {
		t.Errorf("bad config changed hotkeys: %v", registered)
	}
	write(`{"default_library":"personal"}`)
	d.reload()
	if len(registered) != 0 || len(status()) != 0 {
		t.Errorf("after removal: %v %v", registered, status())
	}
}

func TestBindServiceInstall(t *testing.T) {
	needHotkeys(t)
	ta := newApp(t)
	userHome := t.TempDir()
	ta.env["HOME"] = userHome
	ta.env["PATH"] = "/opt/homebrew/bin:/usr/bin:/bin"
	var calls []string
	ta.Launchctl = func(args ...string) error {
		calls = append(calls, strings.Join(args, " "))
		if args[0] == "bootstrap" {
			ta.fakeDaemon(t, map[string]string{})
		}
		return nil
	}
	if code := ta.run(nil, "bind", "service", "status"); code != 0 || !strings.Contains(ta.out.String(), "not installed") {
		t.Errorf("status before: %d %q", code, ta.out)
	}
	if code := ta.run(nil, "bind", "service", "install"); code != 0 {
		t.Fatalf("install: %s", ta.err)
	}
	if !strings.Contains(ta.err.String(), "Hotkey service running") {
		t.Errorf("install said %q", ta.err)
	}
	agents, _ := filepath.Glob(filepath.Join(userHome, "Library", "LaunchAgents", serviceLabel+".*.plist"))
	if len(agents) != 1 {
		t.Fatalf("launch agents: %v", agents)
	}
	plist, _ := os.ReadFile(agents[0])
	for _, want := range []string{
		"<string>__hotkeyd</string>",
		"<key>MM_HOME</key>\n\t\t<string>" + ta.Home + "</string>",
		"<key>PATH</key>\n\t\t<string>/opt/homebrew/bin:/usr/bin:/bin</string>",
		"<string>" + filepath.Join(ta.Home, "hotkeys.log") + "</string>",
		"<key>SuccessfulExit</key>\n\t\t<false/>",
	} {
		if !strings.Contains(string(plist), want) {
			t.Errorf("plist lacks %q:\n%s", want, plist)
		}
	}
	label := strings.TrimSuffix(filepath.Base(agents[0]), ".plist")
	if len(calls) != 2 || !strings.HasPrefix(calls[0], "bootout gui/") || !strings.HasSuffix(calls[0], "/"+label) || !strings.HasSuffix(calls[1], agents[0]) {
		t.Errorf("launchctl calls %v", calls)
	}

	if code := ta.run(nil, "bind", "service"); code != 0 || !strings.Contains(ta.out.String(), "running (pid") {
		t.Errorf("status: %d %q", code, ta.out)
	}

	calls = nil
	if code := ta.run(nil, "bind", "service", "uninstall"); code != 0 {
		t.Fatalf("uninstall: %s", ta.err)
	}
	if _, err := os.Stat(agents[0]); !os.IsNotExist(err) {
		t.Error("launch agent left behind")
	}
	if len(calls) != 1 || !strings.HasPrefix(calls[0], "bootout") {
		t.Errorf("uninstall calls %v", calls)
	}
}

func TestBindUnsupported(t *testing.T) {
	if hotkey.Supported {
		t.Skip("hotkeys are supported here")
	}
	ta := newApp(t)
	ta.save(t, "personal/greet", "echo hi\n")
	if code := ta.run(nil, "bind", "add", "cmd+g", "greet"); code == 0 || !strings.Contains(ta.err.String(), "only work on macOS") {
		t.Errorf("bind add: %d %q", code, ta.err)
	}
}

func TestCompleteBind(t *testing.T) {
	ta := newApp(t)
	ta.save(t, "personal/greet", "echo hi {{who}}\n")
	s, err := store.Open(ta.Home)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		words []string
		want  []string
		not   []string
	}{
		{[]string{"bind", ""}, []string{"add", "ls", "rm", "service"}, nil},
		{[]string{"bind", "add", ""}, []string{"cmd+", "ctrl+"}, []string{"d"}},
		{[]string{"bind", "add", "cmd+"}, []string{"cmd+shift+", "cmd+d", "cmd+f5"}, []string{"cmd+cmd+"}},
		{[]string{"bind", "add", "cmd+d", ""}, []string{"greet"}, nil},
		{[]string{"bind", "add", "cmd+d", "greet", ""}, []string{"who=", "--quiet"}, nil},
		{[]string{"bind", "service", ""}, []string{"install", "uninstall", "status"}, nil},
	} {
		got := complete(s, tc.words)
		for _, w := range tc.want {
			if !slices.Contains(got, w) {
				t.Errorf("%v: %v lacks %q", tc.words, got, w)
			}
		}
		for _, w := range tc.not {
			if slices.Contains(got, w) {
				t.Errorf("%v: %v has %q", tc.words, got, w)
			}
		}
	}
}
