#!/usr/bin/env bash
# platform-auth-local-test.sh
#
# Spins up a local, patched Coder server wired to TWO dummy validation
# endpoints (a fallback list) to test the platform-cookie auth bridge, and
# optionally a real Docker workspace so you can open code-server directly by
# presenting the platform cookie.
#
#   ./scripts/platform-auth-local-test.sh up     # build + start everything
#   ./scripts/platform-auth-local-test.sh down    # stop everything
#   ./scripts/platform-auth-local-test.sh curl    # re-run the headless bridge check
#
# Requirements: Go (to build), python3. Docker is OPTIONAL: without it the
# script still proves the auth bridge via curl, but cannot build a real
# workspace for you to open in a browser.
set -euo pipefail

# ------------------------------------------------------------------ config ---
REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORK="${WORK:-${TMPDIR:-/tmp}/coder-platform-test}"
CODER_BIN="${CODER_BIN:-$WORK/coder}"
ACCESS_URL="${ACCESS_URL:-http://127.0.0.1:3000}"
PORT_A=9998            # dummy endpoint A: always rejects (401)
PORT_B=9999            # dummy endpoint B: accepts, returns member email
ADMIN_EMAIL="admin@coder.com"
ADMIN_PASS="SecurePass123!"
MEMBER_EMAIL="member@example.com"
MEMBER_USER="member"
WS_NAME="${WS_NAME:-proj-dev}"          # NB: the "-dev" suffix drives env selection
COOKIE_NAME="access_token"
# Template used to build the member's workspace. Defaults to the Docker port of
# the production AKS template (same coder_app "code-server", share = "owner").
# Override TEMPLATE_DIR to test with a different template.
TEMPLATE_DIR="${TEMPLATE_DIR:-$REPO/scripts/platform-auth-testdata/docker-workspace}"
TEMPLATE_NAME="platform-test"
# Where a member with no valid platform session is sent (instead of Coder's
# login page). {redirect} is replaced with the original app URL.
PLATFORM_LOGIN_URL="${PLATFORM_LOGIN_URL:-https://app.trumio.ai/login?next={redirect}}"
PIDS="$WORK/pids"

mkdir -p "$WORK"
jqget() { python3 -c "import sys,json;print(json.load(sys.stdin).get('$1',''))"; }
say() { printf '\n\033[1;36m==> %s\033[0m\n' "$*"; }

# ------------------------------------------------------------------- down ---
down() {
  say "Stopping everything"
  [ -f "$PIDS" ] && while read -r p; do kill "$p" 2>/dev/null || true; done < "$PIDS"
  pkill -f "coder-platform-test/coder server" 2>/dev/null || true
  pkill -f "platform_auth_dummy" 2>/dev/null || true
  [ -d "$WORK/pgdata" ] && pg_ctl -D "$WORK/pgdata" stop >/dev/null 2>&1 || true
  rm -f "$PIDS"
  echo "stopped."
}

# --------------------------------------------------------------- dummies ---
start_dummies() {
  say "Starting two dummy validation endpoints (A rejects, B accepts)"
  ACCEPT_EMAIL="$MEMBER_EMAIL" PORT_A="$PORT_A" PORT_B="$PORT_B" python3 - <<'PY' > "$WORK/dummy.log" 2>&1 &
# platform_auth_dummy
import http.server, json, threading, time, os
email = os.environ["ACCEPT_EMAIL"]
pa, pb = int(os.environ["PORT_A"]), int(os.environ["PORT_B"])
def make(name, status, em=None):
    class H(http.server.BaseHTTPRequestHandler):
        def do_GET(self):
            print(f"HIT endpoint={name} path={self.path} cookie={self.headers.get('Cookie','')}", flush=True)
            if status == 200:
                self.send_response(200); self.send_header("Content-Type","application/json"); self.end_headers()
                # Mirror the real backend envelope exactly: {"status":"SUCCESS","data":{"email":...}}.
                # Coder reads .data.email, so this is a faithful test of the parser.
                self.wfile.write(json.dumps({"status": "SUCCESS", "data": {"email": em}}).encode())
            else:
                self.send_response(status); self.end_headers()
        def log_message(self, *a): pass
    return H
threading.Thread(target=http.server.HTTPServer(("127.0.0.1", pa), make("A-reject", 401)).serve_forever, daemon=True).start()
threading.Thread(target=http.server.HTTPServer(("127.0.0.1", pb), make("B-accept", 200, email)).serve_forever, daemon=True).start()
print(f"dummies up: A(reject)=:{pa}  B(accept, {email})=:{pb}", flush=True)
while True: time.sleep(3600)
PY
  echo $! >> "$PIDS"
  sleep 1
}

