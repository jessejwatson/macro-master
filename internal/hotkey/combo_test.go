package hotkey

import (
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	for in, want := range map[string]struct{ canon, symbols string }{
		"cmd+shift+d":               {"shift+cmd+d", "⇧⌘D"},
		" Shift + CMD + D ":         {"shift+cmd+d", "⇧⌘D"},
		"opt+command+control+space": {"ctrl+alt+cmd+space", "⌃⌥⌘Space"},
		"ctrl+alt+f5":               {"ctrl+alt+f5", "⌃⌥F5"},
		"f13":                       {"f13", "F13"},
		"shift+f2":                  {"shift+f2", "⇧F2"},
		"cmd+enter":                 {"cmd+return", "⌘↩"},
		"ctrl+escape":               {"ctrl+esc", "⌃⎋"},
		"cmd+shift+=":               {"shift+cmd+=", "⇧⌘="},
		"cmd+/":                     {"cmd+/", "⌘/"},
		"⌃⌥+up":                     {"", ""}, // symbols can't be run together
		"super+backtick":            {"cmd+`", "⌘`"},
	} {
		c, err := Parse(in)
		if want.canon == "" {
			if err == nil {
				t.Errorf("%q parsed as %s", in, c)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: %v", in, err)
			continue
		}
		if c.String() != want.canon || c.Symbols() != want.symbols {
			t.Errorf("%q = %s %s, want %s %s", in, c, c.Symbols(), want.canon, want.symbols)
		}
		// The canonical form parses back to itself.
		if again, err := Parse(c.String()); err != nil || again != c {
			t.Errorf("%s doesn't round-trip: %v %v", c, again, err)
		}
	}
}

func TestParseErrors(t *testing.T) {
	for in, want := range map[string]string{
		"":           "no keys",
		"d":          "ordinary typing",
		"shift+d":    "ordinary typing",
		"cmd+shift":  "only modifiers",
		"cmd+d+e":    "more than one key",
		"cmd+cmd+d":  "cmd twice",
		"hyper+d":    "isn't a modifier",
		"cmd+banana": "isn't a key",
		"cmd++":      "+ can't be bound",
	} {
		_, err := Parse(in)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: error %v, want %q", in, err, want)
		}
	}
}

func TestKeyCodesAreDistinct(t *testing.T) {
	seen := map[uint32]string{}
	for _, k := range keys {
		if other, ok := seen[k.code]; ok {
			t.Errorf("%s and %s share key code %#x", k.name, other, k.code)
		}
		seen[k.code] = k.name
	}
}
