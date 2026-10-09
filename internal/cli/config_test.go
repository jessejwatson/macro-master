package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"macro-master/internal/store"
)

func key(s string) tea.KeyPressMsg {
	switch s {
	case "enter":
		return tea.KeyPressMsg{Code: tea.KeyEnter}
	case "esc":
		return tea.KeyPressMsg{Code: tea.KeyEscape}
	case "down":
		return tea.KeyPressMsg{Code: tea.KeyDown}
	case "up":
		return tea.KeyPressMsg{Code: tea.KeyUp}
	case "left":
		return tea.KeyPressMsg{Code: tea.KeyLeft}
	case "right":
		return tea.KeyPressMsg{Code: tea.KeyRight}
	case "backspace":
		return tea.KeyPressMsg{Code: tea.KeyBackspace}
	}
	r := []rune(s)[0]
	return tea.KeyPressMsg{Code: r, Text: s}
}

// press feeds keys to the panel; text is typed a rune at a time.
func press(m configModel, keys ...string) configModel {
	for _, k := range keys {
		if strings.HasPrefix(k, "type:") {
			for _, r := range strings.TrimPrefix(k, "type:") {
				next, _ := m.Update(key(string(r)))
				m = next.(configModel)
			}
			continue
		}
		next, _ := m.Update(key(k))
		m = next.(configModel)
	}
	return m
}

// panel opens the settings panel on a fresh store with a host and a second
// library.
func panel(t *testing.T) (*testApp, configModel) {
	t.Helper()
	ta := newApp(t)
	ta.run(nil, "lib", "add", "work")
	ta.store.UpdateConfig(func(c *store.Config) {
		c.Hosts = map[string]store.HostConfig{"git.example.com": {Type: "gitea"}}
	})
	return ta, newConfigModel(ta.App, ta.newUI(false))
}

// row moves the cursor to the setting with key k.
func row(t *testing.T, m configModel, k string) configModel {
	t.Helper()
	for i, s := range m.items {
		if s.key == k {
			m.cursor = i
			return m
		}
	}
	t.Fatalf("no setting %s", k)
	return m
}

func TestConfigPanelChoicesAndText(t *testing.T) {
	ta, m := panel(t)
	keys := []string{}
	for _, s := range m.items {
		keys = append(keys, s.key)
	}
	want := "default_library sync_interval jobs.notify jobs.keep_for jobs.keep_max hosts.git.example.com.type hosts.git.example.com.api"
	if strings.Join(keys, " ") != want {
		t.Fatalf("settings = %v", keys)
	}

	m = press(m, "down", "down", "right") // notify: desktop -> bell
	if ta.store.Config.Jobs.Notify != "bell" || !strings.Contains(m.note, "Set jobs.notify to bell") {
		t.Errorf("notify = %q, note %q", ta.store.Config.Jobs.Notify, m.note)
	}
	m = press(m, "left", "left") // bell -> desktop -> off
	if ta.store.Config.Jobs.Notify != "off" {
		t.Errorf("notify after left = %q", ta.store.Config.Jobs.Notify)
	}
	m = press(m, "up", "up", "enter") // default library cycles to work
	if ta.store.Config.DefaultLibrary != "work" {
		t.Errorf("default library = %q", ta.store.Config.DefaultLibrary)
	}

	m = row(t, m, "jobs.keep_for")
	m = press(m, "enter", "type:3d", "enter")
	if ta.store.Config.Jobs.KeepFor != "3d" || m.editing {
		t.Errorf("keep_for = %q, editing %v", ta.store.Config.Jobs.KeepFor, m.editing)
	}
	// A bad value is refused and the input stays open.
	m = press(m, "enter", "type:x", "enter")
	if ta.store.Config.Jobs.KeepFor != "3d" || !m.editing || !m.noteBad || !strings.Contains(m.note, "isn't a length of time") {
		t.Errorf("bad keep_for: %q editing=%v note=%q", ta.store.Config.Jobs.KeepFor, m.editing, m.note)
	}
	m = press(m, "esc")
	if m.editing {
		t.Error("esc didn't cancel")
	}

	m = row(t, m, "jobs.keep_max")
	m = press(m, "enter", "type:0", "enter")
	if km := ta.store.Config.Jobs.KeepMax; km == nil || *km != 0 {
		t.Errorf("keep_max = %v", km)
	}
	m = press(m, "backspace")
	if ta.store.Config.Jobs.KeepMax != nil {
		t.Error("keep_max not reset")
	}

	m = row(t, m, "hosts.git.example.com.api")
	m = press(m, "enter", "type:ftp://x", "enter")
	if !m.noteBad {
		t.Error("non-http API accepted")
	}
	m = press(m, "esc", "enter", "type:https://git.example.com/api/v1", "enter")
	if got := ta.store.Config.Hosts["git.example.com"]; got.API != "https://git.example.com/api/v1" || got.Type != "gitea" {
		t.Errorf("host = %+v", got)
	}

	// Saved for real, not just in memory.
	b, _ := os.ReadFile(filepath.Join(ta.Home, "config.json"))
	for _, s := range []string{`"default_library": "work"`, `"notify": "off"`, `"keep_for": "3d"`, `"api": "https://git.example.com/api/v1"`} {
		if !strings.Contains(string(b), s) {
			t.Errorf("config.json lacks %s:\n%s", s, b)
		}
	}

	view := m.render()
	for _, s := range []string{"Default library", "work", "Keep at most", "50 default", "git.example.com type"} {
		if !strings.Contains(view, s) {
			t.Errorf("view lacks %q:\n%s", s, view)
		}
	}
	if m := press(m, "e"); !m.openEditor || m.View().Content != "" {
		t.Error("e didn't hand over to the editor with the panel cleared")
	}
	if m := press(m, "q"); m.View().Content != "" {
		t.Errorf("panel left on screen after quitting: %q", m.View().Content)
	}
}

