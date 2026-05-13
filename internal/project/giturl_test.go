package project

import "testing"

// TestCanonicaliseGitRemote_RoundTrip pins the canonicaliser shape so
// project_add + project_set (sty_4db7c3a3) cannot drift — both use
// this function as their shared normaliser.
func TestCanonicaliseGitRemote_RoundTrip(t *testing.T) {
	t.Parallel()
	want := "https://github.com/owner/repo"
	cases := []string{
		"git@github.com:owner/repo.git",
		"git@github.com:owner/repo",
		"ssh://git@github.com/owner/repo.git",
		"ssh://git@github.com/owner/repo",
		"https://github.com/owner/repo.git",
		"https://github.com/owner/repo.git/",
		"https://github.com/owner/repo/",
		"https://github.com/owner/repo",
		"HTTPS://GitHub.com/owner/repo",
		"git://github.com/owner/repo.git",
		"  https://github.com/owner/repo  ",
	}
	for _, in := range cases {
		got, err := CanonicaliseGitRemote(in)
		if err != nil {
			t.Errorf("CanonicaliseGitRemote(%q) error = %v, want nil", in, err)
			continue
		}
		if got != want {
			t.Errorf("CanonicaliseGitRemote(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCanonicaliseGitRemote_Empty(t *testing.T) {
	t.Parallel()
	got, err := CanonicaliseGitRemote("")
	if err != nil {
		t.Errorf("empty input err = %v, want nil", err)
	}
	if got != "" {
		t.Errorf("empty input out = %q, want \"\"", got)
	}
}

func TestCanonicaliseGitRemote_Invalid(t *testing.T) {
	t.Parallel()
	cases := []string{
		"not a url",
		"://broken",
		"git@",
		"git@:owner/repo",
		"https:///owner/repo",
		"ftp://github.com/owner/repo",
		"https://github.com/",
	}
	for _, in := range cases {
		if _, err := CanonicaliseGitRemote(in); err == nil {
			t.Errorf("CanonicaliseGitRemote(%q) err = nil, want error", in)
		}
	}
}

// TestResolveMCPURL covers the resolver precedence: persisted MCPURL
// wins; otherwise derive from publicBaseURL + project id; otherwise
// empty.
func TestResolveMCPURL(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		project Project
		base    string
		want    string
	}{
		{"persisted_wins", Project{ID: "proj_aaa", MCPURL: "https://custom/mcp"}, "https://other.example", "https://custom/mcp"},
		{"derived_from_base", Project{ID: "proj_aaa"}, "https://satellites.example", "https://satellites.example/mcp?project_id=proj_aaa"},
		{"derived_strips_trailing_slash", Project{ID: "proj_bbb"}, "https://satellites.example/", "https://satellites.example/mcp?project_id=proj_bbb"},
		{"derived_trims_whitespace", Project{ID: "proj_ccc"}, "  https://satellites.example  ", "https://satellites.example/mcp?project_id=proj_ccc"},
		{"empty_base_returns_empty", Project{ID: "proj_ddd"}, "", ""},
		{"empty_id_returns_empty", Project{}, "https://satellites.example", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ResolveMCPURL(tc.project, tc.base)
			if got != tc.want {
				t.Errorf("ResolveMCPURL(%+v, %q) = %q, want %q", tc.project, tc.base, got, tc.want)
			}
		})
	}
}
