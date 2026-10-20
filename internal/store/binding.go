package store

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"macro-master/internal/hotkey"
)

// Binding is a global key combination that runs a macro as a detached
// job, from whichever app is in front. Config.Bindings keys them by the
// combination's canonical form, e.g. "shift+cmd+d".
type Binding struct {
	Macro string   `json:"macro"` // full id, "library/folders/name"
	Args  []string `json:"args,omitempty"`
	// Quiet turns off notifications for this binding: when its job ends,
	// and when it can't start one.
	Quiet bool `json:"quiet,omitempty"`
}

// BindingsFor lists the combinations bound to the macro id, sorted.
func (c Config) BindingsFor(id string) []string {
	var out []string
	for keys, b := range c.Bindings {
		if b.Macro == id {
			out = append(out, keys)
		}
	}
	slices.Sort(out)
	return out
}

// RetargetBindings points the bindings that run from (see Alias.Targets)
// at to instead.
func (c *Config) RetargetBindings(from, to string, prefix bool) {
	for keys, b := range c.Bindings {
		if (Alias{Macro: b.Macro}).Targets(from, prefix) {
			b.Macro = to + strings.TrimPrefix(b.Macro, from)
			c.Bindings[keys] = b
		}
	}
}

func checkBindings(bindings map[string]Binding) error {
	for keys, b := range bindings {
		c, err := hotkey.Parse(keys)
		if err != nil {
			return fmt.Errorf("bindings: %w", err)
		}
		if c.String() != keys {
			return fmt.Errorf("bindings.%s: write it as %s", keys, c.String())
		}
		if b.Macro == "" {
			return fmt.Errorf("bindings.%s: macro is empty", keys)
		}
	}
	return nil
}

// HotkeyStatusPath is where the hotkey daemon says what it registered.
func (s *Store) HotkeyStatusPath() string { return filepath.Join(s.Home, "hotkeys.json") }

// HotkeyLogPath is the hotkey daemon's output.
func (s *Store) HotkeyLogPath() string { return filepath.Join(s.Home, "hotkeys.log") }
