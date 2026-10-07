// Package gitx runs git for synced libraries. It shells out to the git
// command and never lets git prompt for anything.
package gitx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Available reports whether git is installed.
func Available() bool {
	_, err := exec.LookPath("git")
	return err == nil
}

// Error is a failed git command with its stderr.
type Error struct {
	Args   []string
	Stderr string
	Err    error
}

func (e *Error) Error() string {
	msg := strings.TrimSpace(e.Stderr)
	if msg == "" {
		msg = e.Err.Error()
	}
	return fmt.Sprintf("git %s: %s", e.Args[0], msg)
}

func (e *Error) Unwrap() error { return e.Err }

// Env is the environment git runs with: no terminal prompts, SSH in batch
// mode, English messages so they can be recognised, and no editors.
func Env() []string {
	ssh := os.Getenv("GIT_SSH_COMMAND")
	if ssh == "" {
		ssh = "ssh"
	}
	return append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_SSH_COMMAND="+ssh+" -o BatchMode=yes",
		"GCM_INTERACTIVE=never",
		"LC_ALL=C",
		"GIT_EDITOR=true",
		"GIT_MERGE_AUTOEDIT=no",
	)
}

func run(ctx context.Context, dir string, args ...string) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = Env()
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return out.String(), &Error{Args: args, Stderr: "timed out", Err: ctx.Err()}
	}
	if err != nil {
		return out.String(), &Error{Args: args, Stderr: stderr.String(), Err: err}
	}
	return out.String(), nil
}

// Clone clones url into dest. On failure nothing is left at dest.
func Clone(ctx context.Context, url, dest string) error {
	tmp := filepath.Join(filepath.Dir(dest), "."+filepath.Base(dest)+".cloning")
	os.RemoveAll(tmp)
	if _, err := run(ctx, filepath.Dir(dest), "clone", "--quiet", url, tmp); err != nil {
		os.RemoveAll(tmp)
		return err
	}
	if err := os.Rename(tmp, dest); err != nil {
		os.RemoveAll(tmp)
		return err
	}
	return nil
}

// Repo is a synced library's working copy.
type Repo struct{ Dir string }

func (r Repo) git(ctx context.Context, args ...string) (string, error) {
	return run(ctx, r.Dir, args...)
}

func (r Repo) out(args ...string) string {
	s, _ := r.git(nil, args...)
	return strings.TrimSpace(s)
}

// LockPath is the per-library lock that stops two syncs running at once.
func (r Repo) LockPath() string { return filepath.Join(r.Dir, ".git", "mm.lock") }

// RemoteURL returns origin's URL.
func (r Repo) RemoteURL() string { return r.out("remote", "get-url", "origin") }

// hasHead reports whether the current branch has any commits.
func (r Repo) hasHead() bool {
	_, err := r.git(nil, "rev-parse", "--verify", "--quiet", "HEAD")
	return err == nil
}

