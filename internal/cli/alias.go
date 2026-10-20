package cli

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"text/tabwriter"

	"charm.land/lipgloss/v2"
	"charm.land/lipgloss/v2/table"

	"macro-master/internal/macro"
	"macro-master/internal/store"
)

// shellBuiltins are bash, zsh and fish builtins and keywords. An alias
// can never take one of these names: shadowing cd or if with a macro is
// never what anyone wants.
var shellBuiltins = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`
		abbr alias and argparse autoload begin bg bind bindkey block break
		breakpoint builtin bye caller case cd chdir command commandline
		compadd comparguments compcall compctl compdescribe compfiles compgen
		complete compgroups compopt compquote compset comptags comptry
		compvalues contains continue coproc count declare dirs disable disown
		do done echo echotc echoti elif else emit emulate enable end esac eval
		exec exit export false fc fg fi float for foreach function functions
		getln getopts hash help history if in integer jobs kill let limit local
		log logout mapfile math nocorrect noglob not or path popd print printf
		private pushd pushln pwd random read readarray readonly realpath rehash
		repeat return sched select set set_color setopt shift shopt source
		status string suspend test then time times trap true ttyctl type
		typeset ulimit umask unalias unfunction unhash unlimit unset unsetopt
		until vared wait whence where which while zcompile zformat zle zmodload
		zparseopts zregexparse zstyle`) {
		shellBuiltins[w] = true
	}
}

func (a *App) cmdAlias(args []string) error {
	if len(args) == 0 {
		return a.aliasLs()
	}
	sub, args := args[0], args[1:]
	switch sub {
	case "ls":
		if len(args) > 0 {
			return errors.New("usage: mm alias ls")
		}
		return a.aliasLs()
	case "add":
		force, args := cutForce(args)
		if len(args) < 2 {
			return errors.New("usage: mm alias add <alias> <macro> [k=v...] [--force]")
		}
		ref, err := a.resolve(args[1])
		if err != nil {
			return err
		}
		return a.addAlias(args[0], ref, args[2:], force)
	case "rm":
		name, err := oneArg("alias rm", args)
		if err != nil {
			return err
		}
		al, ok := a.store.Config.Aliases[name]
		if !ok {
			return fmt.Errorf("there is no alias called %q; run mm alias ls to see them", name)
		}
		if err := a.store.UpdateConfig(func(c *store.Config) { delete(c.Aliases, name) }); err != nil {
			return err
		}
		a.done("Removed alias %s (it ran %s).", a.errUI().bold(name), describeAlias(al))
		return nil
	case "accept":
		force, names := cutForce(args)
		return a.aliasAccept(names, force)
	}
	return fmt.Errorf("unknown alias command %q; use ls, add, rm or accept", sub)
}

// cutForce pulls --force out of args, which may otherwise hold anything
// to pass to the macro.
func cutForce(args []string) (bool, []string) {
	return cutFlag(args, "--force")
}

// cutFlag pulls the boolean flag out of args, which may otherwise hold
// anything to pass to a macro.
func cutFlag(args []string, flag string) (bool, []string) {
	found := false
	var rest []string
	for _, a := range args {
		if a == flag {
			found = true
			continue
		}
		rest = append(rest, a)
	}
	return found, rest
}

// describeAlias is what an alias runs: the macro and its preset args.
func describeAlias(al store.Alias) string {
	return strings.Join(append([]string{al.Macro}, al.Args...), " ")
}

// checkAliasName reports why name can't be a new alias. A command of the
// same name on PATH is allowed with force; builtins, mm's own words and
// macro names never are, the last so mm <name> can't mean two things.
func (a *App) checkAliasName(name string, force bool) error {
	if err := store.ValidateAliasName(name); err != nil {
		return err
	}
	if name == "mm" || macro.Reserved[name] {
		return fmt.Errorf("%q is a word mm uses itself; pick another alias name", name)
	}
	if shellBuiltins[name] {
		return fmt.Errorf("%q is a shell builtin or keyword; pick another alias name", name)
	}
	if refs := a.store.Find(name); len(refs) > 0 {
		return fmt.Errorf("mm %s already runs the macro %s; pick another alias name", name, refs[0].ID())
	}
	if p, err := a.lookPath(name); err == nil && !force {
		return fmt.Errorf("%q is already a command (%s); pick another name, or pass --force to shadow it in your shell", name, p)
	}
	return nil
}

