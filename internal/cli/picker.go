package cli

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"charm.land/lipgloss/v2"

	"macro-master/internal/jobs"
	"macro-master/internal/macro"
	"macro-master/internal/store"
)

// Picker actions.
const (
	actRun    = "run"
	actEdit   = "edit"
	actDetach = "detach"
	actAttach = "attach"
)

// errPickCancelled is returned when the user leaves the picker.
var errPickCancelled = exitCode(130)

type pickItem struct {
	ref   store.Ref
	fav   bool
	desc  string
	trust string     // "", "new" or "changed"
	job   *jobs.Meta // a running job instead of a macro
}

// key is the hidden first field fzf passes to the preview and back.
func (it pickItem) key() string {
	if it.job != nil {
		return "job:" + it.job.ID
	}
	return it.ref.Path
}

func (it pickItem) label(u ui) string {
	if it.job != nil {
		id := it.job.Macro
		if u.on {
			dir, name := path.Split(id)
			id = u.faint(dir) + u.bold(name)
		}
		return u.fg(colCmd, "●") + " " + id + "  " + u.faint(fmt.Sprintf("%s · running %s", it.job.Title(), it.job.Duration()))
	}
	star, id := " ", it.ref.ID()
	if it.fav {
		star = u.fg(colWarn, "★")
	}
	if u.on {
		dir, name := path.Split(id)
		id = u.faint(dir) + u.bold(name)
	}
	s := star + " " + id
	if it.trust != "" {
		c := colWarn
		if it.trust == trustChanged {
			c = colBad
		}
		s += "  " + u.fg(c, "["+it.trust+"]")
	}
	if it.desc != "" {
		s += "  " + u.faint(it.desc)
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
	var running []*jobs.Meta
	for _, m := range jobs.List(a.store.JobsDir()) {
		if m.Running() {
			running = append(running, m)
		}
	}
	it, action, err := a.pick(all, running, true)
	if err != nil {
		return err
	}
	switch action {
	case actAttach:
		return a.attachJob(it.job)
	case actEdit:
		return a.editMacro(it.ref)
	case actDetach:
		return a.runDetached(it.ref, nil, "")
	}
	return a.runMacro(it.ref, nil)
}

// pick lets the user choose one of refs, with fzf when installed and a
// numbered menu otherwise. With actions, the fzf picker also lists running
// jobs to attach to, and offers edit, detached runs and favourite toggling.
func (a *App) pick(refs []store.Ref, running []*jobs.Meta, actions bool) (pickItem, string, error) {
	if !a.Interactive {
		return pickItem{}, "", errors.New("the picker needs a terminal; run mm <name> instead")
	}
	if fzf, err := a.lookPath("fzf"); err == nil {
		for {
			var items []pickItem
			for _, m := range running {
				items = append(items, pickItem{job: m})
			}
			items = append(items, a.pickItems(refs)...)
			it, key, err := a.pickFzf(fzf, items, actions)
			if err != nil {
				return it, "", err
			}
			if it.job != nil {
				if key == "ctrl-e" || key == "ctrl-f" {
					continue // nothing to edit or favourite
				}
				return it, actAttach, nil
			}
			switch key {
			case "ctrl-f":
				if err := a.store.UpdateState(func(st *store.State) { st.ToggleFavourite(it.ref.ID()) }); err != nil {
					return it, "", err
				}
				continue // reopen with the new order
			case "ctrl-e":
				return it, actEdit, nil
			case "ctrl-d":
				return it, actDetach, nil
			}
			return it, actRun, nil
		}
	}
	ref, err := a.pickMenu(a.pickItems(refs))
	return pickItem{ref: ref}, actRun, err
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

func (a *App) pickFzf(fzf string, items []pickItem, actions bool) (pickItem, string, error) {
	u := a.errUI() // fzf draws on the terminal through stderr/tty
	var in bytes.Buffer
	byKey := map[string]pickItem{}
	for _, it := range items {
		// Field 1 (hidden) is the file path or job, for the preview and lookup.
		fmt.Fprintf(&in, "%s\t%s\n", it.key(), strings.ReplaceAll(it.label(u), "\t", " "))
		byKey[it.key()] = it
	}
	args := []string{
		"--delimiter=\t", "--with-nth=2..",
		"--height=60%", "--reverse", "--no-multi", "--preview=" + a.previewCmd(u),
	}
	ver := fzfVersion(fzf)
	if u.on && ver >= 42 {
		args = append(args,
			"--ansi", "--prompt=❯ ", "--pointer=▸", "--info=inline-right",
			"--border=rounded", "--border-label= macro-master ", "--border-label-pos=3",
			"--preview-window=right,50%,wrap,border-rounded",
			"--color="+fzfBaseScheme()+"border:8,label:5:bold,prompt:5,pointer:5,hl:3,hl+:3:bold,header:8,info:8,separator:8,scrollbar:8,preview-border:8",
		)
		if ver >= 66 {
			args = append(args, "--gutter= ")
		}
	} else {
		args = append(args, "--prompt=mm> ", "--preview-window=right,50%,wrap")
		if u.on {
			args = append(args, "--ansi")
		}
	}
	header := ""
	if actions {
		args = append(args, "--expect=ctrl-e,ctrl-f,ctrl-d")
		header = "enter: run · ctrl-e: edit · ctrl-f: favourite\nctrl-d: run detached"
		for _, it := range items {
			if it.job != nil {
				header += " · ●: running job, enter attaches"
				break
			}
		}
	}
	for _, it := range items {
		if it.trust != "" {
			header = strings.TrimPrefix(header+"\n[new]/[changed]: shared, checked before it runs", "\n")
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
			return pickItem{}, "", errPickCancelled
		}
		return pickItem{}, "", fmt.Errorf("fzf failed: %w", err)
	}
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	key := ""
	if actions {
		key, lines = lines[0], lines[1:]
	}
	if len(lines) == 0 {
		return pickItem{}, "", errPickCancelled
	}
	k, _, _ := strings.Cut(lines[0], "\t")
	it, ok := byKey[k]
	if !ok {
		return pickItem{}, "", errPickCancelled
	}
	return it, key, nil
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
		u := a.errUI()
		for i, it := range shown {
			fmt.Fprintf(a.Stderr, "%s  %s\n", u.faint(fmt.Sprintf("%3d", i+1)), it.label(u))
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

// previewCmd is the fzf preview. mm previews jobs itself and passes macros
// to bat when it's installed, otherwise highlights them when styled, or
// prints them plain.
func (a *App) previewCmd(u ui) string {
	bat := ""
	for _, name := range []string{"bat", "batcat"} {
		if p, err := a.lookPath(name); err == nil {
			bat = p
			break
		}
	}
	exe, err := os.Executable()
	if err != nil {
		if bat != "" {
			return shellQuote(bat) + " --color=always --style=numbers --paging=never {1}"
		}
		return "cat {1}"
	}
	cmd := shellQuote(exe) + " __preview"
	if bat != "" {
		cmd += " --bat " + shellQuote(bat)
	}
	if !u.on {
		cmd += " --plain"
	}
	return cmd + " {1}"
}

// cmdPreview prints a macro file or a job for the fzf preview pane.
func (a *App) cmdPreview(args []string) error {
	bat, plain := "", false
	for len(args) > 1 {
		switch {
		case args[0] == "--bat" && len(args) > 2:
			bat, args = args[1], args[2:]
		case args[0] == "--plain":
			plain, args = true, args[1:]
		default:
			return errors.New("usage: mm __preview [--bat <path>] [--plain] <file|job:id>")
		}
	}
	if len(args) != 1 {
		return errors.New("usage: mm __preview [--bat <path>] [--plain] <file|job:id>")
	}
	if id, ok := strings.CutPrefix(args[0], "job:"); ok {
		return a.previewJob(id, !plain)
	}
	if bat != "" {
		cmd := exec.Command(bat, "--color=always", "--style=numbers", "--paging=never", args[0])
		cmd.Stdout, cmd.Stderr = a.Stdout, a.Stderr
		return cmd.Run()
	}
	b, err := os.ReadFile(args[0])
	if err != nil {
		return err
	}
	if plain {
		a.Stdout.Write(b)
		return nil
	}
	fmt.Fprintln(a.Stdout, a.newUI(true).code(string(b)))
	return nil
}

// previewJob shows a job's details and the end of its output, cleaned of
// everything but colours so it reads in the preview pane.
func (a *App) previewJob(id string, styled bool) error {
	home, err := a.homeDir()
	if err != nil {
		return err
	}
	m, err := jobs.Get(filepath.Join(home, "jobs"), id)
	if err != nil {
		return err
	}
	u := a.newUI(styled)
	status := "running " + m.Duration().String()
	if !m.Running() {
		status = m.Summary()
	}
	fmt.Fprintf(a.Stdout, "%s %s\n%s\n\n", u.accent(m.Title()), m.Macro, u.faint(status+" · enter to attach"))
	lines := strings.Split(strings.TrimRight(cleanOutput(tailBytes(m.LogPath(), 32<<10)), "\n"), "\n")
	if len(lines) > 200 {
		lines = lines[len(lines)-200:]
	}
	fmt.Fprintln(a.Stdout, strings.Join(lines, "\n"))
	return nil
}

// cleanOutput keeps terminal output's text and colours but drops cursor
// movement and other control sequences, and keeps only what's left of a
// line after its last carriage return, as with progress bars.
func cleanOutput(b []byte) string {
	var out strings.Builder
	var line strings.Builder
	flush := func() {
		out.WriteString(line.String())
		line.Reset()
	}
	for i := 0; i < len(b); i++ {
		c := b[i]
		switch {
		case c == 0x1b && i+1 < len(b) && b[i+1] == '[':
			j := i + 2
			for j < len(b) && (b[j] < 0x40 || b[j] > 0x7e) {
				j++
			}
			if j < len(b) && b[j] == 'm' {
				line.Write(b[i : j+1])
			}
			i = j
		case c == 0x1b && i+1 < len(b) && (b[i+1] == ']' || b[i+1] == 'P' || b[i+1] == '_'):
			j := i + 2
			for j < len(b) && b[j] != 0x07 && !(b[j] == 0x1b && j+1 < len(b) && b[j+1] == '\\') {
				j++
			}
			if j < len(b) && b[j] == 0x1b {
				j++
			}
			i = j
		case c == 0x1b:
			i++
		case c == '\n':
			flush()
			out.WriteByte('\n')
		case c == '\r':
			if i+1 < len(b) && b[i+1] == '\n' {
				continue
			}
			line.Reset()
		case c == '\t' || c >= 0x20:
			line.WriteByte(c)
		}
	}
	flush()
	return out.String()
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// fzfBaseScheme is "light," on a light terminal, so the selected row's
// background isn't fzf's dark-scheme grey. Unknown backgrounds stay dark.
func fzfBaseScheme() string {
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return ""
	}
	defer tty.Close()
	if lipgloss.HasDarkBackground(tty, tty) {
		return ""
	}
	return "light,"
}

// fzfVersion returns fzf's minor version (0.42 is 42), or 0 if it can't
// tell. Border labels need 42 and --gutter 66.
func fzfVersion(fzf string) int {
	out, err := exec.Command(fzf, "--version").Output()
	if err != nil {
		return 0
	}
	var major, minor int
	if _, err := fmt.Sscanf(string(out), "%d.%d", &major, &minor); err != nil {
		return 0
	}
	if major > 0 {
		return 1 << 30
	}
	return minor
}
