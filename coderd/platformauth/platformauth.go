// Package platformauth contains helpers for the platform-cookie authentication
// bridge: deriving the environment from a workspace name, extracting the
// workspace name from an incoming request, and validating a platform session
// cookie against the external backend.
//
// Coder performs no token verification of its own. The backend is the sole
// authority: it is handed the raw platform cookie and, on success, returns the
// owning user's email.
package platformauth

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strings"

	"golang.org/x/xerrors"

	"github.com/coder/coder/v2/coderd/workspaceapps/appurl"
)

// ErrInvalidToken is returned by Validate when the backend rejects the cookie
// (HTTP 401/403). The caller treats it the same as any other failure: fall
// through to the normal login flow.
var ErrInvalidToken = xerrors.New("platform token rejected by backend")

// maxResponseBytes bounds how much of the backend response we read to guard
// against a misbehaving or hostile endpoint.
const maxResponseBytes = 1 << 16

// pathAppRe matches the workspace name in a path-based app request:
// /@{user}/{workspace}[.{agent}]/apps/{slug}. The "@" may be percent-encoded.
// Workspace and user names are alphanumeric with single hyphens, so the
// workspace capture stops at the first "." (the agent separator) or "/".
var pathAppRe = regexp.MustCompile(`^/(?:@|%40)[^/]+/([^/.]+)(?:\.[^/]+)?/apps(?:/|$)`)

// EnvFromWorkspaceName returns the environment encoded as the final "-"
// delimited segment of the workspace name (e.g. "my-project-dev" -> "dev"). It
// returns ok=false when the suffix is not one of the allowed environments.
func EnvFromWorkspaceName(name string, allowed []string) (string, bool) {
	idx := strings.LastIndex(name, "-")
	if idx < 0 || idx == len(name)-1 {
		return "", false
	}
	env := name[idx+1:]
	if !slices.Contains(allowed, env) {
		return "", false
	}
	return env, true
}

// WorkspaceNameFromRequest extracts the workspace name from a workspace-app
// request, handling both path-based apps and subdomain-based apps. It returns
// ok=false when the request is not an app request we can parse.
func WorkspaceNameFromRequest(r *http.Request) (string, bool) {
	if m := pathAppRe.FindStringSubmatch(r.URL.Path); m != nil {
		return m[1], true
	}

	// Subdomain apps: the first DNS label encodes
	// {slug}--{agent}--{workspace}--{user}. ParseSubdomainAppURL rejects any
	// label that does not match, so a bare dashboard host is safely ignored.
	host := r.Host
	if host == "" {
		host = r.URL.Host
	}
	label, _, found := strings.Cut(host, ".")
	if !found || label == "" {
		return "", false
	}
	app, err := appurl.ParseSubdomainAppURL(label)
	if err != nil || app.WorkspaceName == "" {
		return "", false
	}
	return app.WorkspaceName, true
}

// CandidateURLs returns the ordered list of validation endpoints to try for a
// request. Each configured URL may contain a "{env}" placeholder. When the
// environment is known it is substituted directly; when it is unknown, the URL
// is expanded once per configured environment (in order) so the caller can
// fall back across environments. URLs without a placeholder are used as-is.
// Duplicates are removed while preserving order.
func CandidateURLs(urls []string, env string, envOK bool, allEnvs []string) []string {
	var out []string
	seen := make(map[string]struct{})
	add := func(u string) {
		if _, ok := seen[u]; ok {
			return
		}
		seen[u] = struct{}{}
		out = append(out, u)
	}
	for _, u := range urls {
		if !strings.Contains(u, "{env}") {
			add(u)
			continue
		}
		if envOK {
			add(strings.ReplaceAll(u, "{env}", env))
			continue
		}
		for _, e := range allEnvs {
			add(strings.ReplaceAll(u, "{env}", e))
		}
	}
	return out
}

// Validate asks the backend at the given (already-resolved) URL to validate the
// platform cookie. On success it returns the email of the user the cookie
// belongs to.
func Validate(ctx context.Context, client *http.Client, url, cookieName, token string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", xerrors.Errorf("build validate request: %w", err)
	}
	// Forward only the platform cookie, never Coder's own session cookie.
	req.AddCookie(&http.Cookie{Name: cookieName, Value: token})

	resp, err := client.Do(req)
	if err != nil {
		return "", xerrors.Errorf("call backend: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized, http.StatusForbidden:
		return "", ErrInvalidToken
	default:
		return "", xerrors.Errorf("backend returned status %d", resp.StatusCode)
	}

	// The backend wraps its payload as {"data": {"email": ...}, "status": ...}.
	// A top-level "email" is also accepted for flexibility.
	var body struct {
		Email string `json:"email"`
		Data  struct {
			Email string `json:"email"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(&body); err != nil {
		return "", xerrors.Errorf("decode backend response: %w", err)
	}
	email := body.Data.Email
	if email == "" {
		email = body.Email
	}
	if email == "" {
		return "", xerrors.New("backend response missing email")
	}
	return email, nil
}