// Upstream returns the tracked remote branch, e.g. "origin/main".
// It's missing until the remote has a branch, even when a clone of an empty
// repo has upstream config.
func (r Repo) Upstream() (string, bool) {
	u, err := r.git(nil, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{u}")
	if err != nil {
		return "", false
	}
	if _, err := r.git(nil, "rev-parse", "--verify", "--quiet", "@{u}"); err != nil {
		return "", false
	}
	return strings.TrimSpace(u), true
}

// author returns -c flags to set a commit identity when git has none.
func (r Repo) author() []string {
	if r.out("config", "user.name") != "" && r.out("config", "user.email") != "" {
		return nil
	}
	name := "user"
	if u, err := user.Current(); err == nil {
		name = u.Username
	}
	host, _ := os.Hostname()
	return []string{"-c", "user.name=mm", "-c", "user.email=" + name + "@" + host}
}

// Commit stages paths (or everything, when none are given) and commits
// them. It reports whether a commit was made.
func (r Repo) Commit(msg string, paths ...string) (bool, error) {
	add := []string{"add", "-A", "--"}
	if len(paths) == 0 {
		add = append(add, ".")
	}
	if _, err := r.git(nil, append(add, paths...)...); err != nil {
		return false, err
	}
	if _, err := r.git(nil, "diff", "--cached", "--quiet"); err == nil {
		return false, nil // nothing staged
	}
	args := append(r.author(), "commit", "--quiet", "--no-verify", "-m", msg)
	if _, err := r.git(nil, args...); err != nil {
		return false, err
	}
	return true, nil
}

// Dirty reports whether the working tree has uncommitted changes.
func (r Repo) Dirty() bool { return r.out("status", "--porcelain") != "" }

// Fetch fetches origin and makes sure the branch tracks the remote's
// default branch. It reports false when the remote has no branches yet.
func (r Repo) Fetch(ctx context.Context) (bool, error) {
	if _, err := r.git(ctx, "fetch", "--quiet", "--prune", "origin"); err != nil {
		return false, err
	}
	if _, ok := r.Upstream(); ok {
		return true, nil
	}
	head := r.out("symbolic-ref", "--quiet", "refs/remotes/origin/HEAD")
	if head == "" {
		r.git(ctx, "remote", "set-head", "origin", "--auto")
		head = r.out("symbolic-ref", "--quiet", "refs/remotes/origin/HEAD")
	}
	if head == "" {
		return false, nil // empty remote
	}
	remoteBranch := strings.TrimPrefix(head, "refs/remotes/")
	if !r.hasHead() {
		branch := r.out("symbolic-ref", "--short", "HEAD")
		if _, err := r.git(nil, "checkout", "--quiet", "-B", branch, remoteBranch); err != nil {
			return false, err
		}
	}
	if _, err := r.git(nil, "branch", "--quiet", "--set-upstream-to="+remoteBranch); err != nil {
		return false, err
	}
	return true, nil
}

// Conflict is a file where the remote version won and the local version
// was set aside.
type Conflict struct {
	Path    string // relative to the repo
	SavedAt string // where the local version was saved
}

// SaveFunc stores a set-aside local version of path and returns where.
type SaveFunc func(path string, content []byte) (string, error)

func (r Repo) rebasing() bool {
	for _, d := range []string{"rebase-merge", "rebase-apply"} {
		if _, err := os.Stat(filepath.Join(r.Dir, ".git", d)); err == nil {
			return true
		}
	}
	return false
}

// Rebase replays local commits on the upstream branch. On a conflict the
// remote version is kept and the local one saved with save, except that an
// edit always beats a delete.
func (r Repo) Rebase(save SaveFunc) ([]Conflict, error) {
	up, ok := r.Upstream()
	if !ok {
		return nil, nil
	}
	if !r.hasHead() {
		_, err := r.git(nil, "reset", "--quiet", "--hard", up)
		return nil, err
	}
	var conflicts []Conflict
	_, err := r.git(nil, "rebase", "--quiet", up)
	for i := 0; err != nil && r.rebasing(); i++ {
		if i > 1000 {
			r.git(nil, "rebase", "--abort")
			return conflicts, errors.New("rebase didn't finish; aborted it")
		}
		cs, rerr := r.resolve(save)
		conflicts = append(conflicts, cs...)
		if rerr != nil {
			r.git(nil, "rebase", "--abort")
			return conflicts, rerr
		}
		if _, err = r.git(nil, "rebase", "--continue"); err != nil && r.rebasing() {
			if _, qerr := r.git(nil, "diff", "--cached", "--quiet"); qerr == nil {
				// The resolution left the commit empty; drop it.
				_, err = r.git(nil, "rebase", "--skip")
			}
		}
	}
	return conflicts, err
}

// resolve settles every unmerged file in the current rebase step. During a
// rebase, stage 2 ("ours") is the upstream side and stage 3 ("theirs") is
// the local commit being replayed.
func (r Repo) resolve(save SaveFunc) ([]Conflict, error) {
	stages := map[string]map[int]bool{}
	var order []string
	raw, _ := r.git(nil, "ls-files", "-u", "-z")
	for _, entry := range strings.Split(raw, "\x00") {
		meta, path, ok := strings.Cut(entry, "\t")
		if !ok {
			continue
		}
		f := strings.Fields(meta)
		if len(f) != 3 {
			continue
		}
		n, _ := strconv.Atoi(f[2])
		if stages[path] == nil {
			stages[path] = map[int]bool{}
			order = append(order, path)
		}
		stages[path][n] = true
	}
	var conflicts []Conflict
	for _, path := range order {
		s := stages[path]
		switch {
		case s[2] && s[3]:
			content, err := r.git(nil, "show", ":3:"+path)
			if err != nil {
				return conflicts, err
			}
			saved, err := save(path, []byte(content))
			if err != nil {
				return conflicts, err
			}
			conflicts = append(conflicts, Conflict{Path: path, SavedAt: saved})
			if _, err := r.git(nil, "checkout", "--ours", "--", path); err != nil {
				return conflicts, err
			}
		case s[2]: // deleted locally, edited remotely: keep the edit
			if _, err := r.git(nil, "checkout", "--ours", "--", path); err != nil {
				return conflicts, err
			}
		case s[3]: // deleted remotely, edited locally: keep the edit
			if _, err := r.git(nil, "checkout", "--theirs", "--", path); err != nil {
				return conflicts, err
			}
		}
		if _, err := r.git(nil, "add", "--", path); err != nil {
			return conflicts, err
		}
	}
	return conflicts, nil
}

// Ahead counts local commits not yet pushed.
func (r Repo) Ahead() int {
	if !r.hasHead() {
		return 0
	}
	rng := "HEAD"
	if up, ok := r.Upstream(); ok {
		rng = up + "..HEAD"
	}
	n, _ := strconv.Atoi(r.out("rev-list", "--count", rng))
	return n
}

// Push sends local commits to origin. A rejection for lack of permission
// comes back as a *Rejected error.
func (r Repo) Push(ctx context.Context) error {
	args := []string{"push", "--quiet", "--porcelain", "-u", "origin", "HEAD"}
	if up, ok := r.Upstream(); ok {
		_, branch, _ := strings.Cut(up, "/")
		args = []string{"push", "--quiet", "--porcelain", "origin", "HEAD:refs/heads/" + branch}
	}
	stdout, err := r.git(ctx, args...)
	if err == nil {
		return nil
	}
	var ge *Error
	if errors.As(err, &ge) {
		if reason, ok := PermissionRejection(stdout + "\n" + ge.Stderr); ok {
			return &Rejected{Reason: reason, Err: err}
		}
	}
	return err
}

// Rejected is a push refused because the user may not write.
type Rejected struct {
	Reason string
	Err    error
}

func (e *Rejected) Error() string { return "push rejected: " + e.Reason }
func (e *Rejected) Unwrap() error { return e.Err }

// PermissionRejection recognises git output from a push refused for lack
// of permission, and names the reason.
func PermissionRejection(output string) (string, bool) {
	o := strings.ToLower(output)
	if strings.Contains(o, "permission denied (publickey") {
		return "", false // an SSH key problem, not a permission one
	}
	switch {
	case strings.Contains(o, "protected branch"):
		return "protected branch", true
	case strings.Contains(o, "pre-receive hook declined"):
		return "pre-receive hook declined", true
	case strings.Contains(o, "403"),
		strings.Contains(o, "permission denied"),
		strings.Contains(o, "permission to") && strings.Contains(o, "denied to"),
		strings.Contains(o, "not allowed to push"),
		strings.Contains(o, "write access to repository not granted"),
		strings.Contains(o, "insufficient permission"):
		return "no write permission", true
	}
	return "", false
}

// LocalChanges lists files changed on this machine relative to the
// upstream branch: unpushed commits, uncommitted edits and new files.
func (r Repo) LocalChanges() []string {
	seen := map[string]bool{}
	var out []string
	add := func(s string) {
		for _, p := range strings.Split(s, "\n") {
			if p = strings.TrimSpace(p); p != "" && !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	if up, ok := r.Upstream(); ok && r.hasHead() {
		if base := r.out("merge-base", "HEAD", up); base != "" {
			add(r.out("diff", "--name-only", base))
		}
	} else if r.hasHead() {
		add(r.out("ls-files"))
	}
	add(r.out("ls-files", "--others", "--exclude-standard"))
	return out
}

// ResetToUpstream throws away local commits and edits.
func (r Repo) ResetToUpstream() error {
	up, ok := r.Upstream()
	if !ok {
		return errors.New("no upstream branch to reset to")
	}
	if _, err := r.git(nil, "reset", "--quiet", "--hard", up); err != nil {
		return err
	}
	_, err := r.git(nil, "clean", "--quiet", "-fd")
	return err
}

// FileCommit returns the last commit that touched path, or "".
func (r Repo) FileCommit(path string) string {
	return r.out("log", "-1", "--format=%H", "--", path)
}

// FileLog describes the last commit touching path as "author, date".
func (r Repo) FileLog(path string) string {
	return r.out("log", "-1", "--format=%an <%ae>, %ad", "--date=format:%d %b %Y %H:%M", "--", path)
}

// CommitExists reports whether c is a commit in this repo.
func (r Repo) CommitExists(c string) bool {
	if c == "" {
		return false
	}
	_, err := r.git(nil, "cat-file", "-e", c+"^{commit}")
	return err == nil
}

// Diff shows how path changed since commit, including uncommitted edits.
func (r Repo) Diff(commit, path string) string {
	s, _ := r.git(nil, "diff", "--no-color", commit, "--", path)
	return s
}

// WithTimeout is a convenience for network operations.
func WithTimeout(d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), d)
}
