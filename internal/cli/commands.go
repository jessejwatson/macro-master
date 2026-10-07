package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"text/tabwriter"
	"time"

	"macro-master/internal/gitx"
	"macro-master/internal/macro"
	"macro-master/internal/store"
)

func (a *App) cmdAdd(args []string) error {
	flags, args, err := splitFlags(args, "--stdin")
	if err != nil {
		return err
	}
	addr, err := oneArg("add", args)
	if err != nil {
		return err
	}
	ref, err := a.store.Target(addr)
	if err != nil {
		return err
	}
	if err := a.checkWritable(ref.Library); err != nil {
		return err
	}

	var raw string
	source := "clipboard"
	if flags["--stdin"] {
		source = "stdin"
		b, err := io.ReadAll(a.Stdin)
		if err != nil {
			return fmt.Errorf("can't read stdin: %w", err)
		}
		raw = string(b)
	} else if raw, err = a.ReadClipboard(); err != nil {
		return err
	}
	text := macro.Clean(raw)
	if strings.TrimSpace(text) == "" {
		if source == "clipboard" {
			return errors.New("the clipboard is empty; copy a command first, or use --stdin")
		}
		return errors.New("stdin was empty; nothing to save")
	}
	if a.Prompts == nil {
		return errors.New("mm add needs a terminal to preview and confirm the save")
	}
	a.beforeChange(ref.Library)

	parsed := macro.Parse(text)
	fmt.Fprintf(a.Stderr, "Saving %s from the %s:\n\n", ref.ID(), source)
	lines := strings.Split(text, "\n")
	for i, l := range lines {
		fmt.Fprintf(a.Stderr, "%4d  %s\n", i+1, l)
	}
	fmt.Fprintln(a.Stderr)
	if ph := macro.Placeholders(parsed.Body); len(ph) > 0 {
		fmt.Fprintf(a.Stderr, "Placeholders: %s\n\n", describePlaceholders(ph))
	}

	q := "Description (optional, Enter to skip): "
	if parsed.Description != "" {
		q = fmt.Sprintf("Description [%s]: ", parsed.Description)
	}
	desc, err := a.ask(q)
	if err != nil {
		return err
	}
	desc = strings.ReplaceAll(desc, "\t", " ")

	if ref.Exists() {
		ok, err := a.confirm(ref.ID()+" already exists. Overwrite it?", false)
		if err != nil {
			return err
		}
		if !ok {
			fmt.Fprintln(a.Stderr, "Nothing saved.")
			return errCancelled
		}
	}
	ok, err := a.confirm("Save?", true)
	if err != nil {
		return err
	}
	if !ok {
		fmt.Fprintln(a.Stderr, "Nothing saved.")
		return errCancelled
	}
	content := macro.Compose(text, desc)
	if err := ref.Write(content); err != nil {
		return fmt.Errorf("can't save %s: %w", ref.Path, err)
	}
	a.afterChange(ref.Library, "mm: add "+ref.Name, ref.Path)
	if err := a.trust(ref, content); err != nil {
		return err
	}
	runAs := ref.Name
	if len(a.store.Find(ref.Name)) > 1 {
		runAs = ref.ID()
	}
	fmt.Fprintf(a.Stderr, "Saved %s. Run it with: mm %s\n", ref.ID(), runAs)
	return nil
}

func describePlaceholders(ph []macro.Placeholder) string {
	parts := make([]string, len(ph))
	for i, p := range ph {
		parts[i] = p.Name
		if p.HasDefault {
			parts[i] += fmt.Sprintf(" (default %q)", p.Default)
		}
	}
	return strings.Join(parts, ", ")
}

func (a *App) cmdEdit(args []string) error {
	addr, err := oneArg("edit", args)
	if err != nil {
		return err
	}
	ref, err := a.resolve(addr)
	if err != nil {
		return err
	}
	return a.editMacro(ref)
}

