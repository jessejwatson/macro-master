package gitx

import "testing"

func TestPermissionRejection(t *testing.T) {
	cases := map[string]string{
		"remote: error: GH006: Protected branch update failed for refs/heads/main.":   "protected branch",
		"! [remote rejected] main -> main (pre-receive hook declined)":                "pre-receive hook declined",
		"ERROR: Permission to acme/macros.git denied to someone.":                     "no write permission",
		"fatal: unable to access 'https://x/': The requested URL returned error: 403": "no write permission",
		"remote: You are not allowed to push code to this project.":                   "no write permission",
	}
	for out, want := range cases {
		if got, ok := PermissionRejection(out); !ok || got != want {
			t.Errorf("%q: got %q %v, want %q", out, got, ok, want)
		}
	}
	for _, out := range []string{
		"git@github.com: Permission denied (publickey).",
		"! [rejected] main -> main (fetch first)",
		"fatal: Could not read from remote repository.",
	} {
		if r, ok := PermissionRejection(out); ok {
			t.Errorf("%q wrongly read as a permission rejection (%s)", out, r)
		}
	}
}
