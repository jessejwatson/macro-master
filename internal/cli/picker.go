package cli

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"

	"macro-master/internal/macro"
	"macro-master/internal/store"
)

// Picker actions.
const (
	actRun  = "run"
	actEdit = "edit"
)

// errPickCancelled is returned when the user leaves the picker.
var errPickCancelled = exitCode(130)

type pickItem struct {
	ref   store.Ref
	fav   bool
	desc  string
	trust string // "", "new" or "changed"
}

func (it pickItem) label() string {
	star := " "
	if it.fav {
		star = "★"
	}
	s := star + " " + it.ref.ID()
	if it.trust != "" {
		s += "  [" + it.trust + "]"
	}
	if it.desc != "" {
		s += "  " + it.desc
	}
	return s
}

func (a *App) cmdPick() error {
	if !a.Interactive {
		return errors.New("the picker needs a terminal; run mm <name> instead, or mm ls to list macros")
	}
	all := a.store.AllMacros()
	if len(all) == 0 {
		fmt.Fprintln(a.Stderr, "No macros yet. Copy a command, then run: mm add <name>")
		return nil
	}
	ref, action, err := a.pick(all, true)
	if err != nil {
		return err
	}
	if action == actEdit {
		return a.editMacro(ref)
	}
	return a.runMacro(ref, nil)
}

// pick lets the user choose one of refs, with fzf when installed and a
// numbered menu otherwise. With actions, the fzf picker also offers edit and
// favourite toggling.
func (a *App) pick(refs []store.Ref, actions bool) (store.Ref, string, error) {
	if !a.Interactive {
		return store.Ref{}, "", errors.New("the picker needs a terminal; run mm <name> instead")
	}
	if fzf, err := a.lookPath("fzf"); err == nil {
		for {
			ref, key, err := a.pickFzf(fzf, a.pickItems(refs), actions)
			if err != nil {
				return ref, "", err
			}
			switch key {
			case "ctrl-f":
				if err := a.store.UpdateState(func(st *store.State) { st.ToggleFavourite(ref.ID()) }); err != nil {
					return ref, "", err
				}
				continue // reopen with the new order
			case "ctrl-e":
				return ref, actEdit, nil
			}
			return ref, actRun, nil
		}
	}
	ref, err := a.pickMenu(a.pickItems(refs))
	return ref, actRun, err
}

// pickItems orders refs for the picker: favourites, then most recently run,
// then alphabetical.
func (a *App) pickItems(refs []store.Ref) []pickItem {
	items := make([]pickItem, len(refs))
	for i, r := range refs {
		content, _ := r.Read()
		items[i] = pickItem{
			ref:   r,
			fav:   a.store.IsFavourite(r.ID()),
			desc:  macro.Parse(content).Description,
			trust: a.trustStatus(r, content),
		}
	}
	last := a.store.State.LastRun
	sort.SliceStable(items, func(i, j int) bool {
		x, y := items[i], items[j]
		if x.fav != y.fav {
			return x.fav
		}
		tx, ty := last[x.ref.ID()], last[y.ref.ID()]
		if !tx.Equal(ty) {
			return tx.After(ty)
		}
		return x.ref.ID() < y.ref.ID()
	})
	return items
}

func (a *App) pickFzf(fzf string, items []pickItem, actions bool) (store.Ref, string, error) {
	var in bytes.Buffer
	byPath := map[string]store.Ref{}
	for _, it := range items {
		// Field 1 (hidden) is the file path, for the preview and lookup.
		fmt.Fprintf(&in, "%s\t%s\n", it.ref.Path, strings.ReplaceAll(it.label(), "\t", " "))
		byPath[it.ref.Path] = it.ref
	}
	args := []string{
		"--delimiter=\t", "--with-nth=2..", "--prompt=mm> ",
		"--height=60%", "--reverse", "--no-multi",
		"--preview=cat {1}", "--preview-window=right,50%,wrap",
	}
	header := ""
	if actions {
		args = append(args, "--expect=ctrl-e,ctrl-f")
		header = "enter: run · ctrl-e: edit · ctrl-f: favourite"
	}
	for _, it := range items {
		if it.trust != "" {
			header = strings.TrimPrefix(header+" · [new]/[changed]: shared macro you'll be asked to check", " · ")
			break
		}
	}
	if header != "" {
		args = append(args, "--header="+header)
	}
	cmd := exec.Command(fzf, args...)
	cmd.Stdin = &in
	cmd.Stderr = os.Stderr // fzf draws on the terminal through stderr/tty
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && (ee.ExitCode() == 130 || ee.ExitCode() == 1) {
			return store.Ref{}, "", errPickCancelled
		}
		return store.Ref{}, "", fmt.Errorf("fzf failed: %w", err)
	}
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	key := ""
	if actions {
		key, lines = lines[0], lines[1:]
	}
	if len(lines) == 0 {
		return store.Ref{}, "", errPickCancelled
	}
	path, _, _ := strings.Cut(lines[0], "\t")
	ref, ok := byPath[path]
	if !ok {
		return store.Ref{}, "", errPickCancelled
	}
	return ref, key, nil
}

// pickMenu is the built-in fallback: a numbered list where typing a number
// chooses and typing text filters.
func (a *App) pickMenu(items []pickItem) (store.Ref, error) {
	filter := ""
	for {
		shown := items
		if filter != "" {
			shown = nil
			f := strings.ToLower(filter)
			for _, it := range items {
				if strings.Contains(strings.ToLower(it.ref.ID()+" "+it.desc), f) {
					shown = append(shown, it)
				}
			}
			if len(shown) == 0 {
				fmt.Fprintf(a.Stderr, "No macros match %q.\n", filter)
				filter, shown = "", items
			}
		}
		for i, it := range shown {
			fmt.Fprintf(a.Stderr, "%3d  %s\n", i+1, it.label())
		}
		ans, err := a.ask("Number to run, text to filter, Enter to cancel: ")
		if err != nil {
			return store.Ref{}, err
		}
		if ans == "" {
			return store.Ref{}, errPickCancelled
		}
		if n, err := strconv.Atoi(ans); err == nil {
			if n >= 1 && n <= len(shown) {
				return shown[n-1].ref, nil
			}
			fmt.Fprintf(a.Stderr, "Pick a number from 1 to %d.\n", len(shown))
			continue
		}
		filter = ans
	}
}
