package cli

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"macro-master/internal/store"
)

// mm looks, at most every few hours, for a newer version than its own, so
// the picker can say there's one and how to get it. Through Homebrew, newer
// means in the tap's formula, as that's what brew upgrade installs;
// otherwise it means a newer tag on GitHub.
const (
	tapFormulaURL = "https://raw.githubusercontent.com/jessejwatson/homebrew-tap/HEAD/Formula/macro-master.rb"
	tagsURL       = "https://api.github.com/repos/jessejwatson/macro-master/tags?per_page=100"
	updateEvery   = 3 * time.Hour
	// updateWait is how long the picker waits on a look before drawing;
	// a slower one shows next time.
	updateWait = 300 * time.Millisecond
)

// updateFile is what the last look found, kept between runs.
type updateFile struct {
	Checked time.Time `json:"checked"`
	Source  string    `json:"source"` // the URL looked at
	Latest  string    `json:"latest"` // e.g. "0.1.12"; "" when none was found
}

func updatePath(home string) string { return filepath.Join(home, "update.json") }

var versionRe = regexp.MustCompile(`v?(\d+)\.(\d+)\.(\d+)`)

// parseVersion reads a release's version, e.g. "0.1.11" or "v0.1.11"; a
// build between releases, e.g. "0.1.11-2-gabc1234", or "dev", isn't one.
func parseVersion(s string) ([3]int, bool) {
	var v [3]int
	m := versionRe.FindStringSubmatch(s)
	if m == nil || m[0] != s {
		return v, false
	}
	for i := range v {
		v[i], _ = strconv.Atoi(m[i+1])
	}
	return v, true
}

func newer(a, b [3]int) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] > b[i]
		}
	}
	return false
}

// viaBrew says whether this mm was installed by Homebrew.
func viaBrew() bool {
	exe, err := os.Executable()
	if err != nil {
		return false
	}
	if p, err := filepath.EvalSymlinks(exe); err == nil {
		exe = p
	}
	return strings.Contains(exe, "/Cellar/macro-master/")
}

// upgradeCommand is how to get the newer version.
func upgradeCommand(brew bool) string {
	if brew {
		return "brew upgrade macro-master"
	}
	return "git pull && make install"
}

func updateSource(brew bool) string {
	if brew {
		return tapFormulaURL
	}
	return tagsURL
}

// updateCheckWanted says whether to look for a newer version at all: not
// for a build between releases, nor with MM_NO_UPDATE_CHECK set.
func (a *App) updateCheckWanted() bool {
	_, ok := parseVersion(a.Version)
	return ok && a.Getenv("MM_NO_UPDATE_CHECK") == "" && a.store != nil
}

// availableUpdate is a newer version than this one, if the last look
// found one, and whether it's time to look again.
func (a *App) availableUpdate(source string) (latest string, stale bool) {
	var f updateFile
	if b, err := os.ReadFile(updatePath(a.store.Home)); err == nil {
		json.Unmarshal(b, &f)
	}
	stale = f.Source != source || time.Since(f.Checked) > updateEvery || f.Checked.After(time.Now())
	if f.Source != source {
		return "", stale
	}
	have, _ := parseVersion(a.Version)
	if v, ok := parseVersion(f.Latest); ok && newer(v, have) {
		return f.Latest, stale
	}
	return "", stale
}

// updateNotice is the line saying there's a newer mm and how to get it, or
// "". When the last look is old it looks again, waiting up to updateWait
// for the answer; past that the look carries on in the background, for
// next time.
func (a *App) updateNotice(u ui) string {
	if !a.updateCheckWanted() {
		return ""
	}
	brew := viaBrew()
	source := updateSource(brew)
	latest, stale := a.availableUpdate(source)
	if stale {
		done := make(chan struct{})
		go func() {
			a.refreshUpdate(source)
			close(done)
		}()
		select {
		case <-done:
			latest, _ = a.availableUpdate(source)
		case <-time.After(updateWait):
		}
	}
	if latest == "" {
		return ""
	}
	return u.fg(colOK, "●") + " " + u.bold("mm "+latest+" is out: ") + u.bold(u.fg(colCmd, upgradeCommand(brew)))
}

// refreshUpdate looks for the newest version at source and keeps what it
// found; on failure it leaves the last look, to try again next time.
func (a *App) refreshUpdate(source string) {
	fetch := a.FetchLatest
	if fetch == nil {
		fetch = fetchLatest
	}
	found, err := fetch(source)
	if err != nil {
		return
	}
	b, err := json.Marshal(updateFile{Checked: time.Now(), Source: source, Latest: found})
	if err != nil {
		return
	}
	store.WriteFileAtomic(updatePath(a.store.Home), b, 0o644)
}

// fetchLatest reads the newest version at source.
func fetchLatest(source string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", source, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "macro-master")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", &httpError{resp.Status}
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	return latestIn(source, b), nil
}

type httpError struct{ status string }

func (e *httpError) Error() string { return e.status }

var tagURLRe = regexp.MustCompile(`refs/tags/(v\d+\.\d+\.\d+)\.tar\.gz`)

// latestIn finds the newest version in what source sent: the formula's
// url, or the highest of the tags.
func latestIn(source string, b []byte) string {
	var names []string
	if source == tapFormulaURL {
		if m := tagURLRe.FindSubmatch(b); m != nil {
			names = append(names, string(m[1]))
		}
	} else {
		var tags []struct {
			Name string `json:"name"`
		}
		json.Unmarshal(b, &tags)
		for _, t := range tags {
			names = append(names, t.Name)
		}
	}
	best, bestV := "", [3]int{}
	for _, n := range names {
		if v, ok := parseVersion(n); ok && (best == "" || newer(v, bestV)) {
			best, bestV = strings.TrimPrefix(n, "v"), v
		}
	}
	return best
}
