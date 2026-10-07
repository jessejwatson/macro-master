// Package macro parses and composes macro files.
//
// A macro file is a normal script with optional header comments:
//
//	#!/usr/bin/env bash
//	# description: Flush DNS cache on macOS
//	# mode: source
//	sudo dscacheutil -flushcache
package macro

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
)

// DefaultShebang is written into new macros that don't bring their own.
const DefaultShebang = "#!/usr/bin/env bash"

// Ext is the file extension of macro files.
const Ext = ".sh"

var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// Reserved holds subcommand names that can't be used as macro names.
var Reserved = map[string]bool{
	"add": true, "edit": true, "show": true, "print": true, "rm": true,
	"mv": true, "ls": true, "fav": true, "lib": true, "sync": true,
	"auth": true, "trust": true, "init": true, "completion": true,
	"run": true, "help": true, "version": true, "__complete": true,
}

// ValidateName reports why name can't be used as a macro or library name.
func ValidateName(name string) error {
	if !nameRe.MatchString(name) {
		return fmt.Errorf("%q is not a valid name; use lowercase letters, digits, - and _", name)
	}
	return nil
}

// ValidateMacroName is ValidateName plus the reserved-word check.
func ValidateMacroName(name string) error {
	if err := ValidateName(name); err != nil {
		return err
	}
	if Reserved[name] {
		return fmt.Errorf("%q is a reserved word in mm; pick another name", name)
	}
	return nil
}

// Macro is a parsed macro file.
type Macro struct {
	Shebang     string // full first line including "#!", or ""
	Description string
	Mode        string // "" or "source"
	Body        string // everything below the header
}

// Parse splits a macro file into header fields and body.
// Header lines are the shebang plus leading "# description:" and "# mode:"
// comments; any other comment belongs to the body.
func Parse(content string) Macro {
	var m Macro
	lines := strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n")
	i := 0
	if len(lines) > 0 && strings.HasPrefix(lines[0], "#!") {
		m.Shebang = strings.TrimSpace(lines[0])
		i = 1
	}
	for ; i < len(lines); i++ {
		key, val, ok := headerField(lines[i])
		if !ok {
			break
		}
		switch key {
		case "description":
			m.Description = val
		case "mode":
			m.Mode = strings.ToLower(val)
		}
	}
	m.Body = strings.Join(lines[i:], "\n")
	return m
}

func headerField(line string) (key, val string, ok bool) {
	rest, found := strings.CutPrefix(strings.TrimSpace(line), "#")
	if !found {
		return "", "", false
	}
	k, v, found := strings.Cut(rest, ":")
	if !found {
		return "", "", false
	}
	k = strings.ToLower(strings.TrimSpace(k))
	if k != "description" && k != "mode" {
		return "", "", false
	}
	return k, strings.TrimSpace(v), true
}

// String renders the macro back into file content, ending in a newline.
func (m Macro) String() string {
	var b strings.Builder
	if m.Shebang != "" {
		b.WriteString(m.Shebang + "\n")
	}
	if m.Description != "" {
		b.WriteString("# description: " + m.Description + "\n")
	}
	if m.Mode != "" {
		b.WriteString("# mode: " + m.Mode + "\n")
	}
	b.WriteString(strings.TrimRight(m.Body, "\n") + "\n")
	return b.String()
}

// Compose builds a new macro file from pasted text and an optional
// description. Pasted text that already has a header keeps it; a missing
// shebang gets DefaultShebang so the file stays runnable.
func Compose(text, description string) string {
	m := Parse(text)
	if m.Shebang == "" {
		m.Shebang = DefaultShebang
	}
	if description != "" {
		m.Description = description
	}
	return m.String()
}

// Interpreter returns the argv prefix used to run the macro: the shebang's
// interpreter and its optional single argument, or bash.
func (m Macro) Interpreter() []string {
	line := strings.TrimSpace(strings.TrimPrefix(m.Shebang, "#!"))
	if line == "" {
		return []string{"bash"}
	}
	// Like the kernel: the interpreter, then everything else as one argument.
	interp, arg, found := strings.Cut(line, " ")
	arg = strings.TrimSpace(arg)
	if !found || arg == "" {
		return []string{interp}
	}
	return []string{interp, arg}
}

// BlobHash is the git blob hash of content, so trust records match what
// git would store.
func BlobHash(content string) string {
	h := sha1.New()
	fmt.Fprintf(h, "blob %d\x00", len(content))
	h.Write([]byte(content))
	return hex.EncodeToString(h.Sum(nil))
}