// addAlias saves name as an alias for ref with preset args.
func (a *App) addAlias(name string, ref store.Ref, args []string, force bool) error {
	al := store.Alias{Macro: ref.ID(), Args: args, Force: force}
	if old, ok := a.store.Config.Aliases[name]; ok {
		if old.Macro == al.Macro && slices.Equal(old.Args, al.Args) {
			a.nothing(fmt.Sprintf("%s already runs %s.", name, describeAlias(al)))
			return nil
		}
		if a.Prompts == nil {
			return fmt.Errorf("%s already runs %s; remove it first with mm alias rm %s", name, describeAlias(old), name)
		}
		ok, err := a.confirm(fmt.Sprintf("%s already runs %s. Point it at %s instead?", name, describeAlias(old), describeAlias(al)), false)
		if err != nil {
			return err
		}
		if !ok {
			a.nothing("Alias unchanged.")
			return errCancelled
		}
		al.Force = al.Force || old.Force
	} else if err := a.checkAliasName(name, force); err != nil {
		return err
	}
	if err := a.store.UpdateConfig(func(c *store.Config) {
		if c.Aliases == nil {
			c.Aliases = map[string]store.Alias{}
		}
		c.Aliases[name] = al
	}); err != nil {
		return err
	}
	u := a.errUI()
	a.done("%s now runs %s.", u.fg(colCmd, name), u.bold(describeAlias(al)))
	a.aliasHookHint(name)
	return nil
}

// aliasHookHint says when a new alias can be typed on its own.
func (a *App) aliasHookHint(name string) {
	shell := a.Getenv("MM_SHELL")
	switch {
	case shell == "":
		a.notef("To type %s without mm, load the shell hook: add eval \"$(mm init zsh)\" to ~/.zshrc (or the bash/fish equivalent). Until then, mm %s runs it.", name, name)
	case a.Getenv("MM_ALIAS_HOOK") == "":
		a.notef("Your shell has an older mm hook; open a new terminal to type %s without mm. Until then, mm %s runs it.", name, name)
	}
}

// aliasRef finds the macro an alias points at.
func (a *App) aliasRef(name string, al store.Alias) (store.Ref, error) {
	for _, r := range a.store.AllMacros() {
		if r.ID() == al.Macro {
			return r, nil
		}
	}
	return store.Ref{}, fmt.Errorf("the alias %s runs %s, which doesn't exist any more; point it elsewhere with mm alias add %s <macro>, or remove it with mm alias rm %s", name, al.Macro, name, name)
}

// resolveRun resolves what mm <addr> runs: a macro, or, when no macro
// matches, an alias, whose preset args go before args.
func (a *App) resolveRun(addr string, args []string) (store.Ref, []string, error) {
	if al, ok := a.store.Config.Aliases[addr]; ok && len(a.store.Find(addr)) == 0 {
		ref, err := a.aliasRef(addr, al)
		return ref, append(slices.Clone(al.Args), args...), err
	}
	ref, err := a.resolve(addr)
	return ref, args, err
}

// cmdAliasRun is mm __alias <name> [args...], what an alias's shell
// function runs.
func (a *App) cmdAliasRun(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: mm __alias <alias> [args...]")
	}
	name := args[0]
	al, ok := a.store.Config.Aliases[name]
	if !ok {
		return fmt.Errorf("%s is no longer an mm alias; it goes from your shell at the next prompt", name)
	}
	ref, err := a.aliasRef(name, al)
	if err != nil {
		return err
	}
	return a.runMacro(ref, append(slices.Clone(al.Args), args[1:]...))
}

