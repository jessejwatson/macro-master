// Package hotkey parses key combinations such as "cmd+shift+d" and, on
// macOS, registers them as global hotkeys that fire whichever app is in
// front.
package hotkey

import (
	"errors"
	"fmt"
	"strings"
)

// ErrUnsupported is returned where global hotkeys aren't available.
var ErrUnsupported = errors.New("global key bindings only work on macOS, in an mm built with cgo")

// Mod is a set of modifier keys.
type Mod uint8

const (
	Ctrl Mod = 1 << iota
	Alt
	Shift
	Cmd
)

// modOrder is the order modifiers are written in, as macOS menus show
// them: ⌃⌥⇧⌘.
var modOrder = []struct {
	mod    Mod
	name   string
	symbol string
}{
	{Ctrl, "ctrl", "⌃"},
	{Alt, "alt", "⌥"},
	{Shift, "shift", "⇧"},
	{Cmd, "cmd", "⌘"},
}

var modNames = map[string]Mod{
	"ctrl": Ctrl, "control": Ctrl, "ctl": Ctrl, "⌃": Ctrl,
	"alt": Alt, "opt": Alt, "option": Alt, "⌥": Alt,
	"shift": Shift, "⇧": Shift,
	"cmd": Cmd, "command": Cmd, "super": Cmd, "⌘": Cmd,
}

// Combo is a key combination: modifiers and one key, by its canonical
// name (see Keys).
type Combo struct {
	Mods Mod
	Key  string
}

// key is one key that can be bound: its name, any other names it's
// written as, how it's shown and its macOS virtual key code (the
// position on an ANSI keyboard).
type key struct {
	name    string
	aliases []string
	symbol  string
	code    uint32
}