// editMacro opens a copy of the macro in $EDITOR and saves it back only if
// the editor succeeded and the content changed.
func (a *App) editMacro(ref store.Ref) error {
	if err := a.checkWritable(ref.Library); err != nil {
		return err
	}
	a.beforeChange(ref.Library)
	orig, err := ref.Read()
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp("", "mm-"+ref.Name+"-*"+macro.Ext)
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_, werr := tmp.WriteString(orig)
	if cerr := tmp.Close(); werr != nil || cerr != nil {
		return errors.Join(werr, cerr)
	}

	editor := a.Getenv("EDITOR")
	if editor == "" {
		editor = "vi"
	}
	// Through sh so EDITOR can carry arguments, e.g. "code -w".
	cmd := exec.Command("sh", "-c", editor+` "$1"`, "mm-edit", tmp.Name())
	cmd.Stdin, cmd.Stdout, cmd.Stderr = a.Stdin, a.Stdout, a.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("the editor exited with an error (%v), so nothing was saved", err)
	}
	edited, err := os.ReadFile(tmp.Name())
	if err != nil {
		return err
	}
	if bytes.Equal(edited, []byte(orig)) {
		fmt.Fprintln(a.Stderr, "No changes; nothing saved.")
		return nil
	}
	if err := ref.Write(string(edited)); err != nil {
		return err
	}
	a.afterChange(ref.Library, "mm: edit "+ref.Name, ref.Path)
	if err := a.trust(ref, string(edited)); err != nil {
		return err
	}
	fmt.Fprintf(a.Stderr, "Saved %s.\n", ref.ID())
	return nil
}

func (a *App) cmdShow(args []string) error {
	addr, err := oneArg("show", args)
	if err != nil {
		return err
	}
	ref, err := a.resolve(addr)
	if err != nil {
		return err
	}
	content, err := ref.Read()
	if err != nil {
		return err
	}
	m := macro.Parse(content)
	w := tabwriter.NewWriter(a.Stdout, 0, 0, 2, ' ', 0)
	title := ref.ID()
	if a.store.IsFavourite(ref.ID()) {
		title += "  ★"
	}
	fmt.Fprintln(w, title)
	if m.Description != "" {
		fmt.Fprintf(w, "Description:\t%s\n", m.Description)
	}
	fmt.Fprintf(w, "File:\t%s\n", ref.Path)
	if m.Mode != "" {
		fmt.Fprintf(w, "Mode:\t%s\n", m.Mode)
	}
	if ph := macro.Placeholders(m.Body); len(ph) > 0 {
		fmt.Fprintf(w, "Placeholders:\t%s\n", describePlaceholders(ph))
	}
	if t, ok := a.store.State.LastRun[ref.ID()]; ok {
		fmt.Fprintf(w, "Last run:\t%s\n", t.Local().Format("2 Jan 2006 15:04"))
	}
	w.Flush()
	fmt.Fprintln(a.Stdout, "---")
	fmt.Fprint(a.Stdout, content)
	if !strings.HasSuffix(content, "\n") {
		fmt.Fprintln(a.Stdout)
	}
	return nil
}

func (a *App) cmdRm(args []string) error {
	addr, err := oneArg("rm", args)
	if err != nil {
		return err
	}
	ref, err := a.resolve(addr)
	if err != nil {
		return err
	}
	if err := a.checkWritable(ref.Library); err != nil {
		return err
	}
	ok, err := a.confirm("Delete "+ref.ID()+"?", false)
	if err != nil {
		return err
	}
	if !ok {
		fmt.Fprintln(a.Stderr, "Nothing deleted.")
		return errCancelled
	}
	a.beforeChange(ref.Library)
	if err := os.Remove(ref.Path); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("%s was already deleted elsewhere", ref.ID())
		}
		return err
	}
	a.afterChange(ref.Library, "mm: rm "+ref.Name, ref.Path)
	if err := a.store.UpdateState(func(st *store.State) { st.Forget(ref.ID()) }); err != nil {
		return err
	}
	fmt.Fprintf(a.Stderr, "Deleted %s.\n", ref.ID())
	return nil
}

