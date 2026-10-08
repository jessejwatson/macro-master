package cli

import (
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"text/tabwriter"

	"charm.land/lipgloss/v2"
	"charm.land/lipgloss/v2/tree"

	"macro-master/internal/store"
)

// cmdLs lists macros as a tree: libraries, then folders indented under
// them. An argument narrows it to one library or folder.
func (a *App) cmdLs(args []string) error {
	flags, args, err := splitFlags(args, "--fav")
	if err != nil {
		return err
	}
	if len(args) > 1 {
		return errors.New("usage: mm ls [library or folder] [--fav]")
	}
	type scope struct{ lib, folder string }
	var scopes []scope
	if len(args) == 1 {
		arg := strings.TrimSuffix(args[0], "/")
		if lib, err := a.store.Library(arg); err == nil {
			scopes = []scope{{lib.Name, ""}}
		} else {
			folders := a.store.FindFolder(arg)
			switch len(folders) {
			case 0:
				return fmt.Errorf("there is no library or folder called %q; run mm ls to see them", arg)
			case 1:
				scopes = []scope{{folders[0].Library, folders[0].Path}}
			default:
				ids := make([]string, len(folders))
				for i, f := range folders {
					ids[i] = f.ID() + "/"
				}
				return fmt.Errorf("%q matches more than one folder; use one of: %s", arg, strings.Join(ids, ", "))
			}
		}
	} else {
		for _, l := range a.store.Libraries() {
			scopes = append(scopes, scope{l.Name, ""})
		}
	}

	w := tabwriter.NewWriter(a.Stdout, 0, 0, 2, ' ', 0)
	shown := 0
	var skipped []string
	for _, sc := range scopes {
		scan := a.store.ScanLibrary(sc.lib)
		if sc.folder == "" {
			for _, s := range scan.Skipped {
				skipped = append(skipped, sc.lib+"/"+s)
			}
		}
		refs := map[string]store.Ref{} // path relative to the scope
		for _, r := range scan.Macros {
			rel := r.Name
			if sc.folder != "" {
				var ok bool
				if rel, ok = strings.CutPrefix(r.Name, sc.folder+"/"); !ok {
					continue
				}
			}
			if flags["--fav"] && !a.store.IsFavourite(r.ID()) {
				continue
			}
			refs[rel] = r
		}
		if len(refs) == 0 && flags["--fav"] {
			continue
		}
		if shown > 0 {
			fmt.Fprintln(w)
		}
		if u := a.outUI(); u.on {
			fmt.Fprintln(a.Stdout, a.styledTree(u, sc.lib, sc.folder, refs))
			shown++
			continue
		}
		header := sc.lib
		if sc.folder != "" {
			header += "/" + sc.folder + "/"
		}
		fmt.Fprintln(w, header)
		if len(refs) == 0 {
			fmt.Fprintln(w, "    (empty)")
		}
		a.printTree(w, refs, "", 0)
		shown++
	}
	w.Flush()
	if len(a.store.AllMacros()) == 0 {
		fmt.Fprintln(a.Stderr, "No macros yet. Copy a command, then run: mm add <name>")
	} else if shown == 0 && flags["--fav"] {
		fmt.Fprintln(a.Stderr, "No favourites yet. Add one with: mm fav <name>")
	}
	if len(skipped) > 0 {
		u := a.errUI()
		fmt.Fprintf(a.Stderr, "\n%s%d item(s) skipped:\n", u.icon(u.icons.warn, colWarn), len(skipped))
		for _, s := range skipped {
			fmt.Fprintf(a.Stderr, "  %s\n", s)
		}
	}
	return nil
}