// aliasSuggestion is a "# alias:" header line in a macro.
type aliasSuggestion struct {
	ref  store.Ref
	name string
	args []string
}

// suggestions lists the aliases macros suggest that aren't aliases yet.
func (a *App) suggestions() []aliasSuggestion {
	var out []aliasSuggestion
	for _, r := range a.store.AllMacros() {
		if content, err := r.Read(); err == nil {
			out = append(out, a.suggestionsFor(r, macro.Parse(content).Aliases)...)
		}
	}
	return out
}

// suggestionsFor turns a macro's "# alias:" lines into suggestions,
// leaving out names already taken by an alias.
func (a *App) suggestionsFor(r store.Ref, lines []string) []aliasSuggestion {
	var out []aliasSuggestion
	for _, l := range lines {
		f := strings.Fields(l)
		if len(f) == 0 {
			continue
		}
		if _, taken := a.store.Config.Aliases[f[0]]; taken {
			continue
		}
		out = append(out, aliasSuggestion{ref: r, name: f[0], args: f[1:]})
	}
	return out
}

func (s aliasSuggestion) describe() string {
	return strings.Join(append([]string{s.ref.ID()}, s.args...), " ")
}

// aliasAccept adds suggested aliases: the named ones, or with no names,
// each suggestion after asking.
func (a *App) aliasAccept(names []string, force bool) error {
	all := a.suggestions()
	if len(names) == 0 {
		if len(all) == 0 {
			a.nothing("No macros suggest aliases you don't have.")
			return nil
		}
		if a.Prompts == nil {
			return errors.New("mm alias accept with no names asks about each suggestion, which needs a terminal; name the ones to accept instead")
		}
		added := 0
		for _, s := range all {
			if err := a.checkAliasName(s.name, force); err != nil {
				a.warnf("can't add %s: %v", s.name, err)
				continue
			}
			ok, err := a.confirm(fmt.Sprintf("Add alias %s for %s?", s.name, s.describe()), true)
			if err != nil {
				return err
			}
			if !ok {
				continue
			}
			if err := a.addAlias(s.name, s.ref, s.args, force); err != nil {
				return err
			}
			added++
		}
		if added == 0 {
			a.nothing("No aliases added.")
		}
		return nil
	}
	for _, name := range names {
		var match []aliasSuggestion
		for _, s := range all {
			if s.name == name {
				match = append(match, s)
			}
		}
		switch len(match) {
		case 0:
			if al, ok := a.store.Config.Aliases[name]; ok {
				return fmt.Errorf("%s is already an alias, for %s", name, describeAlias(al))
			}
			return fmt.Errorf("no macro suggests an alias called %q; run mm alias ls to see suggestions", name)
		case 1:
			if err := a.addAlias(name, match[0].ref, match[0].args, force); err != nil {
				return err
			}
		default:
			ids := make([]string, len(match))
			for i, s := range match {
				ids[i] = s.ref.ID()
			}
			return fmt.Errorf("more than one macro suggests %s (%s); choose with mm alias add %s <macro>", name, strings.Join(ids, ", "), name)
		}
	}
	return nil
}