# --------------------------------------------------------------- postgres ---
# Prefer Coder's built-in Postgres. If that binary is the wrong arch (some
# environments ship a stale cache), set CODER_PG_CONNECTION_URL to an external
# Postgres and this block is skipped.
start_postgres() {
  if [ -n "${CODER_PG_CONNECTION_URL:-}" ]; then
    say "Using external Postgres from CODER_PG_CONNECTION_URL"; return
  fi
  if command -v initdb >/dev/null 2>&1; then
    say "Starting a native Postgres (127.0.0.1:5433)"
    if [ ! -d "$WORK/pgdata" ]; then
      initdb -U coder -A trust -D "$WORK/pgdata" --encoding=UTF8 >/dev/null
    fi
    pg_ctl -D "$WORK/pgdata" -o "-p 5433 -k /tmp -c listen_addresses=127.0.0.1" -l "$WORK/pg.log" start >/dev/null
    sleep 2
    createdb -h 127.0.0.1 -p 5433 -U coder coder 2>/dev/null || true
    export CODER_PG_CONNECTION_URL="postgres://coder@127.0.0.1:5433/coder?sslmode=disable"
  else
    say "No native Postgres found; relying on Coder's built-in Postgres"
  fi
}

# ------------------------------------------------------------------ server ---
build_server() {
  if [ "${BUILD_UI:-0}" = "1" ]; then
    if [ ! -f "$REPO/site/out/index.html" ] || [ "${FORCE_SPA:-0}" = "1" ]; then
      say "Building the web UI (pnpm --dir site build) -- slow first time"
      ( cd "$REPO" && pnpm --dir site build )
    else
      echo "web UI already built ($REPO/site/out); set FORCE_SPA=1 to rebuild"
    fi
    # The workspace agent downloads itself from the server at
    # /bin/coder-linux-<arch>. A plain build does not populate site/out/bin, so
    # cross-compile the agent for the Docker daemon's architecture, otherwise
    # the workspace hangs retrying the download and code-server never starts.
    local darch
    darch=$(docker version --format '{{.Server.Arch}}' 2>/dev/null || echo "")
    darch="${darch:-arm64}"
    if [ ! -f "$REPO/site/out/bin/coder-linux-$darch" ] || [ "${FORCE_AGENT:-0}" = "1" ]; then
      say "Cross-compiling the workspace agent (linux/$darch) into site/out/bin"
      ( cd "$REPO" && GOOS=linux GOARCH="$darch" go build -tags slim -o "site/out/bin/coder-linux-$darch" ./cmd/coder )
    else
      echo "agent binary already built (site/out/bin/coder-linux-$darch)"
    fi
    say "Building patched Coder WITH embedded UI (-tags embed) -> $CODER_BIN"
    ( cd "$REPO" && go build -tags embed -o "$CODER_BIN" ./cmd/coder )
  else
    say "Building patched Coder binary (API only) -> $CODER_BIN"
    ( cd "$REPO" && go build -o "$CODER_BIN" ./cmd/coder )
  fi
}

start_server() {
  say "Starting patched Coder server with the fallback endpoint list"
  CODER_CONFIG_DIR="$WORK/coderv2" \
  CODER_OAUTH2_GITHUB_DEFAULT_PROVIDER_ENABLE=false \
  CODER_PLATFORM_AUTH_ENABLE=true \
  CODER_PLATFORM_AUTH_COOKIE_NAME="$COOKIE_NAME" \
  CODER_PLATFORM_AUTH_VALIDATE_URLS="http://127.0.0.1:$PORT_A/validate,http://127.0.0.1:$PORT_B/validate" \
  CODER_PLATFORM_AUTH_ENVS="dev,uat,prod" \
  CODER_PLATFORM_AUTH_LOGIN_REDIRECT_URL="$PLATFORM_LOGIN_URL" \
    nohup "$CODER_BIN" server --access-url "$ACCESS_URL" --http-address 127.0.0.1:3000 \
    > "$WORK/coder.log" 2>&1 &
  echo $! >> "$PIDS"
  for i in $(seq 1 90); do
    [ "$(curl -s -o /dev/null -w '%{http_code}' "$ACCESS_URL/healthz" 2>/dev/null)" = "200" ] && { echo "server ready (${i}s)"; return; }
    sleep 1
  done
  echo "server did not become ready; see $WORK/coder.log"; tail -20 "$WORK/coder.log"; exit 1
}

