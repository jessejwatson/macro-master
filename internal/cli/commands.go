package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"charm.land/lipgloss/v2"
	"charm.land/lipgloss/v2/table"

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
	newFolder := ""
	if _, err := os.Stat(filepath.Dir(ref.Path)); err != nil {
		newFolder = fmt.Sprintf(" (creates folder %s/)", path.Dir(ref.ID()))
	}
	if u := a.errUI(); u.on {
		title := u.bold("Saving ") + u.accent(ref.ID()) + u.faint(newFolder+" from the "+source)
		body := u.code(text)
		if ph := macro.Placeholders(parsed.Body); len(ph) > 0 {
			body += "\n\n" + u.faint("Placeholders: ") + describePlaceholders(ph)
		}
		fmt.Fprintln(a.Stderr, u.panel(title, body, colBorder))
	} else {
		fmt.Fprintf(a.Stderr, "Saving %s%s from the %s:\n\n", ref.ID(), newFolder, source)
		for i, l := range strings.Split(text, "\n") {
			fmt.Fprintf(a.Stderr, "%4d  %s\n", i+1, l)
		}
		fmt.Fprintln(a.Stderr)
		if ph := macro.Placeholders(parsed.Body); len(ph) > 0 {
			fmt.Fprintf(a.Stderr, "Placeholders: %s\n\n", describePlaceholders(ph))
		}
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
			a.nothing("Nothing saved.")
			return errCancelled
		}
	}
	ok, err := a.confirm("Save?", true)
	if err != nil {
		return err
	}
	if !ok {
		a.nothing("Nothing saved.")
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
	u := a.errUI()
	a.done("Saved %s. Run it with: %s", u.bold(ref.ID()), u.fg(colCmd, "mm "+a.store.ShortAddr(ref)))
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
	tmp, err := os.CreateTemp("", "mm-"+ref.Leaf()+"-*"+macro.Ext)
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
		a.nothing("No changes; nothing saved.")
		return nil
	}
	if err := ref.Write(string(edited)); err != nil {
		return err
	}
	a.afterChange(ref.Library, "mm: edit "+ref.Name, ref.Path)
	if err := a.trust(ref, string(edited)); err != nil {
		return err
	}
	a.done("Saved %s.", a.errUI().bold(ref.ID()))
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
	if u := a.outUI(); u.on {
		fmt.Fprintln(a.Stdout, a.showCard(u, ref, m, content))
		return nil
	}
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

// showCard is mm show for a terminal: details, then the macro, in a panel.
func (a *App) showCard(u ui, ref store.Ref, m macro.Macro, content string) string {
	title := u.icon(u.icons.macro, colAccent) + u.accent(ref.ID())
	if a.store.IsFavourite(ref.ID()) {
		title += " " + u.fg(colWarn, u.icons.fav)
	}
	if st := a.trustStatus(ref, content); st != "" {
		title += " " + u.fg(colWarn, "["+st+"]")
	}
	type kv struct{ k, v string }
	var rows []kv
	if m.Description != "" {
		rows = append(rows, kv{"Description", m.Description})
	}
	file := ref.Path
	if home, err := os.UserHomeDir(); err == nil {
		if rest, ok := strings.CutPrefix(file, home+string(filepath.Separator)); ok {
			file = filepath.Join("~", rest)
		}
	}
	rows = append(rows, kv{"File", u.faint(file)})
	if m.Mode != "" {
		rows = append(rows, kv{"Mode", m.Mode})
	}
	if ph := macro.Placeholders(m.Body); len(ph) > 0 {
		rows = append(rows, kv{"Placeholders", describePlaceholders(ph)})
	}
	if t, ok := a.store.State.LastRun[ref.ID()]; ok {
		rows = append(rows, kv{"Last run", t.Local().Format("2 Jan 2006 15:04") + u.faint(" ("+ago(t)+")")})
	}
	var b strings.Builder
	for _, r := range rows {
		fmt.Fprintf(&b, "%s  %s\n", u.faint(fmt.Sprintf("%-12s", r.k)), r.v)
	}
	b.WriteString("\n")
	b.WriteString(u.code(content))
	return u.panel(title, b.String(), colBorder)
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
		a.nothing("Nothing deleted.")
		return errCancelled
	}
	a.beforeChange(ref.Library)
	if err := os.Remove(ref.Path); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("%s was already deleted elsewhere", ref.ID())
		}
		return err
	}
	a.store.RemoveEmptyDirs(ref.Library, filepath.Dir(ref.Path))
	a.afterChange(ref.Library, "mm: rm "+ref.Name, ref.Path)
	if err := a.store.UpdateState(func(st *store.State) { st.Forget(ref.ID()) }); err != nil {
		return err
	}
	a.done("Deleted %s.", a.errUI().bold(ref.ID()))
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
		u := a.errUI()
		fmt.Fprintf(a.Stderr, "%s %s is now a favourite.\n", u.fg(colWarn, "★"), u.bold(ref.ID()))
	} else {
		fmt.Fprintf(a.Stderr, "%s is no longer a favourite.\n", a.errUI().bold(ref.ID()))
	}
	return nil
}

