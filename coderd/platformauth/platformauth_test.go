package platformauth_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/coder/coder/v2/coderd/platformauth"
	"github.com/coder/coder/v2/testutil"
)

func TestEnvFromWorkspaceName(t *testing.T) {
	t.Parallel()

	allowed := []string{"dev", "uat", "prod"}
	cases := []struct {
		name    string
		ws      string
		wantEnv string
		wantOK  bool
	}{
		{"simple", "proj-dev", "dev", true},
		{"multi hyphen", "my-cool-project-prod", "prod", true},
		{"uat", "x-uat", "uat", true},
		{"no hyphen", "project", "", false},
		{"trailing hyphen", "project-", "", false},
		{"unknown env", "project-staging", "", false},
		{"empty", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env, ok := platformauth.EnvFromWorkspaceName(tc.ws, allowed)
			require.Equal(t, tc.wantOK, ok)
			require.Equal(t, tc.wantEnv, env)
		})
	}
}

func TestWorkspaceNameFromRequest(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		method string
		target string
		host   string
		wantWS string
		wantOK bool
	}{
		{"path with agent", http.MethodGet, "/@alice/my-ws-dev.main/apps/code-server/", "coder.example.com", "my-ws-dev", true},
		{"path without agent", http.MethodGet, "/@alice/my-ws-dev/apps/code-server/", "coder.example.com", "my-ws-dev", true},
		{"encoded at", http.MethodGet, "/%40alice/my-ws-dev.main/apps/code-server/", "coder.example.com", "my-ws-dev", true},
		{"non-app path", http.MethodGet, "/api/v2/users/me", "coder.example.com", "", false},
		{"dashboard root", http.MethodGet, "/", "coder.example.com", "", false},
		{"subdomain app", http.MethodGet, "/", "code-server--main--my-ws-dev--alice.apps.example.com", "my-ws-dev", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := httptest.NewRequest(tc.method, tc.target, nil)
			r.Host = tc.host
			ws, ok := platformauth.WorkspaceNameFromRequest(r)
			require.Equal(t, tc.wantOK, ok)
			require.Equal(t, tc.wantWS, ws)
		})
	}
}

func TestCandidateURLs(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		urls    []string
		env     string
		envOK   bool
		allEnvs []string
		want    []string
	}{
		{
			name:  "known env substitutes once",
			urls:  []string{"https://a/{env}/v"},
			env:   "dev",
			envOK: true,
			want:  []string{"https://a/dev/v"},
		},
		{
			name:    "unknown env expands across all envs in order",
			urls:    []string{"https://a/{env}/v"},
			envOK:   false,
			allEnvs: []string{"uat", "dev", "prod"},
			want:    []string{"https://a/uat/v", "https://a/dev/v", "https://a/prod/v"},
		},
		{
			name:  "non-templated urls are fallback list",
			urls:  []string{"https://uat/v", "https://dev/v"},
			envOK: false,
			want:  []string{"https://uat/v", "https://dev/v"},
		},
		{
			name:  "duplicates removed preserving order",
			urls:  []string{"https://a/{env}", "https://a/{env}"},
			env:   "dev",
			envOK: true,
			want:  []string{"https://a/dev"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.want, platformauth.CandidateURLs(tc.urls, tc.env, tc.envOK, tc.allEnvs))
		})
	}
}

func TestValidate(t *testing.T) {
	t.Parallel()

	t.Run("Success", func(t *testing.T) {
		t.Parallel()
		var gotCookie, gotPath string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotCookie = r.Header.Get("Cookie")
			gotPath = r.URL.Path
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"email":"user@example.com"}`))
		}))
		defer srv.Close()

		ctx := testutil.Context(t, testutil.WaitShort)
		email, err := platformauth.Validate(ctx, srv.Client(), srv.URL+"/dev/validate", "access_token", "tok-123")
		require.NoError(t, err)
		require.Equal(t, "user@example.com", email)
		// Only the platform cookie is forwarded.
		require.Equal(t, "access_token=tok-123", gotCookie)
		require.Equal(t, "/dev/validate", gotPath)
	})

	t.Run("SuccessDataEnvelope", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"data":{"email":"user@example.com"},"status":"SUCCESS"}`))
		}))
		defer srv.Close()

		ctx := testutil.Context(t, testutil.WaitShort)
		email, err := platformauth.Validate(ctx, srv.Client(), srv.URL, "access_token", "tok")
		require.NoError(t, err)
		require.Equal(t, "user@example.com", email)
	})

	t.Run("Unauthorized", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
		}))
		defer srv.Close()

		ctx := testutil.Context(t, testutil.WaitShort)
		_, err := platformauth.Validate(ctx, srv.Client(), srv.URL, "access_token", "bad")
		require.ErrorIs(t, err, platformauth.ErrInvalidToken)
	})

	t.Run("ServerError", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer srv.Close()

		ctx := testutil.Context(t, testutil.WaitShort)
		_, err := platformauth.Validate(ctx, srv.Client(), srv.URL, "access_token", "tok")
		require.Error(t, err)
		require.NotErrorIs(t, err, platformauth.ErrInvalidToken)
	})

	t.Run("MissingEmail", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		}))
		defer srv.Close()

		ctx := testutil.Context(t, testutil.WaitShort)
		_, err := platformauth.Validate(ctx, srv.Client(), srv.URL, "access_token", "tok")
		require.Error(t, err)
	})

	t.Run("Timeout", func(t *testing.T) {
		t.Parallel()
		blocked := make(chan struct{})
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			<-blocked
		}))
		defer srv.Close()
		defer close(blocked)

		client := &http.Client{Timeout: time.Millisecond}
		_, err := platformauth.Validate(context.Background(), client, srv.URL, "access_token", "tok")
		require.Error(t, err)
	})
}
