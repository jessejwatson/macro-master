package macro

import (
	"regexp"
	"strings"
)

// placeholderRe matches {{name}} and {{name:default}}.
var placeholderRe = regexp.MustCompile(`\{\{([A-Za-z_][A-Za-z0-9_-]*)(?::([^}]*))?\}\}`)

// Placeholder is one distinct {{name}} in a macro body.
type Placeholder struct {
	Name       string
	Default    string
	HasDefault bool
}

// Placeholders returns the distinct placeholders in body, in order of first
// appearance. The first default given for a name wins.
func Placeholders(body string) []Placeholder {
	var out []Placeholder
	seen := map[string]int{}
	for _, m := range placeholderRe.FindAllStringSubmatchIndex(body, -1) {
		name := body[m[2]:m[3]]
		hasDef := m[4] >= 0
		def := ""
		if hasDef {
			def = body[m[4]:m[5]]
		}
		if i, ok := seen[name]; ok {
			if !out[i].HasDefault && hasDef {
				out[i].Default, out[i].HasDefault = def, true
			}
			continue
		}
		seen[name] = len(out)
		out = append(out, Placeholder{Name: name, Default: def, HasDefault: hasDef})
	}
	return out
}

// Fill replaces every placeholder in body with its value, inserted as typed.
// Placeholders missing from values are left as they are.
func Fill(body string, values map[string]string) string {
	return placeholderRe.ReplaceAllStringFunc(body, func(s string) string {
		name := placeholderRe.FindStringSubmatch(s)[1]
		if v, ok := values[name]; ok {
			return v
		}
		return s
	})
}

// SplitArgs separates name=value arguments that match one of the
// placeholders from arguments to pass through to the macro.
func SplitArgs(args []string, ph []Placeholder) (values map[string]string, rest []string) {
	known := map[string]bool{}
	for _, p := range ph {
		known[p.Name] = true
	}
	values = map[string]string{}
	for _, a := range args {
		if k, v, ok := strings.Cut(a, "="); ok && known[k] {
			values[k] = v
			continue
		}
		rest = append(rest, a)
	}
	return values, rest
}
