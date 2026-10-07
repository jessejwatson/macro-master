// Package hosts works out what the current user may do with a git remote,
// by asking the hosting service's API.
package hosts

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Host types.
const (
	GitHub    = "github"
	Gitea     = "gitea" // also Forgejo
	GitLab    = "gitlab"
	Bitbucket = "bitbucket"
	Generic   = "generic"
)

// Remote is a parsed git remote URL.
type Remote struct {
	Host  string // without port
	Owner string // may contain "/" for GitLab subgroups
	Repo  string
	HTTPS bool
}

// Path is "owner/repo".
func (r Remote) Path() string { return r.Owner + "/" + r.Repo }

// ParseRemote understands git@host:owner/repo.git, ssh://[user@]host[:port]/owner/repo.git
// and https://[user@]host[:port]/owner/repo.git.
func ParseRemote(raw string) (Remote, error) {
	raw = strings.TrimSpace(raw)
	var r Remote
	var path string
	switch {
	case strings.Contains(raw, "://"):
		u, err := url.Parse(raw)
		if err != nil {
			return r, fmt.Errorf("can't parse remote %q: %w", raw, err)
		}
		r.Host = u.Hostname()
		r.HTTPS = u.Scheme == "https" || u.Scheme == "http"
		path = u.Path
	default: // scp-like: [user@]host:path
		hostPart, p, ok := strings.Cut(raw, ":")
		if !ok {
			return r, fmt.Errorf("can't parse remote %q", raw)
		}
		if _, h, found := strings.Cut(hostPart, "@"); found {
			hostPart = h
		}
		r.Host, path = hostPart, p
	}
	path = strings.TrimSuffix(strings.Trim(path, "/"), ".git")
	i := strings.LastIndex(path, "/")
	if r.Host == "" || i <= 0 || i == len(path)-1 {
		return Remote{}, fmt.Errorf("can't find owner/repo in remote %q", raw)
	}
	r.Owner, r.Repo = path[:i], path[i+1:]
	return r, nil
}

// Client talks to host APIs.
type Client struct {
	HTTP *http.Client
	// Base returns the scheme and host to use for host, e.g.
	// "https://example.com". Tests point it at a fake server.
	Base func(host string) string
}

// NewClient returns a client with short timeouts, since permission checks
// must never hold up a command for long.
func NewClient() *Client {
	return &Client{
		HTTP: &http.Client{Timeout: 5 * time.Second},
		Base: func(host string) string { return "https://" + host },
	}
}

// KnownType returns the type of a well-known host, or "".
func KnownType(host string) string {
	switch strings.ToLower(host) {
	case "github.com":
		return GitHub
	case "gitlab.com":
		return GitLab
	case "bitbucket.org":
		return Bitbucket
	}
	return ""
}

// Detect works out the host type, probing the Forgejo/Gitea and GitLab
// version endpoints for unknown hosts.
func (c *Client) Detect(host string) string {
	if t := KnownType(host); t != "" {
		return t
	}
	base := c.Base(host)
	if c.probe(base+"/api/v1/version", false) {
		return Gitea
	}
	if c.probe(base+"/api/v4/version", true) {
		return GitLab
	}
	return Generic
}

// probe reports whether url answers like a version endpoint. GitLab's
// needs auth, so a JSON 401 counts when allow401 is set.
func (c *Client) probe(u string, allow401 bool) bool {
	resp, err := c.HTTP.Get(u)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	var body map[string]any
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&body) != nil {
		return false
	}
	if resp.StatusCode == http.StatusOK {
		_, ok := body["version"]
		return ok
	}
	if allow401 && resp.StatusCode == http.StatusUnauthorized {
		_, ok := body["message"]
		return ok
	}
	return false
}

// APIBase returns the API root for a host type. override wins when set.
func (c *Client) APIBase(typ, host, override string) string {
	if override != "" {
		return strings.TrimRight(override, "/")
	}
	base := c.Base(host)
	switch typ {
	case GitHub:
		if strings.EqualFold(host, "github.com") {
			return strings.Replace(base, "://github.com", "://api.github.com", 1)
		}
		return base + "/api/v3" // GitHub Enterprise
	case Gitea:
		return base + "/api/v1"
	case GitLab:
		return base + "/api/v4"
	case Bitbucket:
		return strings.Replace(base, "://bitbucket.org", "://api.bitbucket.org", 1) + "/2.0"
	}
	return ""
}

