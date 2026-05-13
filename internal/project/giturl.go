package project

import (
	"errors"
	"strings"
)

// ErrInvalidGitRemote signals that an input string does not parse to a
// recognisable git remote. Callers (project_add / project_set HTTP +
// MCP handlers) map this to a structured `repo_url_invalid` error.
var ErrInvalidGitRemote = errors.New("project: invalid git remote URL")

// CanonicaliseGitRemote normalises a git remote URL so the same repo
// reaches the same row regardless of how the caller wrote it. The
// canonical form is `https://<host-lowercased>/<owner>/<repo>` — no
// trailing `.git`, no trailing slash.
//
// Supported inputs (round-trip to the same canonical):
//   - SSH:    `git@github.com:owner/repo.git`
//   - SSH:    `ssh://git@github.com/owner/repo.git`
//   - HTTPS:  `https://github.com/owner/repo.git/`
//   - HTTPS:  `HTTPS://GitHub.com/owner/repo`
//   - Git:    `git://github.com/owner/repo.git`
//
// Empty input returns ("", nil) — callers decide whether empty is
// allowed (Create accepts it, project_set rejects it).
func CanonicaliseGitRemote(input string) (string, error) {
	s := strings.TrimSpace(input)
	if s == "" {
		return "", nil
	}

	// SSH shorthand: git@host:owner/repo(.git)
	if strings.HasPrefix(s, "git@") && !strings.Contains(s, "://") {
		colon := strings.Index(s, ":")
		if colon <= len("git@") {
			return "", ErrInvalidGitRemote
		}
		host := s[len("git@"):colon]
		path := s[colon+1:]
		s = "https://" + host + "/" + path
	} else {
		// Strip recognised protocol prefixes and rebuild as https://.
		switch {
		case strings.HasPrefix(s, "ssh://git@"):
			s = "https://" + s[len("ssh://git@"):]
		case strings.HasPrefix(s, "ssh://"):
			s = "https://" + s[len("ssh://"):]
		case strings.HasPrefix(s, "git://"):
			s = "https://" + s[len("git://"):]
		case strings.HasPrefix(strings.ToLower(s), "https://"):
			s = "https://" + s[len("https://"):]
		case strings.HasPrefix(strings.ToLower(s), "http://"):
			s = "https://" + s[len("http://"):]
		default:
			return "", ErrInvalidGitRemote
		}
	}

	// At this point s is `https://<host>/<path>`.
	rest := s[len("https://"):]
	slash := strings.Index(rest, "/")
	if slash <= 0 {
		return "", ErrInvalidGitRemote
	}
	host := strings.ToLower(rest[:slash])
	path := rest[slash+1:]

	path = strings.TrimRight(path, "/")
	if strings.HasSuffix(path, ".git") {
		path = path[:len(path)-len(".git")]
	}
	path = strings.TrimRight(path, "/")
	if path == "" {
		return "", ErrInvalidGitRemote
	}

	return "https://" + host + "/" + path, nil
}