func (a *App) cmdMv(args []string) error {
	if len(args) != 2 {
		return errors.New("usage: mm mv <name> <new-name or library/new-name>")
	}
	from, err := a.resolve(args[0])
	if err != nil {
		return err
	}
	destAddr := args[1]
	if lib, ok := strings.CutSuffix(destAddr, "/"); ok {
		destAddr = lib + "/" + from.Name // "infra/" keeps the name
	} else if !strings.Contains(destAddr, "/") {
		destAddr = from.Library + "/" + destAddr
	}
	to, err := a.store.Target(destAddr)
	if err != nil {
		return err
	}
	if to.ID() == from.ID() {
		return errors.New("that's the same name and library; nothing to do")
	}
	for _, lib := range []string{from.Library, to.Library} {
		if err := a.checkWritable(lib); err != nil {
			return err
		}
	}
	a.beforeChange(from.Library)
	if to.Library != from.Library {
		a.beforeChange(to.Library)
	}
	if !from.Exists() {
		return fmt.Errorf("%s was deleted elsewhere", from.ID())
	}
	if to.Exists() {
		return fmt.Errorf("%s already exists; remove or rename it first", to.ID())
	}
	before, _ := from.Read()
	wasTrusted := a.trustStatus(from, before) == ""
	if err := os.Rename(from.Path, to.Path); err != nil {
		return err
	}
	if to.Library == from.Library {
		a.afterChange(from.Library, "mm: mv "+from.Name+" to "+to.Name, from.Path, to.Path)
	} else {
		a.afterChange(from.Library, "mm: mv "+from.Name+" to "+to.ID(), from.Path)
		a.afterChange(to.Library, "mm: add "+to.Name+" (moved from "+from.ID()+")", to.Path)
	}
	if err := a.store.UpdateState(func(st *store.State) { st.Rename(from.ID(), to.ID()) }); err != nil {
		return err
	}
	if wasTrusted {
		a.trust(to, before) // a trusted macro stays trusted after a move
	}
	fmt.Fprintf(a.Stderr, "Moved %s to %s.\n", from.ID(), to.ID())
	return nil
}

func (a *App) cmdLs(args []string) error {
	flags, args, err := splitFlags(args, "--fav")
	if err != nil {
		return err
	}
	if len(args) > 1 {
		return errors.New("usage: mm ls [library] [--fav]")
	}
	libs := a.store.Libraries()
	if len(args) == 1 {
		lib, err := a.store.Library(args[0])
		if err != nil {
			return err
		}
		libs = []store.Library{lib}
	}

	w := tabwriter.NewWriter(a.Stdout, 0, 0, 2, ' ', 0)
	shown := 0
	for _, lib := range libs {
		var rows []string
		for _, ref := range a.store.Macros(lib.Name) {
			fav := a.store.IsFavourite(ref.ID())
			if flags["--fav"] && !fav {
				continue
			}
			star := " "
			if fav {
				star = "★"
			}
			rows = append(rows, fmt.Sprintf("  %s %s\t%s", star, ref.Name, a.description(ref)))
		}
		if len(rows) == 0 && flags["--fav"] {
			continue
		}
		if shown > 0 {
			fmt.Fprintln(w)
		}
		fmt.Fprintln(w, lib.Name)
		if len(rows) == 0 {
			fmt.Fprintln(w, "    (empty)")
		}
		for _, r := range rows {
			fmt.Fprintln(w, r)
		}
		shown++
	}
	w.Flush()
	if len(a.store.AllMacros()) == 0 {
		fmt.Fprintln(a.Stderr, "No macros yet. Copy a command, then run: mm add <name>")
	} else if shown == 0 && flags["--fav"] {
		fmt.Fprintln(a.Stderr, "No favourites yet. Add one with: mm fav <name>")
	}
	return nil
}

// description reads a macro's description, or "" if it can't.
func (a *App) description(ref store.Ref) string {
	content, err := ref.Read()
	if err != nil {
		return ""
	}
	return macro.Parse(content).Description
}

func (a *App) cmdFav(args []string) error {
	addr, err := oneArg("fav", args)
	if err != nil {
		return err
	}
	ref, err := a.resolve(addr)
	if err != nil {
		return err
	}
	var on bool
	if err := a.store.UpdateState(func(st *store.State) { on = st.ToggleFavourite(ref.ID()) }); err != nil {
		return err
	}
	if on {
		fmt.Fprintf(a.Stderr, "★ %s is now a favourite.\n", ref.ID())
	} else {
		fmt.Fprintf(a.Stderr, "%s is no longer a favourite.\n", ref.ID())
	}
	return nil
}