// aliasLs lists aliases with what they run and any problem, then the
// aliases macros suggest.
func (a *App) aliasLs() error {
	u := a.outUI()
	aliases := a.store.Config.Aliases
	skipped := strings.Fields(a.Getenv("MM_ALIAS_SKIPPED"))
	var rows [][]string
	for _, name := range slices.Sorted(maps.Keys(aliases)) {
		al := aliases[name]
		var status []string
		if _, err := a.aliasRef(name, al); err != nil {
			status = append(status, u.fg(colBad, "macro missing"))
		}
		if slices.Contains(skipped, name) {
			status = append(status, u.fg(colWarn, "name taken in your shell, so not defined"))
		}
		if refs := a.store.Find(name); len(refs) > 0 {
			status = append(status, u.fg(colWarn, "mm "+name+" runs "+refs[0].ID()))
		}
		if al.Force {
			status = append(status, u.faint("shadows commands"))
		}
		args := strings.Join(al.Args, " ")
		if u.on {
			rows = append(rows, []string{u.fg(colCmd, name), u.bold(al.Macro), u.faint(args), strings.Join(status, ", ")})
		} else {
			rows = append(rows, []string{name, al.Macro, args, strings.Join(status, ", ")})
		}
	}
	if len(rows) > 0 {
		if u.on {
			t := table.New().
				Border(lipgloss.RoundedBorder()).
				BorderStyle(lipgloss.NewStyle().Foreground(colBorder)).
				Headers("Alias", "Macro", "Args", "Status").
				Rows(rows...).
				StyleFunc(func(row, col int) lipgloss.Style {
					s := lipgloss.NewStyle().Padding(0, 1)
					if row == table.HeaderRow {
						s = s.Foreground(colAccent).Bold(true)
					}
					return s
				})
			fmt.Fprintln(a.Stdout, t.String())
		} else {
			w := tabwriter.NewWriter(a.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "ALIAS\tMACRO\tARGS\tSTATUS")
			for _, r := range rows {
				fmt.Fprintln(w, strings.Join(r, "\t"))
			}
			w.Flush()
		}
	}

	sugg := a.suggestions()
	if len(rows) == 0 && len(sugg) == 0 {
		fmt.Fprintln(a.Stderr, "No aliases yet. Add one with: mm alias add <alias> <macro>")
		return nil
	}
	if len(rows) > 0 && a.Getenv("MM_SHELL") == "" {
		eu := a.errUI()
		fmt.Fprintln(a.Stderr, eu.faint(`Aliases need the shell hook to work without mm: add eval "$(mm init zsh)" to ~/.zshrc (or the bash/fish equivalent).`))
	}
	if len(sugg) > 0 {
		if len(rows) > 0 {
			fmt.Fprintln(a.Stdout)
		}
		fmt.Fprintln(a.Stdout, u.accent("Suggested by macros")+u.faint(" (accept with mm alias accept <alias>):"))
		width := 0
		for _, s := range sugg {
			width = max(width, len(s.name))
		}
		for _, s := range sugg {
			// Padded before colouring, so the colour codes don't count.
			fmt.Fprintf(a.Stdout, "  %s  %s\n", u.fg(colCmd, fmt.Sprintf("%-*s", width, s.name)), s.describe())
		}
	}
	return nil
}

// aliasLabel shows a macro's alias names in listings: " (d, dp)", or ""
// when it has none.
func aliasLabel(u ui, names []string) string {
	if len(names) == 0 {
		return ""
	}
	if !u.on {
		return " (" + strings.Join(names, ", ") + ")"
	}
	parts := make([]string, len(names))
	for i, n := range names {
		parts[i] = u.fg(colCmd, n)
	}
	return " " + u.faint("(") + strings.Join(parts, u.faint(", ")) + u.faint(")")
}

// retargetAliases keeps aliases and key bindings pointing at macros that
// moved; see store.Alias.Targets.
func (a *App) retargetAliases(from, to string, prefix bool) error {
	targets := func(id string) bool { return store.Alias{Macro: id}.Targets(from, prefix) }
	for _, al := range a.store.Config.Aliases {
		if targets(al.Macro) {
			return a.store.UpdateConfig(func(c *store.Config) {
				c.RetargetAliases(from, to, prefix)
				c.RetargetBindings(from, to, prefix)
			})
		}
	}
	for _, b := range a.store.Config.Bindings {
		if targets(b.Macro) {
			return a.store.UpdateConfig(func(c *store.Config) { c.RetargetBindings(from, to, prefix) })
		}
	}
	return nil
}

// describeAliases is a macro's aliases with their preset args, for mm
// show: "d, dp env=prod".
func (a *App) describeAliases(ref store.Ref) string {
	var out []string
	for _, n := range a.store.Config.AliasesFor(ref.ID()) {
		out = append(out, strings.Join(append([]string{n}, a.store.Config.Aliases[n].Args...), " "))
	}
	return strings.Join(out, ", ")
}