func TestConfigEditAndPath(t *testing.T) {
	ta := newApp(t)
	ta.run(nil, "ls") // create the store
	ed := filepath.Join(t.TempDir(), "ed.sh")
	// The editor writes whatever is in $NEXT into the file.
	os.WriteFile(ed, []byte("#!/bin/sh\nprintf '%s' \"$NEXT\" > \"$1\"\n"), 0o755)
	ta.env["EDITOR"] = ed

	os.Setenv("NEXT", `{"default_library":"personal","jobs":{"notify":"bell"}}`)
	defer os.Unsetenv("NEXT")
	if code := ta.run(nil, "config", "edit"); code != 0 {
		t.Fatalf("edit: %s", ta.err)
	}
	if ta.store.Config.Jobs.Notify != "bell" {
		t.Errorf("notify = %q", ta.store.Config.Jobs.Notify)
	}

	// Unknown settings and bad values are refused, and nothing is saved.
	for next, want := range map[string]string{
		`{"default_library":"personal","colour":"red"}`:         `unknown field "colour"`,
		`{"default_library":"nope"}`:                            `no library called "nope"`,
		`{"default_library":"personal","sync_interval":"soon"}`: "sync_interval",
		`{"default_library":"personal"`:                         "unexpected EOF",
	} {
		os.Setenv("NEXT", next)
		if code := ta.run(nil, "config", "edit"); code == 0 || !strings.Contains(ta.err.String(), want) {
			t.Errorf("%s: exit %d, err %q, want %q", next, code, ta.err, want)
		}
		// Declining another go leaves it as it was.
		if code := ta.run(ans("n\n"), "config", "edit"); code == 0 {
			t.Errorf("%s: declined edit succeeded", next)
		}
	}
	b, _ := os.ReadFile(filepath.Join(ta.Home, "config.json"))
	if !strings.Contains(string(b), `"notify":"bell"`) {
		t.Errorf("config.json changed: %s", b)
	}

	if code := ta.run(nil, "config", "path"); code != 0 || strings.TrimSpace(ta.out.String()) != filepath.Join(ta.Home, "config.json") {
		t.Errorf("path = %q", ta.out)
	}
	if code := ta.run(nil, "config"); code != 0 || !strings.Contains(ta.out.String(), "jobs.notify = bell") ||
		!strings.Contains(ta.out.String(), "jobs.keep_max = 50 (default)") {
		t.Errorf("list = %q", ta.out)
	}
	if code := ta.run(nil, "config", "x"); code == 0 {
		t.Error("unknown subcommand accepted")
	}
}
