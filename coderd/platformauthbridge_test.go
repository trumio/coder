package coderd_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/coder/coder/v2/coderd/coderdtest"
	"github.com/coder/coder/v2/codersdk"
	"github.com/coder/coder/v2/testutil"
	"github.com/coder/serpent"
)

// setupBridge starts a Coder deployment with the platform-auth bridge configured
// against a stub backend. The returned setEmail controls which email the stub
// returns; an empty email makes the stub reject the cookie with 401.
func setupBridge(t *testing.T, enable bool) (*codersdk.Client, func(string)) {
	t.Helper()

	var (
		mu    sync.Mutex
		email string
	)
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		e := email
		mu.Unlock()
		if e == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"email": e})
	}))
	t.Cleanup(stub.Close)

	dv := coderdtest.DeploymentValues(t)
	dv.PlatformAuth.Enable = serpent.Bool(enable)
	dv.PlatformAuth.CookieName = "access_token"
	dv.PlatformAuth.ValidateURLs = serpent.StringArray{stub.URL + "/{env}/validate"}
	dv.PlatformAuth.Envs = serpent.StringArray{"dev"}
	dv.PlatformAuth.RequestTimeout = serpent.Duration(testutil.WaitShort)

	client := coderdtest.New(t, &coderdtest.Options{DeploymentValues: dv})
	return client, func(e string) {
		mu.Lock()
		defer mu.Unlock()
		email = e
	}
}

// openApp performs an unauthenticated GET to a workspace app URL carrying only
// the platform cookie, and reports whether a Coder session cookie was minted.
func mintedSession(t *testing.T, client *codersdk.Client, username, workspace string) bool {
	t.Helper()
	ctx := testutil.Context(t, testutil.WaitLong)

	reqURL := fmt.Sprintf("%s/@%s/%s.main/apps/code-server/", client.URL.String(), username, workspace)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	require.NoError(t, err)
	req.AddCookie(&http.Cookie{Name: "access_token", Value: "platform-token"})

	hc := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := hc.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	for _, c := range resp.Cookies() {
		if c.Name == codersdk.SessionTokenCookie && c.Value != "" {
			return true
		}
	}
	return false
}

func TestPlatformAuthBridge(t *testing.T) {
	t.Parallel()

	t.Run("MemberBridged", func(t *testing.T) {
		t.Parallel()
		client, setEmail := setupBridge(t, true)
		owner := coderdtest.CreateFirstUser(t, client)
		_, member := coderdtest.CreateAnotherUser(t, client, owner.OrganizationID)
		setEmail(member.Email)

		require.True(t, mintedSession(t, client, member.Username, "proj-dev"))
	})

	t.Run("AdminNotBridged", func(t *testing.T) {
		t.Parallel()
		client, setEmail := setupBridge(t, true)
		coderdtest.CreateFirstUser(t, client)

		ctx := testutil.Context(t, testutil.WaitLong)
		ownerUser, err := client.User(ctx, codersdk.Me)
		require.NoError(t, err)
		setEmail(ownerUser.Email)

		require.False(t, mintedSession(t, client, ownerUser.Username, "proj-dev"))
	})

	t.Run("Disabled", func(t *testing.T) {
		t.Parallel()
		client, setEmail := setupBridge(t, false)
		owner := coderdtest.CreateFirstUser(t, client)
		_, member := coderdtest.CreateAnotherUser(t, client, owner.OrganizationID)
		setEmail(member.Email)

		require.False(t, mintedSession(t, client, member.Username, "proj-dev"))
	})

	t.Run("SuspendedNotBridged", func(t *testing.T) {
		t.Parallel()
		client, setEmail := setupBridge(t, true)
		owner := coderdtest.CreateFirstUser(t, client)
		_, member := coderdtest.CreateAnotherUser(t, client, owner.OrganizationID)
		setEmail(member.Email)

		ctx := testutil.Context(t, testutil.WaitLong)
		_, err := client.UpdateUserStatus(ctx, member.ID.String(), codersdk.UserStatusSuspended)
		require.NoError(t, err)

		require.False(t, mintedSession(t, client, member.Username, "proj-dev"))
	})

	t.Run("FallbackAcrossEndpoints", func(t *testing.T) {
		t.Parallel()

		// First endpoint always rejects; the second returns the member email.
		reject := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
		}))
		t.Cleanup(reject.Close)

		var (
			mu    sync.Mutex
			email string
		)
		accept := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			mu.Lock()
			e := email
			mu.Unlock()
			if e == "" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]string{"email": e})
		}))
		t.Cleanup(accept.Close)

		dv := coderdtest.DeploymentValues(t)
		dv.PlatformAuth.Enable = serpent.Bool(true)
		dv.PlatformAuth.CookieName = "access_token"
		dv.PlatformAuth.ValidateURLs = serpent.StringArray{reject.URL + "/validate", accept.URL + "/validate"}
		dv.PlatformAuth.Envs = serpent.StringArray{"dev"}
		dv.PlatformAuth.RequestTimeout = serpent.Duration(testutil.WaitShort)

		client := coderdtest.New(t, &coderdtest.Options{DeploymentValues: dv})
		owner := coderdtest.CreateFirstUser(t, client)
		_, member := coderdtest.CreateAnotherUser(t, client, owner.OrganizationID)
		mu.Lock()
		email = member.Email
		mu.Unlock()

		// The first endpoint rejects, so the bridge falls back to the second.
		require.True(t, mintedSession(t, client, member.Username, "proj-dev"))
	})
}
