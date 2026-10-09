package cli

import (
	"regexp"
	"strings"
)

var (
	helpRow  = regexp.MustCompile(`^(\s+)(\S.*?)(\s{2,})(\S.*)$`)
	helpArgs = regexp.MustCompile(`<[^>]*>|\[[^\]]*\]`)
)

// help colours the help text: headings, then commands in the left column
// with their <args> and [options] dimmed.
func (u ui) help(text string) string {
	if !u.on {
		return text
	}
	lines := strings.Split(text, "\n")
	for i, l := range lines {
		trimmed := strings.TrimSpace(l)
		indent := len(l) - len(strings.TrimLeft(l, " "))
		switch {
		case i == 0:
			name, rest, _ := strings.Cut(l, " ")
			lines[i] = u.accent(name) + " " + u.bold(rest)
		case indent == 0 && strings.HasSuffix(l, ":"):
			lines[i] = u.accent(l)
		case strings.HasPrefix(trimmed, "mm ") || strings.HasPrefix(trimmed, "#") || indent >= 6:
			if m := helpRow.FindStringSubmatch(l); m != nil {
				lines[i] = m[1] + u.helpCmd(m[2]) + m[3] + m[4]
			} else if strings.HasPrefix(trimmed, "mm ") {
				lines[i] = l[:indent] + u.helpCmd(trimmed)
			}
		}
	}
	return strings.Join(lines, "\n")
}

func (u ui) helpCmd(cmd string) string {
	var b strings.Builder
	last := 0
	for _, m := range helpArgs.FindAllStringIndex(cmd, -1) {
		b.WriteString(u.fg(colCmd, cmd[last:m[0]]))
		b.WriteString(u.faint(cmd[m[0]:m[1]]))
		last = m[1]
	}
	b.WriteString(u.fg(colCmd, cmd[last:]))
	return b.String()
}

const helpText = `mm — save and run macro commands

Usage:
  mm                            Open the picker and run the chosen macro
  mm <name> [args...]           Run a macro; extra args become $1, $2, ...
  mm run <name> [args...]       Same, for when a name clashes with a command
  mm -d <name> [args...]        Run a macro detached, in the background

Saving and changing macros:
  mm add <name>                 Save the clipboard as a macro, after a preview
  mm add <lib>/<name>           Same, into a specific library
  mm add web/<name>             Into a folder (made if needed); nest up to 5 deep
  mm add <name> --stdin         Read the macro from stdin instead
  mm edit <name>                Open the macro in $EDITOR (default vi)
  mm rm <name>                  Delete a macro
  mm rename <name> <new>        Rename a macro or folder where it is
  mm mv <name> <dest>           Move a macro or folder (see Moving below)
  mm fav <name>                 Toggle favourite

Looking at macros:
  mm ls [lib|folder] [--fav]    List macros as a tree; ★ marks favourites
  mm show <name>                Print a macro with its details
  mm print <name> [k=v...]      Print the filled-in command, for eval or pipes

Detached jobs:
  mm -d <name> [args...]        Start a job; also mm run -d, or --detach
  mm -d -n <job> <name> [...]   Start a job with a name (-n alone detaches too)
  mm jobs [clear]               List jobs, or delete the finished ones
  mm attach [job]               Watch or type into a job; with no job, the
                                only running one. A job is its number, its
                                name or its macro. Finished jobs show their
                                output
  mm kill [job]                 Stop a job (TERM, then KILL after 3s)
  In mm attach the bottom row shows the keys. Watching: d detaches, ctrl+c
  interrupts the job (again to kill), i starts typing, n names the job.
  Typing: keys go to the job until ctrl-\. Detaching puts your terminal
  back as it was. In the picker, ctrl-d runs a macro detached and running
  jobs are listed first; enter attaches.

Libraries:
  mm lib ls                     List libraries with access and sync status
  mm lib add <name>             Create a local library
  mm lib add <name> <git-url>   Clone a shared library that syncs with git
  mm lib share <name> <git-url> Push a local library to an empty git repo
                                and sync it from then on
  mm lib rename <old> <new>     Rename a library (on this machine only)
  mm lib rm <name>              Remove a library from this machine
  mm lib default <name>         Set the library mm add saves into

Sharing:
  mm sync [lib]                 Pull and push now, showing what happens
  mm trust <name>               Trust a shared macro's current version
  mm auth <host> [--remove]     Store an API token used to check permissions

Settings:
  mm config                     Change settings in a panel; e opens the file
  mm config edit                Edit config.json in $EDITOR, checked on save
  mm config path                Print where config.json is

Shell:
  mm init zsh|bash|fish         Print the shell hook for "# mode: source"
  mm completion zsh|bash|fish   Print tab completion for your shell
  mm help, mm --version

Addressing macros:
  Every macro has a full path: library/folders/name, e.g. infra/web/deploy.
  You can use any end part of it that's unique: deploy, web/deploy or
  infra/web/deploy. If a name matches several macros, mm asks which one.
  Libraries are what sync and share; folders are just for tidiness.

Moving:
  mm mv deploy ship             Plain name: rename in place
  mm mv deploy web/             Trailing /: move into that folder or library
  mm mv deploy infra/web/ship   Path: from the macro's own library, unless
                                it starts with a library name
  Folders move the same way: mm mv web/ ops/  or  mm rename web site
  Favourites and trust move with them.

Placeholders:
  Write {{name}} or {{name:default}} in a macro. mm asks for each one before
  running, or you can pass name=value after the macro name:
      mm deploy-web host=web1
  Values are inserted exactly as typed, not quoted, so quote them yourself
  if they may contain spaces or shell characters.

Macro headers (optional, at the top of the file):
  #!/usr/bin/env python3        Interpreter; bash when there's no #! line
  # description: ...            Shown in the picker and mm ls
  # mode: source                Run in the current shell, so cd and export
                                stick; needs eval "$(mm init zsh)" in ~/.zshrc

Shared libraries:
  Synced libraries pull in the background every 5 minutes and push your
  changes straight after. A shared macro that is new or has changed since
  you trusted it is shown before it runs. Conflicting local versions are
  kept in ~/.config/mm/conflicts.

Ignoring files:
  A .mmignore file in a library lists paths that aren't macros, one a line:
      docs/                     a folder
      scripts/install.sh        a path from the library root
      *-wip.sh                  a name anywhere
  Hidden folders such as .git and .github are always skipped.

Looks:
  Colours and panels appear when writing to a terminal; pipes and
  NO_COLOR=1 get plain text. MM_ICONS=nerd uses Nerd Font icons. The picker
  previews macros with bat when it's installed.

Job settings (in mm config):
  Notify when done: desktop, a notification when a job ends and nobody's
  attached; bell, a bell in attached terminals; both; or off. Finished
  jobs are kept for 7d and at most 50 of them, unless you change it.

Files:
  ~/.config/mm ($XDG_CONFIG_HOME/mm, or $MM_HOME if set) holds config.json,
  state.json, sync.log, conflicts/, jobs/<id>/ (each job's output.log)
  and libraries/<library>/<name>.sh.
`