func (a *App) cmdLib(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: mm lib ls | add <name> [git-url] | share <name> <git-url> | rename <old> <new> | rm <name> | default <name>")
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
			u := a.errUI()
			a.done("Created library %s at %s.", u.bold(args[0]), u.faint(a.store.LibraryPath(args[0])))
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
			a.nothing("Nothing removed.")
			return errCancelled
		}
		if err := a.store.RemoveLibrary(name); err != nil {
			return err
		}
		a.done("Removed library %s.", a.errUI().bold(name))
		return nil

	case "rename", "mv":
		if len(args) != 2 {
			return errors.New("usage: mm lib rename <old-name> <new-name>")
		}
		return a.libRename(args[0], args[1])

	case "share":
		if len(args) != 2 {
			return errors.New("usage: mm lib share <name> <git-url>")
		}
		return a.libShare(args[0], args[1])

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
		a.done("New macros now go into %s.", a.errUI().bold(name))
		return nil
	}
	return fmt.Errorf("unknown lib command %q; use ls, add, share, rename, rm or default", sub)
}

// recordRun stamps the macro's last-run time for picker ordering.
func (a *App) recordRun(ref store.Ref) {
	if err := a.store.UpdateState(func(st *store.State) { st.LastRun[ref.ID()] = time.Now().UTC() }); err != nil {
		a.warnf("can't save state: %v", err)
	}
}

// libLs lists libraries with type, access and sync status.
func (a *App) libLs() error {
	u := a.outUI()
	var rows [][]string
	for _, l := range a.store.Libraries() {
		typ, access, status := "local", "writable", ""
		var parts []string
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
			switch {
			case !a.gitOK():
				parts = append(parts, u.fg(colWarn, "git not installed"))
			case ls.SyncError != "":
				parts = append(parts, u.fg(colBad, "sync failed, run mm sync"))
			case ls.LastPull.IsZero():
				parts = append(parts, u.faint("never synced"))
			default:
				parts = append(parts, u.fg(colOK, "synced ")+u.faint(ago(ls.LastPull)))
			}
			if a.gitOK() {
				if n := (gitx.Repo{Dir: l.Path}).Ahead(); n > 0 {
					parts = append(parts, u.fg(colWarn, fmt.Sprintf("%d unpushed", n)))
				}
			}
		}
		if l.Name == a.store.Config.DefaultLibrary {
			parts = append(parts, u.accent("default"))
		}
		status = strings.Join(parts, ", ")
		n := strconv.Itoa(len(a.store.Macros(l.Name)))
		if !u.on {
			rows = append(rows, []string{l.Name, typ, access, n, status})
			continue
		}
		icon := u.icon(u.icons.lib, colAccent)
		if l.Synced {
			icon = u.icon(u.icons.synced, colAccent)
		}
		switch {
		case access == "writable":
			access = u.faint(access)
		case access == "unknown":
			access = u.faint(access)
		default:
			access = u.icon(u.icons.lock, colWarn) + u.fg(colWarn, access)
		}
		rows = append(rows, []string{icon + u.bold(l.Name), typ, access, n, status})
	}
	headers := []string{"LIBRARY", "TYPE", "ACCESS", "MACROS", "STATUS"}
	if u.on {
		t := table.New().
			Border(lipgloss.RoundedBorder()).
			BorderStyle(lipgloss.NewStyle().Foreground(colBorder)).
			Headers("Library", "Type", "Access", "Macros", "Status").
			Rows(rows...).
			StyleFunc(func(row, col int) lipgloss.Style {
				s := lipgloss.NewStyle().Padding(0, 1)
				if row == table.HeaderRow {
					s = s.Foreground(colAccent).Bold(true)
				}
				if col == 3 {
					s = s.Align(lipgloss.Right)
				}
				return s
			})
		fmt.Fprintln(a.Stdout, t.String())
		return nil
	}
	w := tabwriter.NewWriter(a.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, strings.Join(headers, "\t"))
	for _, r := range rows {
		fmt.Fprintln(w, strings.Join(r, "\t"))
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
