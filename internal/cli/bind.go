package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"syscall"
	"text/tabwriter"

	"charm.land/lipgloss/v2"
	"charm.land/lipgloss/v2/table"

	"macro-master/internal/hotkey"
	"macro-master/internal/jobs"
	"macro-master/internal/macro"
	"macro-master/internal/store"
)

func (a *App) cmdBind(args []string) error {
	if len(args) == 0 {
		return a.bindLs()
	}
	sub, args := args[0], args[1:]
	switch sub {
	case "ls":
		if len(args) > 0 {
			return errors.New("usage: mm bind ls")
		}
		return a.bindLs()
	case "add":
		quiet, args := cutFlag(args, "--quiet")
		if len(args) < 2 {
			return errors.New("usage: mm bind add <keys> <macro> [k=v...] [--quiet], e.g. mm bind add cmd+shift+d web/deploy env=prod")
		}
		return a.bindAdd(args[0], args[1], args[2:], quiet)
	case "rm":
		if len(args) != 1 {
			return errors.New("usage: mm bind rm <keys>")
		}
		return a.bindRm(args[0])
	case "service":
		return a.cmdBindService(args)
	}
	return fmt.Errorf("unknown bind command %q; use ls, add, rm or service", sub)
}

// quietLabel notes a binding that doesn't notify.
func quietLabel(b store.Binding) string {
	if b.Quiet {
		return ", without notifications"
	}
	return ""
}

// describeBinding is what a binding runs: the macro and its preset args.
func describeBinding(b store.Binding) string {
	return strings.Join(append([]string{b.Macro}, b.Args...), " ")
}

// bindAdd binds keys to the macro at addr, or to what the alias addr runs,
// with preset args; quiet turns off its notifications. A hotkey can't ask questions, so the macro must be
// runnable as it stands: every placeholder filled, and trusted if shared.
func (a *App) bindAdd(keys, addr string, args []string, quiet bool) error {
	if !hotkey.Supported {
		return hotkey.ErrUnsupported
	}
	c, err := hotkey.Parse(keys)
	if err != nil {
		return err
	}
	if what := c.SystemShortcut(); what != "" {
		msg := fmt.Sprintf("%s is %s in macOS; binding it takes it over in every app, or never fires.", c.Symbols(), what)
		if a.Prompts == nil {
			return fmt.Errorf("%s Pick other keys", msg)
		}
		ok, err := a.confirm(msg+" Bind it anyway?", false)
		if err != nil {
			return err
		}
		if !ok {
			a.nothing("Nothing bound.")
			return errCancelled
		}
	}
	ref, args, err := a.resolveRun(addr, args)
	if err != nil {
		return err
	}
	content, err := ref.Read()
	if err != nil {
		return err
	}
	m := macro.Parse(content)
	if m.Mode == "source" {
		return fmt.Errorf("%s can't have a key binding: it's a \"# mode: source\" macro, which changes your current shell, and a hotkey runs it in the background", ref.ID())
	}
	ph := macro.Placeholders(m.Body)
	values, _ := macro.SplitArgs(args, ph)
	for _, p := range ph {
		if _, ok := values[p.Name]; !ok && !p.HasDefault {
			return fmt.Errorf("{{%s}} needs a value, and a hotkey can't ask for one; add %s=<value> after the macro name", p.Name, p.Name)
		}
	}
	if err := a.checkTrust(ref, content); err != nil {
		return err
	}

	b := store.Binding{Macro: ref.ID(), Args: args, Quiet: quiet}
	u := a.errUI()
	if old, ok := a.store.Config.Bindings[c.String()]; ok {
		same := old.Macro == b.Macro && slices.Equal(old.Args, b.Args)
		switch {
		case same && old.Quiet == b.Quiet:
			a.nothing(fmt.Sprintf("%s already runs %s%s.", c.Symbols(), describeBinding(b), quietLabel(b)))
			return nil
		case same:
			// Only notifications change: nothing to confirm.
		case a.Prompts == nil:
			return fmt.Errorf("%s already runs %s; remove it first with mm bind rm %s", c.Symbols(), describeBinding(old), c)
		default:
			ok, err := a.confirm(fmt.Sprintf("%s already runs %s. Run %s instead?", c.Symbols(), describeBinding(old), describeBinding(b)), false)
			if err != nil {
				return err
			}
			if !ok {
				a.nothing("Binding unchanged.")
				return errCancelled
			}
		}
	}
	if err := a.store.UpdateConfig(func(cfg *store.Config) {
		if cfg.Bindings == nil {
			cfg.Bindings = map[string]store.Binding{}
		}
		cfg.Bindings[c.String()] = b
	}); err != nil {
		return err
	}
	a.done("%s now runs %s%s.", u.fg(colCmd, c.Symbols()), u.bold(describeBinding(b)), quietLabel(b))
	if !a.hotkeyStatus().running() {
		a.notef("The hotkey service isn't running, so the binding does nothing yet. Start it with: mm bind service install")
	}
	return nil
}