# --------------------------------------------------------------- provision ---
seed_users() {
  say "Creating admin + member (member@example.com)"
  local first org
  first=$(curl -s -X POST "$ACCESS_URL/api/v2/users/first" -H 'Content-Type: application/json' \
    -d "{\"email\":\"$ADMIN_EMAIL\",\"username\":\"admin\",\"password\":\"$ADMIN_PASS\",\"name\":\"Admin\",\"trial\":false}")
  org=$(echo "$first" | jqget organization_id)
  ADMIN_TOKEN=$(curl -s -X POST "$ACCESS_URL/api/v2/users/login" -H 'Content-Type: application/json' \
    -d "{\"email\":\"$ADMIN_EMAIL\",\"password\":\"$ADMIN_PASS\"}" | jqget session_token)
  curl -s -X POST "$ACCESS_URL/api/v2/users" -H "Coder-Session-Token: $ADMIN_TOKEN" -H 'Content-Type: application/json' \
    -d "{\"email\":\"$MEMBER_EMAIL\",\"username\":\"$MEMBER_USER\",\"login_type\":\"none\",\"organization_ids\":[\"$org\"]}" >/dev/null
  # Mint a member session token (admin acting on the member) so we can create a
  # member-OWNED workspace, mirroring how your platform provisions workspaces.
  MEMBER_TOKEN=$(curl -s -X POST "$ACCESS_URL/api/v2/users/$MEMBER_USER/keys/tokens" \
    -H "Coder-Session-Token: $ADMIN_TOKEN" -H 'Content-Type: application/json' -d '{}' | jqget key)
  ORG_ID="$org"
  echo "admin + member ready."
}

build_workspace() {
  if ! docker info >/dev/null 2>&1; then
    say "Docker is NOT running -> skipping real workspace build"
    echo "The auth bridge is still fully testable via 'curl' below; you just"
    echo "won't have a live code-server to open in a browser."
    return 1
  fi
  say "Pushing template ($TEMPLATE_DIR) and creating a MEMBER-owned workspace via the admin API"
  CODER_URL="$ACCESS_URL" CODER_SESSION_TOKEN="$ADMIN_TOKEN" \
    "$CODER_BIN" templates push "$TEMPLATE_NAME" -d "$TEMPLATE_DIR" -y >/dev/null
  local tid
  tid=$(curl -s -H "Coder-Session-Token: $ADMIN_TOKEN" \
    "$ACCESS_URL/api/v2/organizations/$ORG_ID/templates/$TEMPLATE_NAME" | jqget id)
  # Admin creates the workspace ON BEHALF OF the member -- exactly how your
  # platform provisions workspaces with an admin token.
  curl -s -X POST "$ACCESS_URL/api/v2/users/$MEMBER_USER/workspaces" \
    -H "Coder-Session-Token: $ADMIN_TOKEN" -H 'Content-Type: application/json' \
    -d "{\"template_id\":\"$tid\",\"name\":\"$WS_NAME\"}" \
    | python3 -c "import sys,json;d=json.load(sys.stdin);print('created workspace:',d.get('name'),'owner=',d.get('owner_name'))"
  echo "waiting for the agent to connect..."
  for i in $(seq 1 80); do
    st=$(curl -s -H "Coder-Session-Token: $ADMIN_TOKEN" \
      "$ACCESS_URL/api/v2/users/$MEMBER_USER/workspace/$WS_NAME" \
      | python3 -c "import sys,json;print(json.load(sys.stdin).get('latest_build',{}).get('status',''))" 2>/dev/null)
    [ "$st" = "running" ] && { echo "workspace running"; return 0; }
    sleep 3
  done
  echo "workspace not running yet; check: CODER_SESSION_TOKEN=$MEMBER_TOKEN $CODER_BIN list"; return 0
}

rbac_check() {
  say "Data-layer access: MEMBER is row-scoped, ADMIN sees everything"
  code() { curl -s -o /dev/null -w "%{http_code}" -H "Coder-Session-Token: $2" "$ACCESS_URL$1"; }
  cnt() { curl -s -H "Coder-Session-Token: $2" "$ACCESS_URL$1" | jqget count; }
  echo "  MEMBER  /deployment/config -> $(code /api/v2/deployment/config "$MEMBER_TOKEN")   (403 = blocked)"
  echo "  MEMBER  /users  (visible)  -> $(cnt /api/v2/users "$MEMBER_TOKEN")   (only self)"
  echo "  MEMBER  /workspaces        -> $(cnt /api/v2/workspaces "$MEMBER_TOKEN")   (only own)"
  echo "  ADMIN   /deployment/config -> $(code /api/v2/deployment/config "$ADMIN_TOKEN")   (200)"
  echo "  ADMIN   /users  (visible)  -> $(cnt /api/v2/users "$ADMIN_TOKEN")   (all)"
  echo "  ADMIN   /workspaces        -> $(cnt /api/v2/workspaces "$ADMIN_TOKEN")   (all)"
}

