package store

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// Alias is a command name of its own for a macro, defined in the shell by
// the mm init hook. Args go before any typed after it, so a preset such as
// env=prod can still be overridden.
type Alias struct {
	Macro string   `json:"macro"` // full id, "library/folders/name"
	Args  []string `json:"args,omitempty"`
	// Force lets the alias shadow a command of the same name on PATH.
	Force bool `json:"force,omitempty"`
}

var aliasRe = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)

// ValidateAliasName reports why name can't be an alias name.
func ValidateAliasName(name string) error {
	if !aliasRe.MatchString(name) {
		return fmt.Errorf("%q is not a valid alias name; start with a letter, then use lowercase letters, digits, - and _", name)
	}
	return nil
}

// AliasesFor lists the names of the aliases for the macro id, sorted.
func (c Config) AliasesFor(id string) []string {
	var out []string
	for name, al := range c.Aliases {
		if al.Macro == id {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

// Targets reports whether the alias runs the macro id or, with prefix,
// a macro under the library or folder id ("lib" or "lib/folder").
func (al Alias) Targets(id string, prefix bool) bool {
	if prefix {
		return strings.HasPrefix(al.Macro, id+"/")
	}
	return al.Macro == id
}

// RetargetAliases points the aliases that target from (see Targets) at
// to instead.
func (c *Config) RetargetAliases(from, to string, prefix bool) {
	for name, al := range c.Aliases {
		if al.Targets(from, prefix) {
			al.Macro = to + strings.TrimPrefix(al.Macro, from)
			c.Aliases[name] = al
		}
	}
}

func checkAliases(aliases map[string]Alias) error {
	for name, al := range aliases {
		if err := ValidateAliasName(name); err != nil {
			return fmt.Errorf("aliases.%s: %w", name, err)
		}
		if al.Macro == "" {
			return fmt.Errorf("aliases.%s: macro is empty", name)
		}
	}
	return nil
}

func (s *Store) AliasesPath() string { return filepath.Join(s.Home, "aliases") }

// AliasFile renders the file the shell hook reads: a generation line that
// changes whenever the rest does, then one alias a line, as "name" or
// "name force". Targets aren't listed; mm __alias looks them up when one
// runs, so retargeting needs no shell reload.
func AliasFile(aliases map[string]Alias) string {
	var b strings.Builder
	for _, name := range slices.Sorted(maps.Keys(aliases)) {
		if ValidateAliasName(name) != nil {
			continue
		}
		b.WriteString(name)
		if aliases[name].Force {
			b.WriteString(" force")
		}
		b.WriteString("\n")
	}
	sum := sha1.Sum([]byte(b.String()))
	return hex.EncodeToString(sum[:6]) + "\n" + b.String()
}

// SyncAliasFile brings the alias file up to date with config.json as it is
// on disk now, which may be newer than s.Config if another mm saved
// meanwhile. It writes only when something changed, and leaves no file
// while there are no aliases.
func (s *Store) SyncAliasFile() error {
	c := s.Config
	if data, err := os.ReadFile(s.configPath()); err == nil {
		var fresh Config
		if json.Unmarshal(data, &fresh) == nil {
			c = fresh
		}
	}
	path := s.AliasesPath()
	old, err := os.ReadFile(path)
	if len(c.Aliases) == 0 {
		if err == nil {
			return os.Remove(path)
		}
		return nil
	}
	want := AliasFile(c.Aliases)
	if err == nil && string(old) == want {
		return nil
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return WriteFileAtomic(path, []byte(want), 0o644)
}