func (a *App) bindRm(keys string) error {
	c, err := hotkey.Parse(keys)
	if err != nil {
		return err
	}
	b, ok := a.store.Config.Bindings[c.String()]
	if !ok {
		return fmt.Errorf("nothing is bound to %s; run mm bind ls to see bindings", c.Symbols())
	}
	if err := a.store.UpdateConfig(func(cfg *store.Config) { delete(cfg.Bindings, c.String()) }); err != nil {
		return err
	}
	a.done("Removed %s (it ran %s).", a.errUI().bold(c.Symbols()), describeBinding(b))
	return nil
}

// bindingRef finds the macro a binding runs.
func (a *App) bindingRef(c hotkey.Combo, b store.Binding) (store.Ref, error) {
	for _, r := range a.store.AllMacros() {
		if r.ID() == b.Macro {
			return r, nil
		}
	}
	return store.Ref{}, fmt.Errorf("%s runs %s, which doesn't exist any more; rebind it with mm bind add %s <macro>, or remove it with mm bind rm %s", c.Symbols(), b.Macro, c, c)
}

// comboOf parses a binding's key, which config.json checks is canonical.
func comboOf(keys string) hotkey.Combo {
	c, _ := hotkey.Parse(keys)
	return c
}

// bindLs lists bindings with what they run and whether the hotkey service
// has them.
func (a *App) bindLs() error {
	u := a.outUI()
	bindings := a.store.Config.Bindings
	st := a.hotkeyStatus()
	running := st.running()
	var rows [][]string
	for _, keys := range slices.Sorted(maps.Keys(bindings)) {
		b := bindings[keys]
		c := comboOf(keys)
		var status string
		if _, err := a.bindingRef(c, b); err != nil {
			status = u.fg(colBad, "macro missing")
		} else if running {
			switch e, ok := st.Keys[keys]; {
			case !ok:
				status = u.faint("loading")
			case e != "":
				status = u.fg(colBad, e)
			default:
				status = u.fg(colOK, "active")
			}
		} else {
			status = u.fg(colWarn, "service not running")
		}
		if b.Quiet {
			status += u.faint(", quiet")
		}
		label := c.Symbols()
		if u.on {
			rows = append(rows, []string{u.fg(colCmd, label), u.faint(keys), u.bold(b.Macro), u.faint(strings.Join(b.Args, " ")), status})
		} else {
			rows = append(rows, []string{label, keys, b.Macro, strings.Join(b.Args, " "), status})
		}
	}
	if len(rows) == 0 {
		fmt.Fprintln(a.Stderr, "No key bindings yet. Add one with: mm bind add <keys> <macro>, e.g. mm bind add cmd+shift+d web/deploy")
		return nil
	}
	if u.on {
		t := table.New().
			Border(lipgloss.RoundedBorder()).
			BorderStyle(lipgloss.NewStyle().Foreground(colBorder)).
			Headers("Keys", "", "Macro", "Args", "Status").
			Rows(rows...).
			StyleFunc(func(row, col int) lipgloss.Style {
				s := lipgloss.NewStyle().Padding(0, 1)
				if row == table.HeaderRow {
					s = s.Foreground(colAccent).Bold(true)
				}
				return s
			})
		fmt.Fprintln(a.Stdout, t.String())
	} else {
		w := tabwriter.NewWriter(a.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "KEYS\tCOMBO\tMACRO\tARGS\tSTATUS")
		for _, r := range rows {
			fmt.Fprintln(w, strings.Join(r, "\t"))
		}
		w.Flush()
	}
	if !running {
		fmt.Fprintln(a.Stderr, a.errUI().faint("Bindings work while the hotkey service runs; start it with: mm bind service install"))
	}
	return nil
}