// Access is the result of a permission check.
type Access int

const (
	Unknown Access = iota
	Writable
	ReadOnly
)

func (a Access) String() string {
	return [...]string{"unknown", "writable", "read-only"}[a]
}

// ErrBadToken means the host rejected the token.
var ErrBadToken = errors.New("the host rejected the token")

func (c *Client) get(typ, u, token string, v any) (int, error) {
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Accept", "application/json")
	if token != "" {
		setAuth(req, typ, token)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, nil
	}
	return resp.StatusCode, json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(v)
}

// setAuth adds each host type's own auth header.
func setAuth(req *http.Request, typ, token string) {
	switch typ {
	case GitHub:
		req.Header.Set("Authorization", "Bearer "+token)
	case Gitea:
		req.Header.Set("Authorization", "token "+token)
	case GitLab:
		req.Header.Set("PRIVATE-TOKEN", token)
	case Bitbucket:
		if user, pass, ok := strings.Cut(token, ":"); ok { // username:app-password
			req.SetBasicAuth(user, pass)
		} else {
			req.Header.Set("Authorization", "Bearer "+token)
		}
	}
}

// Check asks the host whether the token's user can push to the remote.
// Anything the API can't answer is Unknown, not an error.
func (c *Client) Check(typ, api string, r Remote, token string) (Access, error) {
	if api == "" || typ == Generic || typ == "" {
		return Unknown, nil
	}
	switch typ {
	case GitHub, Gitea:
		var body struct {
			Permissions *struct {
				Push bool `json:"push"`
			} `json:"permissions"`
		}
		code, err := c.get(typ, api+"/repos/"+r.Path(), token, &body)
		if err != nil || code != http.StatusOK || body.Permissions == nil {
			return Unknown, statusErr(code, err)
		}
		return boolAccess(body.Permissions.Push), nil

	case GitLab:
		var body struct {
			Permissions struct {
				Project *struct {
					Level int `json:"access_level"`
				} `json:"project_access"`
				Group *struct {
					Level int `json:"access_level"`
				} `json:"group_access"`
			} `json:"permissions"`
		}
		code, err := c.get(typ, api+"/projects/"+url.PathEscape(r.Path()), token, &body)
		if err != nil || code != http.StatusOK {
			return Unknown, statusErr(code, err)
		}
		level := 0
		if p := body.Permissions.Project; p != nil {
			level = p.Level
		}
		if g := body.Permissions.Group; g != nil && g.Level > level {
			level = g.Level
		}
		if token == "" && level == 0 {
			return Unknown, nil // a public project seen anonymously
		}
		return boolAccess(level >= 30), nil // 30 is Developer

	case Bitbucket:
		var body struct {
			Values []struct {
				Permission string `json:"permission"`
			} `json:"values"`
		}
		q := url.Values{"q": {fmt.Sprintf("repository.full_name=%q", r.Path())}}
		code, err := c.get(typ, api+"/user/permissions/repositories?"+q.Encode(), token, &body)
		if err != nil || code != http.StatusOK || len(body.Values) == 0 {
			return Unknown, statusErr(code, err)
		}
		p := body.Values[0].Permission
		return boolAccess(p == "write" || p == "admin"), nil
	}
	return Unknown, nil
}

func boolAccess(w bool) Access {
	if w {
		return Writable
	}
	return ReadOnly
}

func statusErr(code int, err error) error {
	switch {
	case err != nil:
		return err
	case code == http.StatusUnauthorized:
		return ErrBadToken
	}
	return nil
}

// TestToken makes one authenticated call to confirm a token works.
func (c *Client) TestToken(typ, api, token string) error {
	if api == "" {
		return errors.New("this host type has no API mm knows how to check")
	}
	var path string
	switch typ {
	case GitHub, Gitea, GitLab, Bitbucket:
		path = "/user"
	default:
		return errors.New("this host type has no API mm knows how to check")
	}
	var body map[string]any
	code, err := c.get(typ, api+path, token, &body)
	switch {
	case err != nil:
		return err
	case code == http.StatusUnauthorized || code == http.StatusForbidden:
		return ErrBadToken
	case code != http.StatusOK:
		return fmt.Errorf("the host answered %d", code)
	}
	return nil
}