// printTree prints the macros under prefix at this level, then each
// subfolder indented beneath its name.
func (a *App) printTree(w io.Writer, refs map[string]store.Ref, prefix string, depth int) {
	var leaves []string
	folders := map[string]bool{}
	for rel := range refs {
		rest, ok := strings.CutPrefix(rel, prefix)
		if !ok {
			continue
		}
		if dir, _, isFolder := strings.Cut(rest, "/"); isFolder {
			folders[dir] = true
		} else {
			leaves = append(leaves, rest)
		}
	}
	sort.Strings(leaves)
	indent := strings.Repeat("  ", depth)
	for _, leaf := range leaves {
		r := refs[prefix+leaf]
		star := " "
		if a.store.IsFavourite(r.ID()) {
			star = "★"
		}
		// Stars sit in a fixed left column so names indent only by depth.
		line := fmt.Sprintf("  %s %s%s", star, indent, leaf)
		if desc := a.description(r); desc != "" {
			line += "\t" + desc
		}
		fmt.Fprintln(w, line)
	}
	names := make([]string, 0, len(folders))
	for f := range folders {
		names = append(names, f)
	}
	sort.Strings(names)
	for _, f := range names {
		fmt.Fprintf(w, "    %s%s/\n", indent, f)
		a.printTree(w, refs, prefix+f+"/", depth+1)
	}
}

// styledTree draws one library (or folder) as a tree with branch lines,
// favourites starred and descriptions lined up in a dimmed column.
func (a *App) styledTree(u ui, lib, folder string, refs map[string]store.Ref) string {
	l, _ := a.store.Library(lib)
	root := u.icon(u.icons.lib, colAccent)
	if l.Synced {
		root = u.icon(u.icons.synced, colAccent)
	}
	root += u.accent(lib)
	if folder != "" {
		root += u.fg(colFolder, "/"+folder+"/")
	}
	count := fmt.Sprintf("%d macro", len(refs))
	if len(refs) != 1 {
		count += "s"
	}
	root += "  " + u.faint(count)

	label := func(r store.Ref, name string) string {
		l := u.icon(u.icons.macro, colBorder) + name
		if a.store.IsFavourite(r.ID()) {
			l = u.fg(colWarn, u.icons.fav) + " " + l
		}
		return l
	}
	// Descriptions line up just past the widest label, counting the four
	// columns of branch lines each level adds.
	descs := map[string]string{}
	descCol := 0
	for rel, r := range refs {
		if descs[rel] = a.description(r); descs[rel] != "" {
			depth := strings.Count(rel, "/") + 1
			descCol = max(descCol, 4*depth+lipgloss.Width(label(r, path.Base(rel))))
		}
	}

	branch := lipgloss.NewStyle().Foreground(colBorder).PaddingRight(1)
	newTree := func(root string) *tree.Tree {
		return tree.Root(root).Enumerator(tree.RoundedEnumerator).EnumeratorStyle(branch).IndenterStyle(branch)
	}
	var build func(t *tree.Tree, prefix string, depth int)
	build = func(t *tree.Tree, prefix string, depth int) {
		var names []string
		folders := map[string]bool{}
		for rel := range refs {
			rest, ok := strings.CutPrefix(rel, prefix)
			if !ok {
				continue
			}
			if dir, _, isFolder := strings.Cut(rest, "/"); isFolder {
				folders[dir] = true
			} else {
				names = append(names, rest)
			}
		}
		sort.Strings(names)
		for _, name := range names {
			r := refs[prefix+name]
			l := label(r, name)
			if d := descs[prefix+name]; d != "" {
				pad := descCol - 4*(depth+1) - lipgloss.Width(l) + 2
				l += strings.Repeat(" ", pad) + u.faint(d)
			}
			t.Child(l)
		}
		dirs := make([]string, 0, len(folders))
		for f := range folders {
			dirs = append(dirs, f)
		}
		sort.Strings(dirs)
		for _, f := range dirs {
			sub := newTree(u.icon(u.icons.folder, colFolder) + u.fg(colFolder, f+"/"))
			build(sub, prefix+f+"/", depth+1)
			t.Child(sub)
		}
	}
	t := newTree(root)
	build(t, "", 0)
	if len(refs) == 0 {
		t.Child(u.faint("(empty)"))
	}
	return t.String()
}