curl_check() {
  say "Headless bridge check (fallback: endpoint A rejected, B accepted)"
  : > "$WORK/dummy.log"; sleep 0.3
  local hdrs
  hdrs=$(curl -s -D - -o /dev/null --cookie "$COOKIE_NAME=platform-token-xyz" \
    "$ACCESS_URL/@$MEMBER_USER/$WS_NAME.main/apps/code-server/")
  echo "$hdrs" | grep -iE "^HTTP/|^set-cookie|^location" || true
  echo "--- dummy endpoint hits (proves fallback order) ---"
  grep -a HIT "$WORK/dummy.log" || echo "(no hits logged)"
  local tok
  tok=$(echo "$hdrs" | sed -n "s/.*coder_session_token=\([^;]*\).*/\1/p" | head -1)
  if [ -n "$tok" ]; then
    echo "--- minted session identity ---"
    curl -s -H "Coder-Session-Token: $tok" "$ACCESS_URL/api/v2/users/me" \
      | python3 -c "import sys,json;d=json.load(sys.stdin);print('  authenticated as:',d.get('username'),d.get('email'),'status=',d.get('status'))"
    echo "  => bridge authenticated the request with NO login page."
  else
    echo "  => no session minted (unexpected)."
  fi
}

print_browser_help() {
  if [ "${BUILD_UI:-0}" != "1" ]; then
    cat <<EOF

This 'up' run serves the API only (no dashboard). The auth + RBAC checks above
already prove the bridge. To eyeball the dashboard pages in a browser, run:
    ./scripts/platform-auth-local-test.sh ui
which serves the full web UI on $ACCESS_URL.
EOF
    return
  fi
  say "Browser checks -- open these in a real browser (dashboard is on $ACCESS_URL)"
  cat <<EOF
IMPORTANT (local testing only): a cookie is scoped to ONE host. "localhost" and
"127.0.0.1" are DIFFERENT origins, so set the cookie and open the app URL on the
SAME host. These steps use $ACCESS_URL throughout. (In prod this is a non-issue:
your platform sets the cookie on the shared parent domain, e.g. .trumio.ai.)

CHECK 1 - member opens the IDE directly via the cookie:
  a. Open $ACCESS_URL/login  (Coder's login page loads without redirecting; you
     cannot set the cookie on the app URL because it redirects you away first).
  b. In the DevTools console ON THAT PAGE, set the cookie:
       document.cookie = "$COOKIE_NAME=platform-token-xyz; path=/";
  c. Navigate to:  $ACCESS_URL/@$MEMBER_USER/$WS_NAME.main/apps/code-server/
     Expect: lands straight in code-server -- no login page, no GitHub.

CHECK 2 - member cannot reach any Coder page:
  Still holding the member cookie (no admin login), visit $ACCESS_URL/workspaces
  (or /templates, /deployment, /settings). Expect: redirected to the PLATFORM
  login ($PLATFORM_LOGIN_URL) via /platform-login. A non-admin never sees any
  Coder dashboard page. (Locally the platform URL may 502; that just proves the
  redirect fired. In prod it is your real platform login.)

CHECK 3 - admin sees all pages + workspaces:
  Open $ACCESS_URL with NO platform cookie, log in with the password form
  ($ADMIN_EMAIL / $ADMIN_PASS; GitHub button is gone). Expect: full dashboard --
  Workspaces (all), Templates, and the admin menu (Deployment, Users, etc.).

CHECK 4 - member with NO/expired platform session goes to the PLATFORM login:
  In incognito (no cookie set), open the app URL:
    $ACCESS_URL/@$MEMBER_USER/$WS_NAME.main/apps/code-server/
  Expect: redirected to $PLATFORM_LOGIN_URL (your platform's login), NOT Coder's
  login page. The member re-authenticates on the platform and comes back.

Logs:   server=$WORK/coder.log   dummies=$WORK/dummy.log
Stop:   ./scripts/platform-auth-local-test.sh down
EOF
}

# -------------------------------------------------------------------- main ---
case "${1:-up}" in
  up)
    : > "$PIDS"
    build_server
    start_dummies
    start_postgres
    start_server
    seed_users
    build_workspace || true
    curl_check
    rbac_check
    print_browser_help
    ;;
  ui)
    # Full setup with the embedded dashboard on one port, for BROWSER testing.
    # Bypasses ./scripts/develop.sh (which needs GNU tools + a working embedded
    # Postgres). Uses a separate binary so it does not clobber the API-only one.
    BUILD_UI=1
    CODER_BIN="$WORK/coder-ui"
    : > "$PIDS"
    build_server
    start_dummies
    start_postgres
    start_server
    seed_users
    build_workspace || true
    curl_check
    rbac_check
    print_browser_help
    ;;
  curl)  curl_check ;;
  down)  down ;;
  *) echo "usage: $0 {up|ui|curl|down}   (up=headless, ui=with dashboard for browser)"; exit 1 ;;
esac