var keys = []key{
	{"a", nil, "A", 0x00}, {"b", nil, "B", 0x0B}, {"c", nil, "C", 0x08},
	{"d", nil, "D", 0x02}, {"e", nil, "E", 0x0E}, {"f", nil, "F", 0x03},
	{"g", nil, "G", 0x05}, {"h", nil, "H", 0x04}, {"i", nil, "I", 0x22},
	{"j", nil, "J", 0x26}, {"k", nil, "K", 0x28}, {"l", nil, "L", 0x25},
	{"m", nil, "M", 0x2E}, {"n", nil, "N", 0x2D}, {"o", nil, "O", 0x1F},
	{"p", nil, "P", 0x23}, {"q", nil, "Q", 0x0C}, {"r", nil, "R", 0x0F},
	{"s", nil, "S", 0x01}, {"t", nil, "T", 0x11}, {"u", nil, "U", 0x20},
	{"v", nil, "V", 0x09}, {"w", nil, "W", 0x0D}, {"x", nil, "X", 0x07},
	{"y", nil, "Y", 0x10}, {"z", nil, "Z", 0x06},
	{"0", nil, "0", 0x1D}, {"1", nil, "1", 0x12}, {"2", nil, "2", 0x13},
	{"3", nil, "3", 0x14}, {"4", nil, "4", 0x15}, {"5", nil, "5", 0x17},
	{"6", nil, "6", 0x16}, {"7", nil, "7", 0x1A}, {"8", nil, "8", 0x1C},
	{"9", nil, "9", 0x19},
	{"-", []string{"minus"}, "-", 0x1B}, {"=", []string{"equal", "equals"}, "=", 0x18},
	{"[", []string{"lbracket"}, "[", 0x21}, {"]", []string{"rbracket"}, "]", 0x1E},
	{";", []string{"semicolon"}, ";", 0x29}, {"'", []string{"quote"}, "'", 0x27},
	{",", []string{"comma"}, ",", 0x2B}, {".", []string{"period"}, ".", 0x2F},
	{"/", []string{"slash"}, "/", 0x2C}, {`\`, []string{"backslash"}, `\`, 0x2A},
	{"`", []string{"grave", "backtick"}, "`", 0x32},
	{"space", nil, "Space", 0x31}, {"return", []string{"enter"}, "↩", 0x24},
	{"tab", nil, "⇥", 0x30}, {"esc", []string{"escape"}, "⎋", 0x35},
	{"delete", []string{"backspace"}, "⌫", 0x33},
	{"forwarddelete", []string{"fwddelete"}, "⌦", 0x75},
	{"home", nil, "↖", 0x73}, {"end", nil, "↘", 0x77},
	{"pageup", []string{"pgup"}, "⇞", 0x74}, {"pagedown", []string{"pgdn"}, "⇟", 0x79},
	{"left", nil, "←", 0x7B}, {"right", nil, "→", 0x7C},
	{"up", nil, "↑", 0x7E}, {"down", nil, "↓", 0x7D},
	{"f1", nil, "F1", 0x7A}, {"f2", nil, "F2", 0x78}, {"f3", nil, "F3", 0x63},
	{"f4", nil, "F4", 0x76}, {"f5", nil, "F5", 0x60}, {"f6", nil, "F6", 0x61},
	{"f7", nil, "F7", 0x62}, {"f8", nil, "F8", 0x64}, {"f9", nil, "F9", 0x65},
	{"f10", nil, "F10", 0x6D}, {"f11", nil, "F11", 0x67}, {"f12", nil, "F12", 0x6F},
	{"f13", nil, "F13", 0x69}, {"f14", nil, "F14", 0x6B}, {"f15", nil, "F15", 0x71},
	{"f16", nil, "F16", 0x6A}, {"f17", nil, "F17", 0x40}, {"f18", nil, "F18", 0x4F},
	{"f19", nil, "F19", 0x50}, {"f20", nil, "F20", 0x5A},
}

var keyByName = map[string]*key{}

func init() {
	for i := range keys {
		k := &keys[i]
		keyByName[k.name] = k
		for _, a := range k.aliases {
			keyByName[a] = k
		}
	}
}

// Parse reads a combination such as "cmd+shift+d" or "ctrl+alt+f5":
// modifiers and a key joined by +, in any order and case. A key alone is
// allowed only for F-keys, and shift needs ctrl, alt or cmd too, so a
// binding can't swallow ordinary typing.
func Parse(s string) (Combo, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return Combo{}, errors.New("no keys given; write them like cmd+shift+d")
	}
	parts := strings.Split(s, "+")
	// A trailing "+" means the key is "+" itself, which is shift+= on most
	// keyboards.
	if parts[len(parts)-1] == "" {
		return Combo{}, fmt.Errorf("%q: + can't be bound, as it joins the keys; use shift+= instead", s)
	}
	var c Combo
	for _, p := range parts[:len(parts)-1] {
		p = strings.TrimSpace(p)
		m, ok := modNames[p]
		if !ok {
			if _, isKey := keyByName[p]; isKey {
				return Combo{}, fmt.Errorf("%q has more than one key; a combination is modifiers and one key, like cmd+shift+d", s)
			}
			return Combo{}, fmt.Errorf("%q: %q isn't a modifier; use ctrl, alt (or opt), shift or cmd", s, p)
		}
		if c.Mods&m != 0 {
			return Combo{}, fmt.Errorf("%q has %s twice", s, p)
		}
		c.Mods |= m
	}
	last := strings.TrimSpace(parts[len(parts)-1])
	k, ok := keyByName[last]
	if !ok {
		if _, isMod := modNames[last]; isMod {
			return Combo{}, fmt.Errorf("%q has no key, only modifiers; end it with a key, like cmd+shift+d", s)
		}
		return Combo{}, fmt.Errorf("%q: %q isn't a key mm knows; use a letter, digit, punctuation, f1-f20, space, return, tab, esc, delete, an arrow (up, down, left, right), home, end, pageup or pagedown", s, last)
	}
	c.Key = k.name
	if c.Mods&(Ctrl|Alt|Cmd) == 0 && !c.isFKey() {
		return Combo{}, fmt.Errorf("%q would take over ordinary typing; add ctrl, alt or cmd, like cmd+shift+%s", s, k.name)
	}
	return c, nil
}

func (c Combo) isFKey() bool {
	return len(c.Key) >= 2 && c.Key[0] == 'f' && c.Key[1] >= '0' && c.Key[1] <= '9'
}

// String is the canonical form, modifiers in ⌃⌥⇧⌘ order: "shift+cmd+d".
func (c Combo) String() string {
	var parts []string
	for _, m := range modOrder {
		if c.Mods&m.mod != 0 {
			parts = append(parts, m.name)
		}
	}
	return strings.Join(append(parts, c.Key), "+")
}

// Symbols is the combination as macOS menus show it: "⇧⌘D".
func (c Combo) Symbols() string {
	var b strings.Builder
	for _, m := range modOrder {
		if c.Mods&m.mod != 0 {
			b.WriteString(m.symbol)
		}
	}
	if k := keyByName[c.Key]; k != nil {
		b.WriteString(k.symbol)
	}
	return b.String()
}

// systemShortcuts are well-known macOS shortcuts. macOS doesn't say when a
// hotkey clashes with one, and a binding would take over, say, copying in
// every app, or never fire because macOS gets it first.
var systemShortcuts = map[string]string{
	"cmd+a": "Select All", "cmd+c": "Copy", "cmd+v": "Paste", "cmd+x": "Cut",
	"cmd+z": "Undo", "shift+cmd+z": "Redo", "cmd+s": "Save", "cmd+q": "Quit",
	"cmd+w": "Close Window", "cmd+t": "New Tab", "cmd+n": "New Window",
	"cmd+o": "Open", "cmd+p": "Print", "cmd+f": "Find", "cmd+h": "Hide",
	"cmd+m": "Minimise", "cmd+,": "Settings", "cmd+tab": "the app switcher",
	"cmd+`": "the window switcher", "cmd+space": "Spotlight",
	"ctrl+space": "the input source switcher", "alt+cmd+esc": "Force Quit",
	"ctrl+cmd+q": "Lock Screen", "ctrl+cmd+space": "the emoji picker",
	"ctrl+cmd+f": "Full Screen", "shift+cmd+3": "a screenshot",
	"shift+cmd+4": "a screenshot", "shift+cmd+5": "the screenshot tools",
}

// SystemShortcut is what c does in macOS already, or "" when it isn't a
// well-known shortcut.
func (c Combo) SystemShortcut() string { return systemShortcuts[c.String()] }

// Code is the key's macOS virtual key code.
func (c Combo) Code() uint32 {
	if k := keyByName[c.Key]; k != nil {
		return k.code
	}
	return 0xFFFF
}

// Keys lists the canonical key names, for completion.
func Keys() []string {
	out := make([]string, len(keys))
	for i, k := range keys {
		out[i] = k.name
	}
	return out
}

// Modifiers lists the canonical modifier names.
func Modifiers() []string {
	var out []string
	for _, m := range modOrder {
		out = append(out, m.name)
	}
	return out
}
