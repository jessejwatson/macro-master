package cli

import (
	"fmt"
	"os/exec"
	"strings"

	"macro-master/internal/store"
)

func lookPathReal(name string) (string, error) { return exec.LookPath(name) }

// resolve turns a "name" or "library/name" address into one macro. When the
// name is ambiguous or unknown and there's a terminal, it falls back to the
// picker.
func (a *App) resolve(addr string) (store.Ref, error) {
	matches := a.store.Find(addr)
	switch {
	case len(matches) == 1:
		return matches[0], nil
	case len(matches) > 1:
		if a.Interactive {
			ref, _, err := a.pick(matches, false)
			return ref, err
		}
		ids := make([]string, len(matches))
		for i, m := range matches {
			ids[i] = m.ID()
		}
		return store.Ref{}, fmt.Errorf("%q matches more than one macro; use one of: %s", addr, strings.Join(ids, ", "))
	}

	msg := fmt.Sprintf("there is no macro called %q", addr)
	if sugg := a.store.Suggest(addr); len(sugg) > 0 {
		msg += "; did you mean " + strings.Join(sugg, ", ") + "?"
	} else {
		msg += "; run mm ls to see your macros"
	}
	if !a.Interactive || len(a.store.AllMacros()) == 0 {
		return store.Ref{}, fmt.Errorf("%s", msg)
	}
	fmt.Fprintf(a.Stderr, "mm: %s\n", msg)
	ok, err := a.confirm("Open the picker?", true)
	if err != nil {
		return store.Ref{}, err
	}
	if !ok {
		return store.Ref{}, errCancelled
	}
	ref, _, err := a.pick(a.store.AllMacros(), false)
	return ref, err
}

// oneArg checks a command got exactly one positional argument.
func oneArg(cmd string, args []string) (string, error) {
	if len(args) != 1 {
		return "", fmt.Errorf("usage: mm %s <name>", cmd)
	}
	return args[0], nil
}
