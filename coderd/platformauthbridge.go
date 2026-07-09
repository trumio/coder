package coderd

import (
	"net/http"
	"net/url"
	"strings"

	"cdr.dev/slog/v3"
	"github.com/coder/coder/v2/coderd/apikey"
	"github.com/coder/coder/v2/coderd/database"
	"github.com/coder/coder/v2/coderd/database/dbauthz"
	"github.com/coder/coder/v2/coderd/httpmw"
	"github.com/coder/coder/v2/coderd/platformauth"
	"github.com/coder/coder/v2/codersdk"
)

// platformAuthBridgeMW authenticates a user opening a workspace app via the
// platform session cookie, so they land directly in the app instead of the
// login page.
//
// It must run before httpmw.PrecheckAPIKey: on success it mints a Coder session
// and injects the cookie into the current request so the downstream API-key
// validation authenticates it. It only acts on GET/HEAD requests to a workspace
// app URL that carry the platform cookie and have no existing Coder session,
// bounding session minting to roughly once per platform session. Any failure is
// a passthrough to the normal login flow; it never rejects a request.
func (api *API) platformAuthBridgeMW(next http.Handler) http.Handler {
	cfg := api.DeploymentValues.PlatformAuth
	// Zero per-request overhead when the feature is off.
	if !cfg.Enable.Value() {
		return next
	}

	var (
		client        = &http.Client{Timeout: cfg.RequestTimeout.Value()}
		cookieName    = cfg.CookieName.Value()
		validateURLs  = cfg.ValidateURLs.Value()
		envs          = cfg.Envs.Value()
		loginRedirect = cfg.LoginRedirectURL.Value()
	)

	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		// Leave non-navigations and already-authenticated requests alone. GET/HEAD
		// only avoids colliding with the CSRF middleware, which runs later and
		// would reject a freshly set session cookie on an unsafe method.
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			next.ServeHTTP(rw, r)
			return
		}
		if httpmw.APITokenFromRequest(r) != "" {
			next.ServeHTTP(rw, r)
			return
		}

		// Engage on two kinds of request:
		//   1. Workspace-app links (the direct IDE open).
		//   2. Top-level dashboard page navigations (e.g. a member landing on
		//      /workspaces), so they get a session and are then shown the
		//      access-restricted screen instead of Coder's login.
		// Sec-Fetch-Dest=document identifies a real page navigation, which
		// excludes the SPA's XHR/asset sub-requests so we don't call the backend
		// on every one of them. Everything else is left untouched so admins and
		// non-browser clients still use Coder's own auth.
		wsName, isAppURL := platformauth.WorkspaceNameFromRequest(r)
		isPageNav := r.Header.Get("Sec-Fetch-Dest") == "document"
		if !isAppURL && !isPageNav {
			next.ServeHTTP(rw, r)
			return
		}

		ctx := r.Context()
		api.Logger.Debug(ctx, "platform auth bridge: handling request",
			slog.F("workspace", wsName), slog.F("path", r.URL.Path), slog.F("app_url", isAppURL))

		// Try to bridge the platform cookie into a Coder session.
		if api.tryPlatformBridge(rw, r, client, cookieName, validateURLs, envs, wsName) {
			next.ServeHTTP(rw, r)
			return
		}

		// Could not bridge (missing/invalid platform cookie, unknown user, admin).
		// For a workspace-app link, send the user to the platform login so they
		// re-authenticate there. For a dashboard page nav, fall through to Coder's
		// own login so admins (who have no platform cookie or are not bridgeable)
		// are unaffected.
		if isAppURL && loginRedirect != "" {
			target := strings.ReplaceAll(loginRedirect, "{redirect}", url.QueryEscape(currentURL(r)))
			api.Logger.Info(ctx, "platform auth bridge: unauthenticated app request, redirecting to platform login",
				slog.F("workspace", wsName), slog.F("redirect_to", target))
			http.Redirect(rw, r, target, http.StatusSeeOther)
			return
		}
		api.Logger.Debug(ctx, "platform auth bridge: not bridged, falling through to default auth",
			slog.F("path", r.URL.Path), slog.F("app_url", isAppURL))
		next.ServeHTTP(rw, r)
	})
}