func (a *App) cmdLib(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: mm lib ls | add <name> [git-url] | rm <name> | default <name>")
	}
	sub, args := args[0], args[1:]
	switch sub {
	case "ls":
		return a.libLs()

	case "add":
		switch len(args) {
		case 1:
			if err := a.store.CreateLibrary(args[0]); err != nil {
				return err
			}
			fmt.Fprintf(a.Stderr, "Created library %s at %s.\n", args[0], a.store.LibraryPath(args[0]))
			return nil
		case 2:
			return a.addSyncedLibrary(args[0], args[1])
		}
		return errors.New("usage: mm lib add <name> [git-url]")

	case "rm":
		name, err := oneArg("lib rm", args)
		if err != nil {
			return err
		}
		lib, err := a.store.Library(name)
		if err != nil {
			return err
		}
		if name == a.store.Config.DefaultLibrary {
			return fmt.Errorf("%s is the default library; set another with mm lib default <name> first", name)
		}
		n := len(a.store.Macros(name))
		q := fmt.Sprintf("Remove library %s and its %d macro(s) from this machine?", name, n)
		if lib.Synced {
			q = fmt.Sprintf("Remove synced library %s from this machine? The remote copy is kept.", name)
			if n := (gitx.Repo{Dir: lib.Path}).Ahead(); n > 0 && a.gitOK() {
				q = fmt.Sprintf("Remove synced library %s from this machine? %d unpushed commit(s) will be lost.", name, n)
			}
		}
		ok, err := a.confirm(q, false)
		if err != nil {
			return err
		}
		if !ok {
			fmt.Fprintln(a.Stderr, "Nothing removed.")
			return errCancelled
		}
		if err := a.store.RemoveLibrary(name); err != nil {
			return err
		}
		fmt.Fprintf(a.Stderr, "Removed library %s.\n", name)
		return nil

	case "default":
		name, err := oneArg("lib default", args)
		if err != nil {
			return err
		}
		if _, err := a.store.Library(name); err != nil {
			return err
		}
		if err := a.store.UpdateConfig(func(c *store.Config) { c.DefaultLibrary = name }); err != nil {
			return err
		}
		fmt.Fprintf(a.Stderr, "New macros now go into %s.\n", name)
		return nil
	}
	return fmt.Errorf("unknown lib command %q; use ls, add, rm or default", sub)
}

// recordRun stamps the macro's last-run time for picker ordering.
func (a *App) recordRun(ref store.Ref) {
	if err := a.store.UpdateState(func(st *store.State) { st.LastRun[ref.ID()] = time.Now().UTC() }); err != nil {
		fmt.Fprintf(a.Stderr, "mm: warning: can't save state: %v\n", err)
	}
}

// libLs lists libraries with type, access and sync status.
func (a *App) libLs() error {
	w := tabwriter.NewWriter(a.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "LIBRARY\tTYPE\tACCESS\tMACROS\tSTATUS")
	for _, l := range a.store.Libraries() {
		typ, access, status := "local", "writable", ""
		if l.Synced {
			typ = "synced"
			ls := a.libState(l.Name)
			switch ls.Access {
			case store.AccessUnknown:
				access = "unknown"
			case store.AccessReadOnly:
				access = "read-only"
				if ls.AccessReason != "" && ls.AccessReason != "no write permission" {
					access += " (" + ls.AccessReason + ")"
				}
			}
			var parts []string
			switch {
			case !a.gitOK():
				parts = append(parts, "git not installed")
			case ls.SyncError != "":
				parts = append(parts, "sync failed, run mm sync")
			case ls.LastPull.IsZero():
				parts = append(parts, "never synced")
			default:
				parts = append(parts, "synced "+ago(ls.LastPull))
			}
			if a.gitOK() {
				if n := (gitx.Repo{Dir: l.Path}).Ahead(); n > 0 {
					parts = append(parts, fmt.Sprintf("%d unpushed", n))
				}
			}
			status = strings.Join(parts, ", ")
		}
		if l.Name == a.store.Config.DefaultLibrary {
			if status != "" {
				status += ", "
			}
			status += "default"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\n", l.Name, typ, access, len(a.store.Macros(l.Name)), status)
	}
	return w.Flush()
}

// ago formats a past time briefly: "just now", "4m ago", "3h ago", "2d ago".
func ago(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}