// describeBindings is a macro's key bindings with their preset args, for
// mm show: "⇧⌘D, ⌃⌥P env=prod".
func (a *App) describeBindings(ref store.Ref) string {
	var out []string
	for _, keys := range a.store.Config.BindingsFor(ref.ID()) {
		b := a.store.Config.Bindings[keys]
		s := strings.Join(append([]string{comboOf(keys).Symbols()}, b.Args...), " ")
		if b.Quiet {
			s += " (quiet)"
		}
		out = append(out, s)
	}
	return strings.Join(out, ", ")
}

// describeBindingKeys shows keys as symbols: "⇧⌘D, ⌃⌥P".
func (a *App) describeBindingKeys(keys []string) string {
	out := make([]string, len(keys))
	for i, k := range keys {
		out[i] = comboOf(k).Symbols()
	}
	return strings.Join(out, ", ")
}

// cmdBindFire is mm __bind <keys>, what the hotkey daemon runs when keys
// are pressed: it starts the bound macro as a detached job. Nobody is at
// a terminal to read an error, so one shows as a notification too.
func (a *App) cmdBindFire(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: mm __bind <keys>")
	}
	keys := args[0]
	// Nothing can be asked: there's no terminal, only a keypress.
	a.Prompts, a.promptTTY, a.Interactive = nil, nil, false
	c := comboOf(keys)
	b, ok := a.store.Config.Bindings[keys]
	if !ok {
		return fmt.Errorf("nothing is bound to %s any more", keys)
	}
	err := a.fireBinding(c, b)
	if err != nil && !b.Quiet {
		a.notify(fmt.Sprintf("✗ %s didn't run", c.Symbols()), b.Macro, err.Error(), false)
	}
	return err
}

func (a *App) fireBinding(c hotkey.Combo, b store.Binding) error {
	ref, err := a.bindingRef(c, b)
	if err != nil {
		return err
	}
	a.jobNotifyOff = b.Quiet
	return a.runDetached(ref, slices.Clone(b.Args), "")
}

// notify shows a desktop notification that isn't about a job ending.
func (a *App) notify(title, subtitle, body string, ok bool) {
	if a.Notify != nil {
		a.Notify(title, subtitle, body, ok)
		return
	}
	sound := a.store.Config.Jobs.SoundMode()
	jobs.NotifyMessage(title, subtitle, body, ok, sound == "both" || (ok && sound == "success") || (!ok && sound == "failure"))
}

// hotkeyStatus is what the hotkey daemon last wrote to hotkeys.json.
type hotkeyStatus struct {
	PID int    `json:"pid"`
	Exe string `json:"exe"`
	// Keys maps each binding's keys to "" when registered, or why not.
	Keys map[string]string `json:"keys"`
}

func (a *App) hotkeyStatus() hotkeyStatus {
	var st hotkeyStatus
	if data, err := os.ReadFile(a.store.HotkeyStatusPath()); err == nil {
		json.Unmarshal(data, &st)
	}
	return st
}

// running reports whether the daemon that wrote st is still alive.
func (st hotkeyStatus) running() bool {
	if st.PID <= 0 {
		return false
	}
	err := syscall.Kill(st.PID, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