// tryPlatformBridge validates the platform cookie for a workspace-app request
// and, on success, mints a Coder session and injects it into the current
// request. It reports whether the request is now authenticated.
func (api *API) tryPlatformBridge(rw http.ResponseWriter, r *http.Request, client *http.Client, cookieName string, validateURLs, envs []string, wsName string) bool {
	ctx := r.Context()

	if len(validateURLs) == 0 {
		api.Logger.Warn(ctx, "platform auth bridge: enabled but no validate URLs configured")
		return false
	}
	cookie, err := r.Cookie(cookieName)
	if err != nil || cookie.Value == "" {
		api.Logger.Debug(ctx, "platform auth bridge: no platform cookie on request",
			slog.F("cookie_name", cookieName), slog.F("workspace", wsName))
		return false
	}

	// The workspace name suffix selects the environment when present; otherwise
	// every configured environment endpoint is tried as a fallback.
	env, envOK := platformauth.EnvFromWorkspaceName(wsName, envs)
	candidates := platformauth.CandidateURLs(validateURLs, env, envOK, envs)
	api.Logger.Debug(ctx, "platform auth bridge: validating platform cookie",
		slog.F("workspace", wsName), slog.F("env", env), slog.F("env_matched", envOK),
		slog.F("candidate_count", len(candidates)))

	// Try each backend in order; the first to accept the cookie wins.
	var email string
	for _, candidate := range candidates {
		e, err := platformauth.Validate(ctx, client, candidate, cookieName, cookie.Value)
		if err != nil {
			api.Logger.Debug(ctx, "platform auth bridge: candidate rejected cookie",
				slog.F("url", candidate), slog.Error(err))
			continue
		}
		email = e
		api.Logger.Debug(ctx, "platform auth bridge: backend validated cookie",
			slog.F("url", candidate), slog.F("email", email))
		break
	}
	if email == "" {
		api.Logger.Info(ctx, "platform auth bridge: no backend accepted the platform cookie",
			slog.F("workspace", wsName), slog.F("candidate_count", len(candidates)))
		return false
	}

	// System context: we are authenticating the user before they hold a Coder
	// session, so there is no actor to authorize the lookup/mint.
	//nolint:gocritic
	sysCtx := dbauthz.AsSystemRestricted(ctx)
	user, err := api.Database.GetUserByEmailOrUsername(sysCtx, database.GetUserByEmailOrUsernameParams{
		Email: email,
	})
	if err != nil {
		api.Logger.Info(ctx, "platform auth bridge: no Coder user matches the platform email",
			slog.F("email", email), slog.Error(err))
		return false
	}

	// Never bridge suspended users, and never bridge accounts that can reach
	// admin surfaces (admins authenticate with a password). Dormant users are
	// allowed: minting and using a session reactivates them, mirroring a normal
	// login, which is required because API-provisioned users start dormant.
	if user.Status == database.UserStatusSuspended {
		api.Logger.Info(ctx, "platform auth bridge: refusing suspended user",
			slog.F("email", email), slog.F("username", user.Username))
		return false
	}
	if !platformUserBridgeable(user.RBACRoles) {
		api.Logger.Info(ctx, "platform auth bridge: refusing non-member (admins use password login)",
			slog.F("email", email), slog.F("username", user.Username), slog.F("roles", user.RBACRoles))
		return false
	}

	//nolint:gocritic // See sysCtx rationale above.
	sessionCookie, _, err := api.createAPIKey(sysCtx, apikey.CreateParams{
		UserID:          user.ID,
		LoginType:       database.LoginTypePassword,
		DefaultLifetime: api.DeploymentValues.Sessions.DefaultDuration.Value(),
		RemoteAddr:      r.RemoteAddr,
	})
	if err != nil {
		api.Logger.Warn(ctx, "platform auth bridge: failed to mint session", slog.Error(err))
		return false
	}

	api.Logger.Info(ctx, "platform auth bridge: minted session for member",
		slog.F("email", email), slog.F("username", user.Username),
		slog.F("workspace", wsName), slog.F("status", user.Status))

	http.SetCookie(rw, sessionCookie)
	// Authenticate the current request. The un-prefixed cookie name is resolved
	// by APITokenFromRequest regardless of any __Host- prefix, which is only
	// applied to the Set-Cookie response header.
	r.AddCookie(&http.Cookie{Name: codersdk.SessionTokenCookie, Value: sessionCookie.Value})
	return true
}

// platformLoginRedirect sends the browser to the configured platform login,
// carrying the "next" query value as the post-login return target. The frontend
// dashboard guard navigates here for non-admin users so the per-environment
// platform URL stays in server config instead of the SPA bundle. When no
// platform login is configured it falls back to the Coder access page.
func (api *API) platformLoginRedirect(rw http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	target := api.DeploymentValues.PlatformAuth.LoginRedirectURL.Value()
	if target == "" {
		// No platform login configured. Render a message rather than redirect to
		// /access, which links back here and would create a click-loop.
		api.Logger.Warn(ctx, "platform login redirect requested but CODER_PLATFORM_AUTH_LOGIN_REDIRECT_URL is not set")
		rw.Header().Set("Content-Type", "text/plain; charset=utf-8")
		rw.WriteHeader(http.StatusNotFound)
		_, _ = rw.Write([]byte("Platform login is not configured. Contact your administrator."))
		return
	}
	target = strings.ReplaceAll(target, "{redirect}", url.QueryEscape(r.URL.Query().Get("next")))
	api.Logger.Debug(ctx, "platform login redirect", slog.F("redirect_to", target))
	http.Redirect(rw, r, target, http.StatusSeeOther)
}

// currentURL reconstructs the absolute URL of the incoming request so the
// platform can redirect the user back after re-authentication.
func currentURL(r *http.Request) string {
	scheme := "https"
	if r.TLS == nil {
		scheme = "http"
	}
	if fp := r.Header.Get("X-Forwarded-Proto"); fp != "" {
		scheme = fp
	}
	return scheme + "://" + r.Host + r.URL.RequestURI()
}

// platformUserBridgeable reports whether a user may be logged in via the
// platform cookie. Only plain members qualify; any elevated or custom site
// role must authenticate through the normal login flow.
func platformUserBridgeable(roles []string) bool {
	for _, role := range roles {
		if role != codersdk.RoleMember {
			return false
		}
	}
	return true
}
