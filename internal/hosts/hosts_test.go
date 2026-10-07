package hosts

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseRemote(t *testing.T) {
	cases := map[string]Remote{
		"git@github.com:acme/macros.git":                 {Host: "github.com", Owner: "acme", Repo: "macros"},
		"github.com:acme/macros":                         {Host: "github.com", Owner: "acme", Repo: "macros"},
		"ssh://git@code.example.com:2222/team/infra.git": {Host: "code.example.com", Owner: "team", Repo: "infra"},
		"https://gitlab.com/group/sub/repo.git":          {Host: "gitlab.com", Owner: "group/sub", Repo: "repo", HTTPS: true},
		"https://user@bitbucket.org/acme/macros.git/":    {Host: "bitbucket.org", Owner: "acme", Repo: "macros", HTTPS: true},
		"http://localhost:3000/me/macros":                {Host: "localhost", Owner: "me", Repo: "macros", HTTPS: true},
	}
	for in, want := range cases {
		got, err := ParseRemote(in)
		if err != nil || got != want {
			t.Errorf("%s: got %+v, %v; want %+v", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "/local/path/repo.git", "https://github.com/onlyowner", "git@host:"} {
		if _, err := ParseRemote(bad); err == nil {
			t.Errorf("%q: expected an error", bad)
		}
	}
}

// fake serves canned JSON and records the auth headers it saw.
type fake struct {
	routes map[string]string // path?query -> body; "401" body means unauthorised
	auth   []string
}

func (f *fake) server(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.auth = append(f.auth, r.Header.Get("Authorization")+r.Header.Get("PRIVATE-TOKEN"))
		key := r.URL.EscapedPath()
		if r.URL.RawQuery != "" {
			key += "?" + r.URL.RawQuery
		}
		body, ok := f.routes[key]
		switch {
		case !ok:
			http.NotFound(w, r)
		case strings.HasPrefix(body, "401"):
			w.WriteHeader(401)
			w.Write([]byte(strings.TrimPrefix(body, "401")))
		default:
			w.Write([]byte(body))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func client(srv *httptest.Server) *Client {
	c := NewClient()
	c.Base = func(string) string { return srv.URL }
	return c
}

func TestDetect(t *testing.T) {
	f := &fake{routes: map[string]string{"/api/v1/version": `{"version":"9.0"}`}}
	if got := client(f.server(t)).Detect("forge.example.com"); got != Gitea {
		t.Errorf("gitea: %s", got)
	}
	f = &fake{routes: map[string]string{"/api/v4/version": `401{"message":"401 Unauthorized"}`}}
	if got := client(f.server(t)).Detect("git.example.com"); got != GitLab {
		t.Errorf("gitlab: %s", got)
	}
	f = &fake{routes: map[string]string{}}
	if got := client(f.server(t)).Detect("plain.example.com"); got != Generic {
		t.Errorf("generic: %s", got)
	}
	if got := client(f.server(t)).Detect("github.com"); got != GitHub {
		t.Errorf("github: %s", got)
	}
}

func TestCheck(t *testing.T) {
	r := Remote{Host: "h", Owner: "acme", Repo: "macros"}
	cases := []struct {
		name, typ, path, body, token string
		want                         Access
		wantAuth                     string
	}{
		{"github push", GitHub, "/repos/acme/macros", `{"permissions":{"push":true}}`, "t", Writable, "Bearer t"},
		{"github read", GitHub, "/repos/acme/macros", `{"permissions":{"push":false}}`, "t", ReadOnly, "Bearer t"},
		{"github anon", GitHub, "/repos/acme/macros", `{"name":"macros"}`, "", Unknown, ""},
		{"gitea push", Gitea, "/repos/acme/macros", `{"permissions":{"push":true}}`, "t", Writable, "token t"},
		{"gitlab dev", GitLab, "/projects/acme%2Fmacros", `{"permissions":{"project_access":{"access_level":30},"group_access":null}}`, "t", Writable, "t"},
		{"gitlab group", GitLab, "/projects/acme%2Fmacros", `{"permissions":{"project_access":null,"group_access":{"access_level":40}}}`, "t", Writable, "t"},
		{"gitlab reporter", GitLab, "/projects/acme%2Fmacros", `{"permissions":{"project_access":{"access_level":20}}}`, "t", ReadOnly, "t"},
		{"bitbucket write", Bitbucket, `/user/permissions/repositories?q=repository.full_name%3D%22acme%2Fmacros%22`, `{"values":[{"permission":"write"}]}`, "t", Writable, "Bearer t"},
		{"bitbucket read", Bitbucket, `/user/permissions/repositories?q=repository.full_name%3D%22acme%2Fmacros%22`, `{"values":[{"permission":"read"}]}`, "u:p", ReadOnly, "Basic dTpw"},
		{"not found", GitHub, "/nope", ``, "t", Unknown, "Bearer t"},
	}
	for _, c := range cases {
		f := &fake{routes: map[string]string{c.path: c.body}}
		srv := f.server(t)
		got, err := client(srv).Check(c.typ, srv.URL, r, c.token)
		if err != nil || got != c.want {
			t.Errorf("%s: got %v, %v; want %v", c.name, got, err, c.want)
		}
		if len(f.auth) != 1 || f.auth[0] != c.wantAuth {
			t.Errorf("%s: auth header %q, want %q", c.name, f.auth, c.wantAuth)
		}
	}
	// A rejected token is reported.
	f := &fake{routes: map[string]string{"/repos/acme/macros": `401{}`}}
	srv := f.server(t)
	if _, err := client(srv).Check(GitHub, srv.URL, r, "bad"); !errors.Is(err, ErrBadToken) {
		t.Errorf("bad token: %v", err)
	}
	if got, _ := client(srv).Check(Generic, "", r, "x"); got != Unknown {
		t.Error("generic host should be unknown")
	}
}

func TestAPIBase(t *testing.T) {
	c := NewClient()
	cases := []struct{ typ, host, want string }{
		{GitHub, "github.com", "https://api.github.com"},
		{GitHub, "ghe.corp", "https://ghe.corp/api/v3"},
		{Gitea, "codeberg.org", "https://codeberg.org/api/v1"},
		{GitLab, "gitlab.com", "https://gitlab.com/api/v4"},
		{Bitbucket, "bitbucket.org", "https://api.bitbucket.org/2.0"},
		{Generic, "x", ""},
	}
	for _, tc := range cases {
		if got := c.APIBase(tc.typ, tc.host, ""); got != tc.want {
			t.Errorf("%s %s: %q, want %q", tc.typ, tc.host, got, tc.want)
		}
	}
	if got := c.APIBase(GitHub, "x", "https://api.x/"); got != "https://api.x" {
		t.Errorf("override: %q", got)
	}
}

func TestTestToken(t *testing.T) {
	f := &fake{routes: map[string]string{"/user": `{"login":"me"}`}}
	srv := f.server(t)
	if err := client(srv).TestToken(GitHub, srv.URL, "t"); err != nil {
		t.Errorf("good token: %v", err)
	}
	f.routes["/user"] = `401{}`
	if err := client(srv).TestToken(GitHub, srv.URL, "t"); !errors.Is(err, ErrBadToken) {
		t.Errorf("bad token: %v", err)
	}
}

func TestTokenLookupOrder(t *testing.T) {
	env := map[string]string{}
	var calls []string
	tk := &Tokens{
		AuthFile: filepath.Join(t.TempDir(), "auth.json"),
		Getenv:   func(k string) string { return env[k] },
		Run: func(stdin, name string, args ...string) (string, error) {
			calls = append(calls, name)
			switch name {
			case "gh":
				return "gh-token\n", nil
			case "git":
				if !strings.Contains(stdin, "host=git.example.com") {
					t.Errorf("credential input %q", stdin)
				}
				return "protocol=https\nhost=git.example.com\nusername=me\npassword=cred-token\n", nil
			}
			return "", errors.New("no")
		},
	}
	if EnvName("git.example.com") != "MM_TOKEN_GIT_EXAMPLE_COM" {
		t.Errorf("env name %s", EnvName("git.example.com"))
	}
	if got := tk.Lookup("git.example.com", Gitea, true); got != "cred-token" {
		t.Errorf("credential helper: %q", got)
	}
	if got := tk.Lookup("git.example.com", Gitea, false); got != "" {
		t.Errorf("ssh remote should skip credential helper: %q", got)
	}
	if got := tk.Lookup("github.com", GitHub, true); got != "gh-token" {
		t.Errorf("gh: %q", got)
	}
	if err := tk.Store("github.com", "stored"); err != nil {
		t.Fatal(err)
	}
	if got := tk.Lookup("github.com", GitHub, true); got != "stored" {
		t.Errorf("stored: %q", got)
	}
	if fi, _ := os.Stat(tk.AuthFile); fi.Mode().Perm() != 0o600 {
		t.Errorf("auth.json mode %v", fi.Mode().Perm())
	}
	env["MM_TOKEN_GITHUB_COM"] = "env"
	if got := tk.Lookup("github.com", GitHub, true); got != "env" {
		t.Errorf("env: %q", got)
	}
	tk.Remove("github.com")
	if v, _ := tk.Stored("github.com"); v != "" {
		t.Errorf("removed token still there: %q", v)
	}
	if err := tk.Store("h", `a"b`); err == nil {
		t.Error("token with a quote accepted")
	}
}

func TestKeychainStoreKeepsTokenOutOfArgs(t *testing.T) {
	var gotStdin string
	var gotArgs []string
	tk := &Tokens{Keychain: true, Getenv: func(string) string { return "" }, Run: func(stdin, name string, args ...string) (string, error) {
		gotStdin, gotArgs = stdin, args
		return "", nil
	}}
	tk.Store("github.com", "sekret")
	if strings.Contains(strings.Join(gotArgs, " "), "sekret") || !strings.Contains(gotStdin, `-w "sekret"`) {
		t.Errorf("args %q stdin %q", gotArgs, gotStdin)
	}
}
