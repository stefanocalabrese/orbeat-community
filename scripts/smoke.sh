#!/usr/bin/env bash
set -euo pipefail

COMPOSE="docker compose -f deploy/docker-compose.yml"

# Revision pruning cap (docs/specs/2026-08-19-orbeat-revision-pruning-design.md,
# fable-audit §7 #17). Exported BEFORE the stack comes up (the `$COMPOSE up`
# call below) so it flows through the compose file's
# ${ORBEAT_ARTIFACT_REVISION_KEEP:-} passthrough (deploy/docker-compose.yml,
# api service) into cmd/api. `make up` and the Playwright e2e job use the same
# compose file WITHOUT this export, so they stay unaffected (unset resolves to
# "no pruning", the default). 3, not 1 or 2: those two are degenerate per the
# spec (1 leaves no rollback target at all; 2 leaves exactly one usable step)
# and would trigger cmd/api's startup warning for a reason unrelated to this
# gate.
export ORBEAT_ARTIFACT_REVISION_KEEP=3

cleanup() {
  rc=$?
  # Dump container logs BEFORE tearing down — otherwise a failure (especially in
  # CI) leaves nothing to debug with, since the teardown below removes them.
  if [ "$rc" != "0" ]; then
    echo "==> smoke FAILED (exit $rc) — recent stack logs:"
    $COMPOSE logs --tail=100 2>&1 | sed 's/^/    | /' || true
  fi
  [ -n "${MP_TMP:-}" ] && rm -rf "$MP_TMP"
  [ -n "${SYNC_HOME:-}" ] && rm -rf "$SYNC_HOME"
  [ -n "${SYNC_PROJ:-}" ] && rm -rf "$SYNC_PROJ"
  [ -n "${SYNC_BIN:-}" ] && rm -rf "$SYNC_BIN"
  [ -n "${SYNC_BAD:-}" ] && rm -rf "$SYNC_BAD"
  [ -n "${SYNC_FATAL_A:-}" ] && rm -rf "$SYNC_FATAL_A"
  [ -n "${SYNC_FATAL_B:-}" ] && rm -rf "$SYNC_FATAL_B"
  [ -n "${SYNC_FRESH_A:-}" ] && rm -rf "$SYNC_FRESH_A"
  [ -n "${SYNC_FRESH_B:-}" ] && rm -rf "$SYNC_FRESH_B"
  # Must stay LAST in cleanup(): its trailing `|| true` is what stops a failing
  # guard above from overriding the script's real exit status under errexit.
  # Appending below it turns a GREEN run red whenever the appended guard's
  # variable is unset — measured: body exits 0, trap ends in a failing guard,
  # process exits 1.
  $COMPOSE down -v >/dev/null 2>&1 || true
}
trap cleanup EXIT

# rfc3339_in_minutes <n> — an RFC3339 UTC timestamp n minutes from now. GNU date
# (CI, ubuntu-latest) and BSD date (macOS dev boxes) take different flags.
rfc3339_in_minutes() {
  date -u -d "+$1 minutes" '+%Y-%m-%dT%H:%M:%SZ' 2>/dev/null \
    || date -u -v"+$1M" '+%Y-%m-%dT%H:%M:%SZ'
}

# current_row_version <token> <artifact-id> — GETs the artifact and echoes its
# rowVersion, for the If-Match an approve call must now send (optimistic
# concurrency, spec 2026-08-11-orbeat-optimistic-concurrency-design.md §5, §2
# table row "Approve artifact"). Every approve call site below needs its OWN
# fresh fetch immediately before the call — submit/approve/rollback/PUT each
# bump row_version via the trigger, so a value captured earlier in the script
# is stale by the time a later approve runs.
current_row_version() {
  local tok="$1" aid="$2" body rv
  body=$(curl -s -H "Authorization: Bearer $tok" "http://localhost:8080/v1/admin/artifacts/$aid")
  if command -v jq >/dev/null 2>&1; then
    rv=$(echo "$body" | jq -r '.rowVersion')
  else
    rv=$(echo "$body" | grep -o '"rowVersion":[0-9]*' | head -n1 | sed 's/"rowVersion"://')
  fi
  [ -n "$rv" ] && [ "$rv" != "null" ] || { echo "FAIL: could not resolve rowVersion for $aid: $body" >&2; exit 1; }
  echo "$rv"
}

# ── Marketplace artifact validation (Phase 2-a) ────────────────────────────────
echo "==> validating the generated Claude Code marketplace"
MP_TMP=$(mktemp -d)
go run ./cmd/marketplacegen -out "$MP_TMP" -gateway-url http://localhost:8090
MP_MANIFEST="$MP_TMP/.claude-plugin/marketplace.json"
MCP_JSON="$MP_TMP/plugins/orbeat-gateway/.mcp.json"
test -f "$MP_MANIFEST" || { echo "FAIL: marketplace.json not generated"; rm -rf "$MP_TMP"; exit 1; }
test -f "$MCP_JSON" || { echo "FAIL: .mcp.json not generated"; rm -rf "$MP_TMP"; exit 1; }
if command -v jq >/dev/null 2>&1; then
  [ "$(jq -r '.name' "$MP_MANIFEST")" = "orbeat" ] || { echo "FAIL: marketplace name != orbeat"; rm -rf "$MP_TMP"; exit 1; }
  [ "$(jq -r '.plugins[0].source' "$MP_MANIFEST")" = "./plugins/orbeat-gateway" ] || { echo "FAIL: plugin source wrong"; rm -rf "$MP_TMP"; exit 1; }
  [ "$(jq -r '.mcpServers["orbeat-gateway"].url' "$MCP_JSON")" = "http://localhost:8090/mcp" ] || { echo "FAIL: mcp url wrong"; rm -rf "$MP_TMP"; exit 1; }
else
  # grep fallback omits the plugins[0].source check (jq unavailable); CI always has jq.
  grep -q '"name": "orbeat"' "$MP_MANIFEST" || { echo "FAIL: marketplace name != orbeat"; rm -rf "$MP_TMP"; exit 1; }
  grep -q '"url": "http://localhost:8090/mcp"' "$MCP_JSON" || { echo "FAIL: mcp url wrong"; rm -rf "$MP_TMP"; exit 1; }
fi
rm -rf "$MP_TMP"
echo "    marketplace OK: name=orbeat, plugin source + mcp url correct"

echo "==> bringing stack up"
$COMPOSE up --build -d

echo "==> waiting for api health"
for i in $(seq 1 30); do
  curl -sf localhost:8080/healthz >/dev/null && break
  [ "$i" -eq 30 ] && { echo "api did not become healthy"; exit 1; }
  sleep 2
done

echo "==> waiting for gateway health"
for i in $(seq 1 30); do
  curl -sf localhost:8090/healthz >/dev/null && break
  [ "$i" -eq 30 ] && { echo "gateway did not become healthy"; exit 1; }
  sleep 2
done

api_body=$(curl -s localhost:8080/healthz)
gw_body=$(curl -s localhost:8090/healthz)
echo "$api_body" | grep -q '"status":"ok"' || { echo "api body: $api_body"; exit 1; }
echo "$gw_body" | grep -q '"status":"ok"' || { echo "gateway body: $gw_body"; exit 1; }

echo "==> waiting for portal health"
for i in $(seq 1 30); do
  curl -sf localhost:8081/healthz >/dev/null && break
  [ "$i" -eq 30 ] && { echo "portal did not become healthy"; exit 1; }
  sleep 2
done
portal_root=$(curl -sf localhost:8081/)
echo "$portal_root" | grep -q '<div id="root">' || { echo "FAIL: portal / missing SPA shell (<div id=\"root\">)"; exit 1; }
echo "    portal /healthz => ok, / serves the SPA shell"

# ── Keycloak authentication checks ─────────────────────────────────────────────

echo "==> waiting for Keycloak realm discovery"
KC_DISCOVERY="http://localhost:8088/realms/orbeat/.well-known/openid-configuration"
for i in $(seq 1 40); do
  http_code=$(curl -s -o /dev/null -w "%{http_code}" "$KC_DISCOVERY")
  if [ "$http_code" = "200" ]; then
    echo "    Keycloak ready (try $i)"
    break
  fi
  if [ "$i" -eq 40 ]; then
    echo "FAIL: Keycloak realm discovery not ready after 40 tries (last HTTP $http_code)"
    exit 1
  fi
  sleep 3
done

echo "==> fetching token via password grant (alice)"
TOKEN_ENDPOINT="http://localhost:8088/realms/orbeat/protocol/openid-connect/token"
token_response=$(curl -s \
  -d grant_type=password \
  -d client_id=orbeat-cli \
  -d username=alice \
  -d password=alice \
  "$TOKEN_ENDPOINT")

if command -v jq >/dev/null 2>&1; then
  ACCESS_TOKEN=$(echo "$token_response" | jq -r '.access_token // empty')
else
  # Portable fallback: extract access_token value between quotes after the key
  ACCESS_TOKEN=$(echo "$token_response" | grep -o '"access_token":"[^"]*"' | sed 's/"access_token":"//;s/"//')
fi

if [ -z "$ACCESS_TOKEN" ]; then
  echo "FAIL: could not extract access_token from response: $token_response"
  exit 1
fi
echo "    token obtained (${#ACCESS_TOKEN} chars)"

echo "==> asserting GET /v1/me with Bearer token => 200 + orbeat-user"
me_response=$(curl -s -w '\n%{http_code}' \
  -H "Authorization: Bearer $ACCESS_TOKEN" \
  http://localhost:8080/v1/me)
me_status=$(printf '%s' "$me_response" | tail -n1)
me_body=$(printf '%s' "$me_response" | sed '$d')

if [ "$me_status" != "200" ]; then
  echo "FAIL: /v1/me with token returned HTTP $me_status; body: $me_body"
  exit 1
fi
if ! echo "$me_body" | grep -q "orbeat-user"; then
  echo "FAIL: /v1/me response does not contain 'orbeat-user'; body: $me_body"
  exit 1
fi
echo "    /v1/me => 200, body contains orbeat-user"

echo "==> asserting GET /v1/me with NO token => 401 (fail-closed)"
no_token_status=$(curl -s -o /dev/null -w "%{http_code}" http://localhost:8080/v1/me)
if [ "$no_token_status" != "401" ]; then
  echo "FAIL: /v1/me without token returned HTTP $no_token_status, expected 401"
  exit 1
fi
echo "    /v1/me (no token) => 401 (fail-closed verified)"

echo "==> asserting GET /v1/catalog with token => 200 + servers array"
cat_response=$(curl -s -w '\n%{http_code}' -H "Authorization: Bearer $ACCESS_TOKEN" http://localhost:8080/v1/catalog)
cat_status=$(printf '%s' "$cat_response" | tail -n1)
cat_body=$(printf '%s' "$cat_response" | sed '$d')
[ "$cat_status" = "200" ] || { echo "FAIL: /v1/catalog status $cat_status; body: $cat_body"; exit 1; }
echo "$cat_body" | grep -q '"servers"' || { echo "FAIL: /v1/catalog missing servers field: $cat_body"; exit 1; }
echo "    /v1/catalog => 200 with servers array"

echo "==> asserting GET /v1/catalog with NO token => 401"
cat_no=$(curl -s -o /dev/null -w '%{http_code}' http://localhost:8080/v1/catalog)
[ "$cat_no" = "401" ] || { echo "FAIL: /v1/catalog no-token status $cat_no, want 401"; exit 1; }
echo "    /v1/catalog (no token) => 401"

# ── Admin CRUD + RBAC end-to-end (P1-d-2b) ─────────────────────────────────────

get_token() { # $1=username $2=password -> echoes access_token
  local resp
  resp=$(curl -s -d grant_type=password -d client_id=orbeat-cli \
    -d "username=$1" -d "password=$2" "$TOKEN_ENDPOINT")
  if command -v jq >/dev/null 2>&1; then
    echo "$resp" | jq -r '.access_token // empty'
  else
    echo "$resp" | grep -o '"access_token":"[^"]*"' | sed 's/"access_token":"//;s/"//'
  fi
}

# seed_sync_token <home> — write a valid orbeat-sync credential cache into <home>,
# the way `orbeat-sync login` would. The device flow is interactive (and is covered
# by the acceptance runbook); reconciliation is the subject here, so a real
# password-grant token is written directly.
#
# Call this per HOME, and AFTER any `cp -a` that creates one: a copied home inherits
# whatever credentials.json held at copy time, and the seeded window is short (see
# below), so re-seeding only the original leaves the copies carrying a stale token.
#
# The window: a 5-minute expiry, minus Token.Valid()'s 60s skew
# (internal/syncclient/token.go:22-28), means loadValidToken starts failing around
# T+4min. Keycloak's own 5-minute default caps the real JWT independently. Either
# failure returns before the render block (cmd/sync/main.go:249-252), so the run
# exits 1 with NO JSON — which would mask a scenario expecting exit 2 as an
# unexplainable red.
seed_sync_token() {
  local home="$1" tok exp
  # `|| true` is load-bearing: a command substitution propagates the callee's exit
  # status, and under `set -e` a failing get_token would abort here BEFORE the
  # guard below could print a diagnostic. See the same pattern at the partial-
  # failure capture further down this file.
  tok=$(get_token alice alice) || true
  [ -n "$tok" ] || { echo "FAIL: seed_sync_token could not fetch alice's token"; exit 1; }
  exp=$(rfc3339_in_minutes 5)
  mkdir -p "$home/.config/orbeat"
  cat > "$home/.config/orbeat/credentials.json" <<EOF
{"access_token":"$tok","refresh_token":"","expiry":"$exp"}
EOF
  chmod 600 "$home/.config/orbeat/credentials.json"
}

echo "==> fetching admin token (boss)"
ADMIN_TOKEN=$(get_token boss boss)
[ -n "$ADMIN_TOKEN" ] || { echo "FAIL: no admin token for boss"; exit 1; }

echo "==> non-admin (alice) hitting admin route => 403"
forbidden=$(curl -s -o /dev/null -w '%{http_code}' \
  -H "Authorization: Bearer $ACCESS_TOKEN" http://localhost:8080/v1/admin/servers)
[ "$forbidden" = "403" ] || { echo "FAIL: alice on /v1/admin/servers = $forbidden, want 403"; exit 1; }
echo "    alice => 403 on admin route"

echo "==> admin creates an MCP server"
create_resp=$(curl -s -w '\n%{http_code}' -X POST \
  -H "Authorization: Bearer $ADMIN_TOKEN" -H "Content-Type: application/json" \
  -d '{"name":"smoke-github","transport":"http","endpointOrCommand":"https://api.example/mcp","secretRef":"vault:kv/orbeat/smoke#token","status":"active"}' \
  http://localhost:8080/v1/admin/servers)
create_status=$(printf '%s' "$create_resp" | tail -n1)
create_body=$(printf '%s' "$create_resp" | sed '$d')
[ "$create_status" = "201" ] || { echo "FAIL: create server $create_status: $create_body"; exit 1; }
echo "$create_body" | grep -q '"hasSecret":true' || { echo "FAIL: hasSecret not true: $create_body"; exit 1; }
if echo "$create_body" | grep -q '"secretRef"'; then echo "FAIL: admin response leaked secretRef: $create_body"; exit 1; fi
if command -v jq >/dev/null 2>&1; then
  SERVER_ID=$(echo "$create_body" | jq -r '.id')
else
  SERVER_ID=$(echo "$create_body" | grep -o '"id":"[^"]*"' | head -n1 | sed 's/"id":"//;s/"//')
fi
echo "    server created id=$SERVER_ID (hasSecret=true, no secretRef leak)"

echo "==> admin creates role orbeat-user (idempotent: 201 or 409)"
role_status=$(curl -s -o /dev/null -w '%{http_code}' -X POST \
  -H "Authorization: Bearer $ADMIN_TOKEN" -H "Content-Type: application/json" \
  -d '{"name":"orbeat-user"}' http://localhost:8080/v1/admin/roles)
case "$role_status" in
  201|409) : ;;
  *) echo "FAIL: create role status $role_status"; exit 1 ;;
esac
roles_body=$(curl -s -H "Authorization: Bearer $ADMIN_TOKEN" http://localhost:8080/v1/admin/roles)
if command -v jq >/dev/null 2>&1; then
  ROLE_ID=$(echo "$roles_body" | jq -r '.roles[] | select(.name=="orbeat-user") | .id')
else
  ROLE_ID=$(echo "$roles_body" | grep -o '{"id":"[^"]*","name":"orbeat-user"}' | grep -o '"id":"[^"]*"' | head -n1 | sed 's/"id":"//;s/"//')
fi
[ -n "$ROLE_ID" ] || { echo "FAIL: could not resolve orbeat-user role id: $roles_body"; exit 1; }
echo "    role orbeat-user id=$ROLE_ID"

echo "==> admin entitles orbeat-user to the server"
ent_status=$(curl -s -o /dev/null -w '%{http_code}' -X POST \
  -H "Authorization: Bearer $ADMIN_TOKEN" -H "Content-Type: application/json" \
  -d "{\"roleId\":\"$ROLE_ID\",\"mcpServerId\":\"$SERVER_ID\"}" \
  http://localhost:8080/v1/admin/entitlements)
case "$ent_status" in
  201|409) : ;;
  *) echo "FAIL: create entitlement status $ent_status"; exit 1 ;;
esac
echo "    entitlement created ($ent_status)"

echo "==> alice now sees the entitled server in /v1/catalog"
alice_cat=$(curl -s -H "Authorization: Bearer $ACCESS_TOKEN" http://localhost:8080/v1/catalog)
echo "$alice_cat" | grep -q '"name":"smoke-github"' || { echo "FAIL: alice catalog missing smoke-github: $alice_cat"; exit 1; }
if echo "$alice_cat" | grep -q '"secretRef"'; then echo "FAIL: catalog leaked secretRef: $alice_cat"; exit 1; fi
echo "    alice sees smoke-github (no secret leak)"

echo "==> admin audit log records the mutations"
audit_body=$(curl -s -H "Authorization: Bearer $ADMIN_TOKEN" "http://localhost:8080/v1/admin/audit?limit=50")
echo "$audit_body" | grep -q '"action":"server.create"' || { echo "FAIL: audit missing server.create: $audit_body"; exit 1; }
echo "$audit_body" | grep -q '"action":"entitlement.create"' || { echo "FAIL: audit missing entitlement.create: $audit_body"; exit 1; }
echo "    audit shows server.create + entitlement.create"

echo "==> audit query is keyset-paginable (limit echoed + nextCursor present)"
audit_p1=$(curl -s -H "Authorization: Bearer $ADMIN_TOKEN" "http://localhost:8080/v1/admin/audit?limit=1")
echo "$audit_p1" | grep -q '"limit":1' || { echo "FAIL: audit missing echoed limit: $audit_p1"; exit 1; }
echo "$audit_p1" | grep -q '"nextCursor":"[^"]' || { echo "FAIL: audit page1 missing nextCursor: $audit_p1"; exit 1; }
echo "    audit page1 => limit echoed + nextCursor present"

echo "==> api emits structured JSON logs + dual-emits audit events to the log stream"
api_logs=$($COMPOSE logs api 2>/dev/null)
echo "$api_logs" | grep -q '"service":"orbeat-api"' || { echo "FAIL: no structured JSON log line from orbeat-api"; exit 1; }
echo "$api_logs" | grep -q '"msg":"http_request"' || { echo "FAIL: no http_request log line"; exit 1; }
echo "$api_logs" | grep -q '"event":"audit"' || { echo "FAIL: no event=audit log line (audit dual-emit)"; exit 1; }
echo "    orbeat-api structured JSON logs + http_request + audit dual-emit present"

echo "==> rate limiter never rejected a request during this smoke run"
# This is a NEGATIVE assertion (grep for zero matches), which is the one kind
# of grep assertion that can pass for a reason unrelated to the thing it
# claims: either the rejection genuinely never happened, or the pattern never
# matches anything at all (a typo, a log-level mismatch, message-text drift).
# A silent-pass-forever gate is worse than no gate (plan correction C5,
# docs/plans/orbeat-rate-limiting-2026-08-12.md), so this is proven capable of
# matching FIRST, by a Go positive control that actually drives a rejection
# and asserts the exact same literal appears in the emitted log line
# (internal/ratelimit/http_test.go's TestHTTPRejectedLogContainsExportedMessage).
# The literal below is copied from internal/ratelimit.RejectedLogMessage
# (internal/ratelimit/observability.go) rather than importing it (bash cannot
# import a Go constant) — if that constant's text ever changes without this
# line changing to match, the Go test above breaks LOUDLY, rather than this
# grep silently matching nothing forever.
echo "$api_logs" | grep -q 'rate limit exceeded' && { echo "FAIL: api log contains a rate-limit rejection during smoke — either the limiter is under-provisioned for this stack (see deploy/docker-compose.yml's ORBEAT_RATELIMIT_BURST comment) or smoke.sh's own call volume grew; do not silence this by widening the negative match"; exit 1; }
echo "    api log contains zero 'rate limit exceeded' rejections"

echo "==> audit export (JSON + CSV, date range)"
export_json=$(curl -s -H "Authorization: Bearer $ADMIN_TOKEN" "http://localhost:8080/v1/admin/audit/export?format=json")
echo "$export_json" | grep -q '"action":"server.create"' || { echo "FAIL: audit export json missing server.create: $export_json"; exit 1; }
export_csv=$(curl -s -H "Authorization: Bearer $ADMIN_TOKEN" "http://localhost:8080/v1/admin/audit/export?format=csv")
echo "$export_csv" | head -n1 | grep -q '^id,ts,actor,action,target,decision,metadata' || { echo "FAIL: audit export csv header wrong: $(echo "$export_csv" | head -n1)"; exit 1; }
echo "$export_csv" | grep -q 'server.create' || { echo "FAIL: audit export csv missing server.create row"; exit 1; }
export_ct=$(curl -s -o /dev/null -w '%{content_type}' -H "Authorization: Bearer $ADMIN_TOKEN" "http://localhost:8080/v1/admin/audit/export?format=csv")
echo "$export_ct" | grep -q 'text/csv' || { echo "FAIL: audit export csv content-type wrong: $export_ct"; exit 1; }
export_bad=$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $ADMIN_TOKEN" "http://localhost:8080/v1/admin/audit/export?format=xml")
[ "$export_bad" = "400" ] || { echo "FAIL: audit export bad format => $export_bad, want 400"; exit 1; }
echo "    audit export json+csv OK (csv content-type text/csv, bad format => 400)"

# ── Gateway end-to-end: call a real upstream tool THROUGH the gateway (P1-e) ────

echo "==> waiting for example upstream MCP server health (:9000)"
for i in $(seq 1 30); do
  curl -sf localhost:9000/healthz >/dev/null && break
  [ "$i" -eq 30 ] && { echo "FAIL: upstream did not become healthy"; exit 1; }
  sleep 2
done
echo "    upstream /healthz => ok"

echo "==> admin registers the example upstream in the catalog"
up_resp=$(curl -s -w '\n%{http_code}' -X POST \
  -H "Authorization: Bearer $ADMIN_TOKEN" -H "Content-Type: application/json" \
  -d '{"name":"smoke-upstream","transport":"http","endpointOrCommand":"http://upstream:9000/mcp","secretRef":"","status":"active"}' \
  http://localhost:8080/v1/admin/servers)
up_status=$(printf '%s' "$up_resp" | tail -n1)
up_body=$(printf '%s' "$up_resp" | sed '$d')
[ "$up_status" = "201" ] || { echo "FAIL: register upstream $up_status: $up_body"; exit 1; }
if command -v jq >/dev/null 2>&1; then
  UPSTREAM_ID=$(echo "$up_body" | jq -r '.id')
else
  UPSTREAM_ID=$(echo "$up_body" | grep -o '"id":"[^"]*"' | head -n1 | sed 's/"id":"//;s/"//')
fi
[ -n "$UPSTREAM_ID" ] || { echo "FAIL: could not resolve upstream server id: $up_body"; exit 1; }
echo "    upstream registered id=$UPSTREAM_ID"

echo "==> admin entitles orbeat-user to the upstream for tool echo"
up_ent_status=$(curl -s -o /dev/null -w '%{http_code}' -X POST \
  -H "Authorization: Bearer $ADMIN_TOKEN" -H "Content-Type: application/json" \
  -d "{\"roleId\":\"$ROLE_ID\",\"mcpServerId\":\"$UPSTREAM_ID\",\"allowedTools\":[\"echo\"]}" \
  http://localhost:8080/v1/admin/entitlements)
case "$up_ent_status" in
  201|409) : ;;
  *) echo "FAIL: entitle upstream status $up_ent_status"; exit 1 ;;
esac
echo "    entitlement (echo) created ($up_ent_status)"

echo "==> gateway protected-resource metadata + no-token 401"
gw_no_token=$(curl -s -o /dev/null -w '%{http_code}' http://localhost:8090/mcp)
[ "$gw_no_token" = "401" ] || { echo "FAIL: gateway /mcp no-token status $gw_no_token, want 401"; exit 1; }
gw_meta=$(curl -sf http://localhost:8090/.well-known/oauth-protected-resource)
echo "$gw_meta" | grep -q '"resource"' || { echo "FAIL: gateway metadata missing resource: $gw_meta"; exit 1; }
echo "    gateway /mcp (no token) => 401, metadata advertises resource"

echo "==> calling upstream tool through the gateway as alice"
GATEWAY_MCP_URL=http://localhost:8090/mcp ACCESS_TOKEN="$ACCESS_TOKEN" WANT_TOOL="smoke-upstream__echo" \
  go run ./deploy/smokeclient || { echo "FAIL: gateway tool call"; exit 1; }
echo "    gateway tool round-trip OK"

# ── Artifact publish round-trip (Phase 2-b-1) ──────────────────────────────────

echo "==> admin creates an artifact (skill)"
artifact_resp=$(curl -s -w '\n%{http_code}' -X POST \
  -H "Authorization: Bearer $ADMIN_TOKEN" -H "Content-Type: application/json" \
  -d '{"type":"skill","name":"smoke-skill","description":"d","content":"---\nname: smoke-skill\ndescription: d\n---\nbody"}' \
  http://localhost:8080/v1/admin/artifacts)
artifact_status=$(printf '%s' "$artifact_resp" | tail -n1)
artifact_body=$(printf '%s' "$artifact_resp" | sed '$d')
[ "$artifact_status" = "201" ] || { echo "FAIL: create artifact $artifact_status: $artifact_body"; exit 1; }
if command -v jq >/dev/null 2>&1; then
  ARTIFACT_ID=$(echo "$artifact_body" | jq -r '.id')
else
  ARTIFACT_ID=$(echo "$artifact_body" | grep -o '"id":"[^"]*"' | head -n1 | sed 's/"id":"//;s/"//')
fi
echo "    artifact created id=$ARTIFACT_ID"

art_get=$(curl -s -w '\n%{http_code}' -H "Authorization: Bearer $ADMIN_TOKEN" "http://localhost:8080/v1/admin/artifacts/$ARTIFACT_ID")
art_get_status=$(printf '%s' "$art_get" | tail -n1)
[ "$art_get_status" = "200" ] || { echo "FAIL: GET artifact $ARTIFACT_ID = $art_get_status"; exit 1; }
echo "    artifact GET-by-id => 200"

# Assert the publish-status endpoint shows a non-empty lastCommit.
# The publisher is async+debounced (~750ms), so we poll for up to ~20s.
# NOTE: The api container runs distroless (no shell, no cat), so we cannot
# docker exec to read /tmp/orbeat-marketplace directly. We rely on
# GET /v1/admin/marketplace/status returning a non-empty lastCommit as the
# primary signal that the publish worker has committed the rendered tree.
echo "==> polling marketplace status for a commit (up to 20s)"
commit_seen=0
for i in $(seq 1 20); do
  mp_status=$(curl -s -H "Authorization: Bearer $ADMIN_TOKEN" http://localhost:8080/v1/admin/marketplace/status)
  if command -v jq >/dev/null 2>&1; then
    last_commit=$(echo "$mp_status" | jq -r '.lastCommit // empty')
  else
    last_commit=$(echo "$mp_status" | grep -o '"lastCommit":"[^"]*"' | sed 's/"lastCommit":"//;s/"//')
  fi
  if [ -n "$last_commit" ] && [ "$last_commit" != "null" ]; then
    echo "    marketplace status: lastCommit=$last_commit"
    commit_seen=1
    break
  fi
  sleep 1
done
[ "$commit_seen" = "1" ] || { echo "FAIL: marketplace lastCommit still empty after 20s; status: $mp_status"; exit 1; }
echo "    artifact publish round-trip OK (lastCommit non-empty)"

echo "==> asserting audit log contains marketplace.publish"
audit_pub=$(curl -s -H "Authorization: Bearer $ADMIN_TOKEN" "http://localhost:8080/v1/admin/audit?limit=50")
echo "$audit_pub" | grep -q '"action":"marketplace.publish"' || { echo "FAIL: audit missing marketplace.publish: $audit_pub"; exit 1; }
echo "    audit shows marketplace.publish"

# ── Client bootstrap config: GET /v1/sync/config (Phase 3 Slice A) ─────────────

echo "==> asserting GET /v1/sync/config with token => 200 + non-empty gateway_url"
sync_cfg=$(curl -s -w '\n%{http_code}' -H "Authorization: Bearer $ACCESS_TOKEN" http://localhost:8080/v1/sync/config)
sync_cfg_status=$(printf '%s' "$sync_cfg" | tail -n1)
sync_cfg_body=$(printf '%s' "$sync_cfg" | sed '$d')
[ "$sync_cfg_status" = "200" ] || { echo "FAIL: /v1/sync/config status $sync_cfg_status; body: $sync_cfg_body"; exit 1; }
if command -v jq >/dev/null 2>&1; then
  GATEWAY_URL=$(echo "$sync_cfg_body" | jq -r '.gateway_url // empty')
else
  GATEWAY_URL=$(echo "$sync_cfg_body" | grep -o '"gateway_url":"[^"]*"' | sed 's/"gateway_url":"//;s/"//')
fi
[ -n "$GATEWAY_URL" ] || { echo "FAIL: /v1/sync/config returned empty gateway_url: $sync_cfg_body"; exit 1; }
echo "    /v1/sync/config => 200 with gateway_url=$GATEWAY_URL"

echo "==> asserting GET /v1/sync/config with NO token => 401"
sync_cfg_no=$(curl -s -o /dev/null -w '%{http_code}' http://localhost:8080/v1/sync/config)
[ "$sync_cfg_no" = "401" ] || { echo "FAIL: /v1/sync/config no-token status $sync_cfg_no, want 401"; exit 1; }
echo "    /v1/sync/config (no token) => 401"

# ── Artifact approval governance gate (Phase 4) ────────────────────────────────

echo "==> Phase 4 governance gate: unapproved hidden, approve→distributed, self-approve 403"

echo "==> admin creates a role-visibility subagent (draft) entitled to orbeat-user"
gov_resp=$(curl -s -w '\n%{http_code}' -X POST \
  -H "Authorization: Bearer $ADMIN_TOKEN" -H "Content-Type: application/json" \
  -d '{"type":"subagent","name":"smoke-gov","description":"d","content":"---\nname: smoke-gov\ndescription: d\n---\nAPPROVED-BODY","visibility":"role"}' \
  http://localhost:8080/v1/admin/artifacts)
gov_status=$(printf '%s' "$gov_resp" | tail -n1)
gov_body=$(printf '%s' "$gov_resp" | sed '$d')
[ "$gov_status" = "201" ] || { echo "FAIL: create governed artifact $gov_status: $gov_body"; exit 1; }
if command -v jq >/dev/null 2>&1; then
  GOV_ID=$(echo "$gov_body" | jq -r '.id')
else
  GOV_ID=$(echo "$gov_body" | grep -o '"id":"[^"]*"' | head -n1 | sed 's/"id":"//;s/"//')
fi
[ -n "$GOV_ID" ] || { echo "FAIL: could not resolve governed artifact id: $gov_body"; exit 1; }
echo "    draft subagent created id=$GOV_ID"

gov_ent_status=$(curl -s -o /dev/null -w '%{http_code}' -X POST \
  -H "Authorization: Bearer $ADMIN_TOKEN" -H "Content-Type: application/json" \
  -d "{\"roleId\":\"$ROLE_ID\",\"artifactId\":\"$GOV_ID\"}" \
  http://localhost:8080/v1/admin/artifact-entitlements)
case "$gov_ent_status" in
  201|409) : ;;
  *) echo "FAIL: create artifact entitlement status $gov_ent_status"; exit 1 ;;
esac
echo "    orbeat-user entitled to smoke-gov ($gov_ent_status)"

echo "==> unapproved artifact is NOT distributed via /v1/sync/artifacts (alice)"
sync_before=$(curl -s -w '\n%{http_code}' -H "Authorization: Bearer $ACCESS_TOKEN" http://localhost:8080/v1/sync/artifacts)
sync_before_status=$(printf '%s' "$sync_before" | tail -n1)
sync_before_body=$(printf '%s' "$sync_before" | sed '$d')
[ "$sync_before_status" = "200" ] || { echo "FAIL: /v1/sync/artifacts (pre-approve) status $sync_before_status: $sync_before_body"; exit 1; }
if echo "$sync_before_body" | grep -q '"name":"smoke-gov"'; then
  echo "FAIL: unapproved smoke-gov leaked via /v1/sync/artifacts: $sync_before_body"
  exit 1
fi
echo "    smoke-gov absent from sync while unapproved (draft not distributed)"

echo "==> submit (boss) then approve (boss2, separation of duties)"
submit_status=$(curl -s -o /dev/null -w '%{http_code}' -X POST \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  "http://localhost:8080/v1/admin/artifacts/$GOV_ID/submit")
[ "$submit_status" = "200" ] || { echo "FAIL: submit smoke-gov status $submit_status"; exit 1; }
echo "    smoke-gov submitted (boss) => 200"

BOSS2_TOKEN=$(get_token boss2 boss2)
[ -n "$BOSS2_TOKEN" ] || { echo "FAIL: no admin token for boss2"; exit 1; }

# approve now enforces If-Match (optimistic concurrency, spec §5): fetch the
# current rowVersion (submit above bumped it via the row_version trigger, so
# it is no longer 1) and echo it back as the precondition.
GOV_RV=$(current_row_version "$ADMIN_TOKEN" "$GOV_ID")
approve_status=$(curl -s -o /dev/null -w '%{http_code}' -X POST \
  -H "Authorization: Bearer $BOSS2_TOKEN" -H "If-Match: \"$GOV_RV\"" \
  "http://localhost:8080/v1/admin/artifacts/$GOV_ID/approve")
[ "$approve_status" = "200" ] || { echo "FAIL: approve smoke-gov (boss2) status $approve_status"; exit 1; }
echo "    smoke-gov approved (boss2) => 200"

echo "==> approved artifact IS distributed via /v1/sync/artifacts (alice)"
sync_after=$(curl -s -w '\n%{http_code}' -H "Authorization: Bearer $ACCESS_TOKEN" http://localhost:8080/v1/sync/artifacts)
sync_after_status=$(printf '%s' "$sync_after" | tail -n1)
sync_after_body=$(printf '%s' "$sync_after" | sed '$d')
[ "$sync_after_status" = "200" ] || { echo "FAIL: /v1/sync/artifacts (post-approve) status $sync_after_status: $sync_after_body"; exit 1; }
echo "$sync_after_body" | grep -q '"name":"smoke-gov"' || { echo "FAIL: approved smoke-gov missing from sync: $sync_after_body"; exit 1; }
echo "$sync_after_body" | grep -q 'APPROVED-BODY' || { echo "FAIL: approved content missing from sync: $sync_after_body"; exit 1; }
echo "    smoke-gov present in sync post-approval, with approved content"

echo "==> edit smoke-gov to v2, approve (rev 2), then roll back to rev 1"
# PUT now enforces If-Match (optimistic concurrency, spec
# 2026-08-11-orbeat-optimistic-concurrency-design.md §5): fetch the current
# rowVersion first (submit + approve above each bumped it via the row_version
# trigger, so it is no longer 1) and echo it back as the precondition.
gov_get_body=$(curl -s -H "Authorization: Bearer $ADMIN_TOKEN" "http://localhost:8080/v1/admin/artifacts/$GOV_ID")
if command -v jq >/dev/null 2>&1; then
  GOV_RV=$(echo "$gov_get_body" | jq -r '.rowVersion')
else
  GOV_RV=$(echo "$gov_get_body" | grep -o '"rowVersion":[0-9]*' | head -n1 | sed 's/"rowVersion"://')
fi
[ -n "$GOV_RV" ] && [ "$GOV_RV" != "null" ] || { echo "FAIL: could not resolve rowVersion for $GOV_ID: $gov_get_body"; exit 1; }
gov_v2_status=$(curl -s -o /dev/null -w '%{http_code}' -X PUT \
  -H "Authorization: Bearer $ADMIN_TOKEN" -H "Content-Type: application/json" \
  -H "If-Match: \"$GOV_RV\"" \
  -d '{"type":"subagent","name":"smoke-gov","description":"d","content":"---\nname: smoke-gov\ndescription: d\n---\nV2-BODY","visibility":"role"}' \
  "http://localhost:8080/v1/admin/artifacts/$GOV_ID")
[ "$gov_v2_status" = "200" ] || { echo "FAIL: edit smoke-gov to v2 status $gov_v2_status"; exit 1; }

submit2_status=$(curl -s -o /dev/null -w '%{http_code}' -X POST \
  -H "Authorization: Bearer $ADMIN_TOKEN" "http://localhost:8080/v1/admin/artifacts/$GOV_ID/submit")
[ "$submit2_status" = "200" ] || { echo "FAIL: resubmit smoke-gov status $submit2_status"; exit 1; }
# approve now enforces If-Match too (spec §5, §2): the PUT and resubmit above
# each bumped row_version again, so GOV_RV from before the PUT is stale —
# fetch fresh.
GOV_RV2=$(current_row_version "$ADMIN_TOKEN" "$GOV_ID")
approve2_status=$(curl -s -o /dev/null -w '%{http_code}' -X POST \
  -H "Authorization: Bearer $BOSS2_TOKEN" -H "If-Match: \"$GOV_RV2\"" \
  "http://localhost:8080/v1/admin/artifacts/$GOV_ID/approve")
[ "$approve2_status" = "200" ] || { echo "FAIL: approve smoke-gov v2 (boss2) status $approve2_status"; exit 1; }

sync_v2=$(curl -s -H "Authorization: Bearer $ACCESS_TOKEN" http://localhost:8080/v1/sync/artifacts)
if ! echo "$sync_v2" | grep -q 'V2-BODY'; then echo "FAIL: v2 not distributed after approve: $sync_v2"; exit 1; fi
echo "    smoke-gov v2 distributed"

# roll distribution back to revision 1 (single-admin; boss2 is fine — no separation of duties on rollback)
rollback_status=$(curl -s -o /dev/null -w '%{http_code}' -X POST \
  -H "Authorization: Bearer $BOSS2_TOKEN" -H "Content-Type: application/json" \
  -d '{"revision":1}' "http://localhost:8080/v1/admin/artifacts/$GOV_ID/rollback")
[ "$rollback_status" = "200" ] || { echo "FAIL: rollback smoke-gov to rev 1 status $rollback_status"; exit 1; }

sync_rb=$(curl -s -H "Authorization: Bearer $ACCESS_TOKEN" http://localhost:8080/v1/sync/artifacts)
if ! echo "$sync_rb" | grep -q 'APPROVED-BODY'; then echo "FAIL: rollback did not restore v1 content: $sync_rb"; exit 1; fi
if echo "$sync_rb" | grep -q 'V2-BODY'; then echo "FAIL: v2 content still distributed after rollback: $sync_rb"; exit 1; fi
echo "    rollback restored v1 (APPROVED-BODY) to distribution, v2 withdrawn"

echo "==> self-approval is blocked (separation of duties)"
selfapprove_resp=$(curl -s -w '\n%{http_code}' -X POST \
  -H "Authorization: Bearer $ADMIN_TOKEN" -H "Content-Type: application/json" \
  -d '{"type":"skill","name":"smoke-selfapprove","description":"d","content":"---\nname: smoke-selfapprove\ndescription: d\n---\nx","visibility":"org"}' \
  http://localhost:8080/v1/admin/artifacts)
selfapprove_status=$(printf '%s' "$selfapprove_resp" | tail -n1)
selfapprove_body=$(printf '%s' "$selfapprove_resp" | sed '$d')
[ "$selfapprove_status" = "201" ] || { echo "FAIL: create smoke-selfapprove $selfapprove_status: $selfapprove_body"; exit 1; }
if command -v jq >/dev/null 2>&1; then
  SELFAPPROVE_ID=$(echo "$selfapprove_body" | jq -r '.id')
else
  SELFAPPROVE_ID=$(echo "$selfapprove_body" | grep -o '"id":"[^"]*"' | head -n1 | sed 's/"id":"//;s/"//')
fi
[ -n "$SELFAPPROVE_ID" ] || { echo "FAIL: could not resolve smoke-selfapprove id: $selfapprove_body"; exit 1; }

selfsubmit_status=$(curl -s -o /dev/null -w '%{http_code}' -X POST \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  "http://localhost:8080/v1/admin/artifacts/$SELFAPPROVE_ID/submit")
[ "$selfsubmit_status" = "200" ] || { echo "FAIL: submit smoke-selfapprove status $selfsubmit_status"; exit 1; }

# A valid If-Match is required here too — the version guard runs BEFORE the
# separation-of-duties check (spec §5/§9 ordering), so a missing/stale header
# would 428/412 before ever reaching the self-approve check this asserts 403.
SELFAPPROVE_RV=$(current_row_version "$ADMIN_TOKEN" "$SELFAPPROVE_ID")
selfapprove_attempt_status=$(curl -s -o /dev/null -w '%{http_code}' -X POST \
  -H "Authorization: Bearer $ADMIN_TOKEN" -H "If-Match: \"$SELFAPPROVE_RV\"" \
  "http://localhost:8080/v1/admin/artifacts/$SELFAPPROVE_ID/approve")
[ "$selfapprove_attempt_status" = "403" ] || { echo "FAIL: self-approve smoke-selfapprove (boss) status $selfapprove_attempt_status, want 403"; exit 1; }
echo "    self-approval by submitter (boss) => 403 (separation of duties enforced)"

# ── Artifact revision pruning (docs/specs/2026-08-19-orbeat-revision-pruning-design.md) ─
#
# This is the ONLY gate that can catch ORBEAT_ARTIFACT_REVISION_KEEP being read
# by config but never wired into cmd/api — a portal feature and a sync feature
# have each shipped CI-green while unreachable in production, at exactly this
# seam. Everything above stops at the API layer's unit tests, which construct
# api.Server directly and set the cap themselves; nothing in Go proves cmd/api
# actually reads the env var. Only a real binary, started the way an operator
# starts it (env var -> compose -> cmd/api), can prove that.
#
# Uses a DEDICATED throwaway artifact, never smoke-gov: smoke-gov's own
# rollback-to-revision-1 assertion (above) depends on revision 1 surviving,
# and pruning it would break a governance gate to test a storage feature.
echo "==> revision pruning gate: KEEP=$ORBEAT_ARTIFACT_REVISION_KEEP caps artifact_revision per artifact"

echo "==> admin creates a throwaway artifact for the pruning gate (no entitlement needed — approve alone drives it)"
prune_resp=$(curl -s -w '\n%{http_code}' -X POST \
  -H "Authorization: Bearer $ADMIN_TOKEN" -H "Content-Type: application/json" \
  -d '{"type":"skill","name":"smoke-prune","description":"d","content":"---\nname: smoke-prune\ndescription: d\n---\nbody-v1","visibility":"org"}' \
  http://localhost:8080/v1/admin/artifacts)
prune_status=$(printf '%s' "$prune_resp" | tail -n1)
prune_body=$(printf '%s' "$prune_resp" | sed '$d')
[ "$prune_status" = "201" ] || { echo "FAIL: create smoke-prune $prune_status: $prune_body"; exit 1; }
if command -v jq >/dev/null 2>&1; then
  PRUNE_ID=$(echo "$prune_body" | jq -r '.id')
else
  PRUNE_ID=$(echo "$prune_body" | grep -o '"id":"[^"]*"' | head -n1 | sed 's/"id":"//;s/"//')
fi
[ -n "$PRUNE_ID" ] || { echo "FAIL: could not resolve smoke-prune id: $prune_body"; exit 1; }
echo "    smoke-prune created id=$PRUNE_ID"

# PRUNE_APPROVALS deliberately exceeds the cap by a margin (KEEP+2, not
# KEEP+1): KEEP+1 approvals surviving at exactly KEEP rows would also pass on
# an off-by-one mutant that retains KEEP+1 but happens to run one fewer prune
# than approvals. A 2-row margin means the DELETE fires on more than one
# approval before the final assertion, so "exactly KEEP rows survive" cannot
# pass by coincidence.
PRUNE_APPROVALS=$((ORBEAT_ARTIFACT_REVISION_KEEP + 2))
echo "==> approving smoke-prune $PRUNE_APPROVALS times (submit=boss, approve=boss2 — separation of duties) to exceed the cap"
for i in $(seq 1 "$PRUNE_APPROVALS"); do
  # Revision 1 comes from the first approve below with no prior edit (the
  # artifact is already draft). Every subsequent revision needs a PUT first —
  # approve leaves the artifact in state "approved", and submit only accepts
  # draft/rejected — mirroring the smoke-gov v2 edit cycle above.
  if [ "$i" -gt 1 ]; then
    prune_rv=$(current_row_version "$ADMIN_TOKEN" "$PRUNE_ID")
    prune_put_status=$(curl -s -o /dev/null -w '%{http_code}' -X PUT \
      -H "Authorization: Bearer $ADMIN_TOKEN" -H "Content-Type: application/json" \
      -H "If-Match: \"$prune_rv\"" \
      -d "{\"type\":\"skill\",\"name\":\"smoke-prune\",\"description\":\"d\",\"content\":\"---\nname: smoke-prune\ndescription: d\n---\nbody-v$i\",\"visibility\":\"org\"}" \
      "http://localhost:8080/v1/admin/artifacts/$PRUNE_ID")
    [ "$prune_put_status" = "200" ] || { echo "FAIL: edit smoke-prune iteration $i status $prune_put_status"; exit 1; }
  fi
  prune_submit_status=$(curl -s -o /dev/null -w '%{http_code}' -X POST \
    -H "Authorization: Bearer $ADMIN_TOKEN" "http://localhost:8080/v1/admin/artifacts/$PRUNE_ID/submit")
  [ "$prune_submit_status" = "200" ] || { echo "FAIL: submit smoke-prune iteration $i status $prune_submit_status"; exit 1; }
  prune_rv2=$(current_row_version "$ADMIN_TOKEN" "$PRUNE_ID")
  prune_approve_status=$(curl -s -o /dev/null -w '%{http_code}' -X POST \
    -H "Authorization: Bearer $BOSS2_TOKEN" -H "If-Match: \"$prune_rv2\"" \
    "http://localhost:8080/v1/admin/artifacts/$PRUNE_ID/approve")
  [ "$prune_approve_status" = "200" ] || { echo "FAIL: approve smoke-prune iteration $i status $prune_approve_status"; exit 1; }
done
echo "    smoke-prune approved $PRUNE_APPROVALS times"

echo "==> GET /v1/admin/artifacts/\$id/revisions returns exactly KEEP=$ORBEAT_ARTIFACT_REVISION_KEEP rows, not $PRUNE_APPROVALS"
prune_revs=$(curl -s -H "Authorization: Bearer $ADMIN_TOKEN" "http://localhost:8080/v1/admin/artifacts/$PRUNE_ID/revisions")
if command -v jq >/dev/null 2>&1; then
  prune_rev_count=$(echo "$prune_revs" | jq -r '.revisions | length')
else
  prune_rev_count=$(echo "$prune_revs" | grep -o '"revision":[0-9]*' | wc -l | tr -d ' ')
fi
[ "$prune_rev_count" = "$ORBEAT_ARTIFACT_REVISION_KEEP" ] || { echo "FAIL: smoke-prune has $prune_rev_count revisions after $PRUNE_APPROVALS approvals, want exactly $ORBEAT_ARTIFACT_REVISION_KEEP (ORBEAT_ARTIFACT_REVISION_KEEP not enforced end-to-end): $prune_revs"; exit 1; }
echo "    smoke-prune has exactly $ORBEAT_ARTIFACT_REVISION_KEEP revisions after $PRUNE_APPROVALS approvals (pruning enforced through the real binary)"

# ── Governed cross-tool rule artifact gate (Phase 3 Slice B) ───────────────────

echo "==> Phase 3 Slice B rule gate: create → submit (boss) → approve (boss2) → entitled rule in sync (alice)"

echo "==> admin creates a role-visibility rule artifact (draft) entitled to orbeat-user"
rule_resp=$(curl -s -w '\n%{http_code}' -X POST \
  -H "Authorization: Bearer $ADMIN_TOKEN" -H "Content-Type: application/json" \
  -d '{"type":"rule","name":"smoke-rule","description":"d","content":"Never commit secrets. RULE-BODY-NO-SECRETS","visibility":"role"}' \
  http://localhost:8080/v1/admin/artifacts)
rule_status=$(printf '%s' "$rule_resp" | tail -n1)
rule_body=$(printf '%s' "$rule_resp" | sed '$d')
[ "$rule_status" = "201" ] || { echo "FAIL: create rule artifact $rule_status: $rule_body"; exit 1; }
if command -v jq >/dev/null 2>&1; then
  RULE_ID=$(echo "$rule_body" | jq -r '.id')
else
  RULE_ID=$(echo "$rule_body" | grep -o '"id":"[^"]*"' | head -n1 | sed 's/"id":"//;s/"//')
fi
[ -n "$RULE_ID" ] || { echo "FAIL: could not resolve rule artifact id: $rule_body"; exit 1; }
echo "    draft rule created id=$RULE_ID"

rule_ent_status=$(curl -s -o /dev/null -w '%{http_code}' -X POST \
  -H "Authorization: Bearer $ADMIN_TOKEN" -H "Content-Type: application/json" \
  -d "{\"roleId\":\"$ROLE_ID\",\"artifactId\":\"$RULE_ID\"}" \
  http://localhost:8080/v1/admin/artifact-entitlements)
case "$rule_ent_status" in
  201|409) : ;;
  *) echo "FAIL: create rule artifact entitlement status $rule_ent_status"; exit 1 ;;
esac
echo "    orbeat-user entitled to smoke-rule ($rule_ent_status)"

echo "==> submit (boss) then approve (boss2, separation of duties) the rule"
rule_submit_status=$(curl -s -o /dev/null -w '%{http_code}' -X POST \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  "http://localhost:8080/v1/admin/artifacts/$RULE_ID/submit")
[ "$rule_submit_status" = "200" ] || { echo "FAIL: submit smoke-rule status $rule_submit_status"; exit 1; }
echo "    smoke-rule submitted (boss) => 200"

RULE_RV=$(current_row_version "$ADMIN_TOKEN" "$RULE_ID")
rule_approve_status=$(curl -s -o /dev/null -w '%{http_code}' -X POST \
  -H "Authorization: Bearer $BOSS2_TOKEN" -H "If-Match: \"$RULE_RV\"" \
  "http://localhost:8080/v1/admin/artifacts/$RULE_ID/approve")
[ "$rule_approve_status" = "200" ] || { echo "FAIL: approve smoke-rule (boss2) status $rule_approve_status"; exit 1; }
echo "    smoke-rule approved (boss2) => 200"

# Re-fetch alice's access token before the sync-consumption section. The token
# grabbed at the top of this run (~450 lines / several minutes ago) can outlive
# Keycloak's ~5-min access-token lifetime on a slow runner. Every assertion below
# reads /v1/sync/* as alice AND the binary gate seeds this token into the
# credential cache with a claimed 5-min expiry — a stale token here is a latent
# flake in the project's key gate (fable-audit D3). smoke-remote.sh fetches
# per-phase for the same reason.
echo "==> re-fetching alice's token for the sync-consumption section (avoids reuse past the ~5-min token lifetime)"
ACCESS_TOKEN=$(get_token alice alice)
[ -n "$ACCESS_TOKEN" ] || { echo "FAIL: could not re-fetch alice's token for the sync section"; exit 1; }

echo "==> approved rule IS distributed via /v1/sync/artifacts (alice) with type:rule + verbatim content"
sync_rule=$(curl -s -w '\n%{http_code}' -H "Authorization: Bearer $ACCESS_TOKEN" http://localhost:8080/v1/sync/artifacts)
sync_rule_status=$(printf '%s' "$sync_rule" | tail -n1)
sync_rule_body=$(printf '%s' "$sync_rule" | sed '$d')
[ "$sync_rule_status" = "200" ] || { echo "FAIL: /v1/sync/artifacts (rule) status $sync_rule_status: $sync_rule_body"; exit 1; }
echo "$sync_rule_body" | grep -q '"name":"smoke-rule"' || { echo "FAIL: approved smoke-rule missing from sync: $sync_rule_body"; exit 1; }
echo "$sync_rule_body" | grep -q '"type":"rule"' || { echo "FAIL: smoke-rule not type:rule in sync: $sync_rule_body"; exit 1; }
echo "$sync_rule_body" | grep -q 'RULE-BODY-NO-SECRETS' || { echo "FAIL: rule verbatim content missing from sync: $sync_rule_body"; exit 1; }
echo "    smoke-rule present in sync post-approval (type:rule, verbatim content)"

# ── orbeat-sync BINARY gate: the API→client seam (the v1.14.0 hole) ────────────
#
# Everything above stops at the API: it proves the server SERVES the artifacts.
# It never proved a client could CONSUME them — and that is exactly where v1.14.0
# shipped a dead feature CI-green (`reconcile: unknown artifact type "rule"`
# aborted the whole sync, so rules never distributed AND skills/subagents stopped
# syncing for any rule-entitled user). Per-reconciler unit tests missed it because
# each was tested only with its own types, never the production mix.
#
# So drive the REAL binary against this live stack, with the mix that broke it:
# alice is entitled to BOTH a rule (smoke-rule) and a subagent (smoke-gov).
echo "==> orbeat-sync binary gate: the real client consumes what the API just served"

# Build into a throwaway dir (registered in the cleanup trap), not the
# developer's ./bin — a smoke run shouldn't leave a stray binary behind.
SYNC_BIN=$(mktemp -d)
go build -o "$SYNC_BIN/orbeat-sync" ./cmd/sync
SYNC_HOME=$(mktemp -d)
SYNC_PROJ=$(mktemp -d)
mkdir -p "$SYNC_HOME/.claude" "$SYNC_HOME/.config/orbeat"

seed_sync_token "$SYNC_HOME"

# A dev's own content, to prove the managed block never clobbers it.
printf '# Smoke project notes\nPre-existing dev content.\n' > "$SYNC_PROJ/AGENTS.md"

HOME="$SYNC_HOME" "$SYNC_BIN/orbeat-sync" project add "$SYNC_PROJ" >/dev/null

set +e
sync_out=$(HOME="$SYNC_HOME" "$SYNC_BIN/orbeat-sync" sync 2>&1)
sync_rc=$?
set -e
echo "$sync_out" | sed 's/^/    | /'
[ "$sync_rc" = "0" ] || { echo "FAIL: orbeat-sync sync exited $sync_rc (want 0); output above"; exit 1; }
echo "    orbeat-sync sync => exit 0"

# The rule reached the project's AGENTS.md, and the dev's own content survived.
grep -q 'ORBEAT-RULES:BEGIN' "$SYNC_PROJ/AGENTS.md" \
  || { echo "FAIL: no ORBEAT-RULES block in $SYNC_PROJ/AGENTS.md"; exit 1; }
grep -q 'RULE-BODY-NO-SECRETS' "$SYNC_PROJ/AGENTS.md" \
  || { echo "FAIL: rule content missing from $SYNC_PROJ/AGENTS.md"; exit 1; }
grep -q 'Pre-existing dev content' "$SYNC_PROJ/AGENTS.md" \
  || { echo "FAIL: the dev's own AGENTS.md content was clobbered"; exit 1; }
grep -q '@AGENTS.md' "$SYNC_PROJ/CLAUDE.md" \
  || { echo "FAIL: no @AGENTS.md import in $SYNC_PROJ/CLAUDE.md"; exit 1; }
echo "    rule distributed to AGENTS.md + CLAUDE.md import; dev content preserved"

# THE v1.14.0 REGRESSION ASSERTION: a file-backed artifact must ALSO land. Under
# the v1.14.0 bug this is where it died — the rule aborted the sync before the
# subagent was ever written.
test -f "$SYNC_HOME/.claude/agents/smoke-gov.md" \
  || { echo "FAIL: subagent smoke-gov.md not written — a rule-entitled sync must still deliver file-backed artifacts (this is the v1.14.0 defect)"; exit 1; }
echo "    file-backed subagent delivered alongside the rule (the v1.14.0 regression)"

# --json must put exactly one parseable object on STDOUT and nothing else. The
# Go tests render into a bytes.Buffer, so they structurally cannot see a stray
# write to os.Stdout (measured: a stray fmt.Println leaves all 21 green). Only a
# real binary run can catch it.
sync_json=$(HOME="$SYNC_HOME" "$SYNC_BIN/orbeat-sync" sync --json 2>/dev/null)
echo "$sync_json" | jq -e . >/dev/null || { echo "FAIL: sync --json did not emit valid JSON on stdout:"; echo "$sync_json"; exit 1; }
json_exit=$(echo "$sync_json" | jq -r '.exitCode')
[ "$json_exit" = "0" ] || { echo "FAIL: sync --json reported exitCode=$json_exit, want 0 on a clean run"; exit 1; }
echo "$sync_json" | jq -e 'has("artifacts") and has("seeds") and has("rules")' >/dev/null \
  || { echo "FAIL: sync --json payload is missing a top-level section"; exit 1; }
echo "    orbeat-sync sync --json emitted one parseable object with exitCode 0"

# Idempotent re-run stays clean (exit 0, no thrash).
set +e
HOME="$SYNC_HOME" "$SYNC_BIN/orbeat-sync" sync >/dev/null 2>&1
resync_rc=$?
set -e
[ "$resync_rc" = "0" ] || { echo "FAIL: idempotent re-sync exited $resync_rc (want 0)"; exit 1; }
echo "    idempotent re-sync => exit 0"

# Partial-failure contract (v1.15.0): a broken project must not abort the sync —
# it is reported and the process exits 1, while healthy work still lands.
SYNC_BAD=$(mktemp -d)
mkdir -p "$SYNC_BAD/AGENTS.md"   # a directory where the file must go => I/O failure
HOME="$SYNC_HOME" "$SYNC_BIN/orbeat-sync" project add "$SYNC_BAD" >/dev/null
set +e
partial_out=$(HOME="$SYNC_HOME" "$SYNC_BIN/orbeat-sync" sync 2>&1)
partial_rc=$?
set -e
[ "$partial_rc" = "1" ] || { echo "FAIL: a per-project I/O failure must exit 1 (partial), got $partial_rc; output: $partial_out"; exit 1; }
echo "$partial_out" | grep -q 'failed:' \
  || { echo "FAIL: a failed project must print a 'failed:' line; output: $partial_out"; exit 1; }
grep -q 'ORBEAT-RULES:BEGIN' "$SYNC_PROJ/AGENTS.md" \
  || { echo "FAIL: the healthy project lost its block when another project failed"; exit 1; }
echo "    partial failure => exit 1 + 'failed:' line, healthy project unaffected"

# F1 — the partial-failure contract in MACHINE-READABLE form. The run above proves
# the exit code and the human line; this proves --json describes the same run.
# Assert the process rc AND .exitCode together: the rc alone passes a mutant that
# stops emitting JSON, and .exitCode alone passes a mutant that hardcodes os.Exit.
set +e
partial_json=$(HOME="$SYNC_HOME" "$SYNC_BIN/orbeat-sync" sync --json 2>/dev/null)
partial_json_rc=$?
set -e
[ "$partial_json_rc" = "1" ] || { echo "FAIL: sync --json on a broken project exited $partial_json_rc, want 1"; exit 1; }
echo "$partial_json" | jq -e . >/dev/null || { echo "FAIL: sync --json emitted no parseable JSON on a FAILING run:"; echo "$partial_json"; exit 1; }
[ "$(echo "$partial_json" | jq -r '.exitCode')" = "1" ] \
  || { echo "FAIL: sync --json reported exitCode $(echo "$partial_json" | jq -r '.exitCode'), want 1"; exit 1; }
echo "$partial_json" | jq -e '.fatal == null' >/dev/null \
  || { echo "FAIL: a partial failure must not set .fatal; got $(echo "$partial_json" | jq -c '.fatal')"; exit 1; }
# Pin the SECTION and the CONTENT. `length >= 1` passes on [""], which would let a
# mutant drop the operator's only clue about which project broke.
echo "$partial_json" | jq -e '.rules.failures | any(contains("rules: write"))' >/dev/null \
  || { echo "FAIL: the broken project must be reported in .rules.failures; got $(echo "$partial_json" | jq -c '.rules.failures')"; exit 1; }
echo "$partial_json" | jq -e '.seeds.failures | length == 0' >/dev/null \
  || { echo "FAIL: .seeds.failures must be empty; got $(echo "$partial_json" | jq -c '.seeds.failures')"; exit 1; }
echo "$partial_json" | jq -e '.artifacts.failures | length == 0' >/dev/null \
  || { echo "FAIL: .artifacts.failures must be empty; got $(echo "$partial_json" | jq -c '.artifacts.failures')"; exit 1; }
# All three sections ran: null means "this reconciler never ran" (cmd/sync/outcome.go:16-20),
# which is the fatal signature, not the partial one.
echo "$partial_json" | jq -e '.artifacts != null and .seeds != null and .rules != null' >/dev/null \
  || { echo "FAIL: a partial run must have run all three reconcilers; got $(echo "$partial_json" | jq -c '{artifacts,seeds,rules}')"; exit 1; }
echo "    partial failure --json => exitCode 1, .rules.failures names it, all sections ran"

# F2 — the failed unit is RETRIED once the condition clears. Repair the project
# instead of deleting it: rmdir is enough because the failure is on the READ
# (mergeRulesFile reads before writing, internal/syncclient/rules.go:329-334), so
# nothing was ever written into the directory, and writeRulesToProject returns on
# the first file so CLAUDE.md was never created either.
rmdir "$SYNC_BAD/AGENTS.md" || { echo "FAIL: could not rmdir the placeholder — something wrote into it"; exit 1; }
set +e
retry_out=$(HOME="$SYNC_HOME" "$SYNC_BIN/orbeat-sync" sync 2>&1)
retry_rc=$?
set -e
[ "$retry_rc" = "0" ] || { echo "FAIL: after repairing the project, sync exited $retry_rc (want 0); output: $retry_out"; exit 1; }
grep -q 'ORBEAT-RULES:BEGIN' "$SYNC_BAD/AGENTS.md" \
  || { echo "FAIL: the repaired project has no ORBEAT-RULES block — the failed unit was not retried"; exit 1; }
# The BODY, not just the markers: a mutant writing markers around an empty payload
# would otherwise report a successful retry that delivered nothing.
grep -q 'RULE-BODY-NO-SECRETS' "$SYNC_BAD/AGENTS.md" \
  || { echo "FAIL: the repaired project has the block but not the rule body"; exit 1; }
# CLAUDE.md's import is what makes Claude Code actually read AGENTS.md.
grep -q '@AGENTS.md' "$SYNC_BAD/CLAUDE.md" \
  || { echo "FAIL: the repaired project has no @AGENTS.md import in CLAUDE.md"; exit 1; }
echo "    repaired project retried on the next run => exit 0, block + body + import"

# ── Fatal-abort scenarios (exit 2) ─────────────────────────────────────────────
#
# Each runs against its OWN copy of the sync home. A copied home still points
# projects.json at the LIVE projects — the registered roots live outside HOME — so
# rewrite it to a throwaway project, or a fatal run could mutate the healthy state
# asserted above. The fresh projects exist for that isolation only; they are not an
# assertion surface (cmd/sync/main_test.go covers "no block after a fatal abort" in
# Go, and as a bare negative here it would pass on a binary that did nothing).
#
# Use `cp -a "$SRC/." "$DST/"`, never `cp -a "$SRC" "$DST"` — the latter creates
# $DST/<basename>, and the basename differs between GNU and BSD mktemp.
SYNC_FATAL_A=$(mktemp -d); SYNC_FRESH_A=$(mktemp -d)
cp -a "$SYNC_HOME/." "$SYNC_FATAL_A/"
printf '{"projects":["%s"]}\n' "$SYNC_FRESH_A" > "$SYNC_FATAL_A/.config/orbeat/projects.json"
seed_sync_token "$SYNC_FATAL_A"

# F3 — a corrupt manifest aborts BEFORE any write. loadManifest is the first
# executable statement in Reconcile (internal/syncclient/reconcile.go), so nothing
# is written at all.
#
# Delete the artifact FIRST. Without that, "the file is still there" is true whether
# the abort happened or not, and the assertion is vacuous — the same trap this whole
# gate exists to catch.
rm "$SYNC_FATAL_A/.claude/agents/smoke-gov.md"
printf 'not json at all\n' > "$SYNC_FATAL_A/.claude/.orbeat-sync-manifest.json"
set +e
fatal_a_out=$(HOME="$SYNC_FATAL_A" "$SYNC_BIN/orbeat-sync" sync 2>&1)
fatal_a_rc=$?
set -e
[ "$fatal_a_rc" = "2" ] || { echo "FAIL: a corrupt manifest must exit 2, got $fatal_a_rc; output: $fatal_a_out"; exit 1; }
test ! -f "$SYNC_FATAL_A/.claude/agents/smoke-gov.md" \
  || { echo "FAIL: the abort was supposed to precede every write, but the artifact was written"; exit 1; }
# The healthy home is untouched: proves the copy isolated, and that F3 failed for
# the reason claimed rather than by wiping state.
test -f "$SYNC_HOME/.claude/agents/smoke-gov.md" \
  || { echo "FAIL: the fatal scenario mutated the healthy sync home"; exit 1; }
echo "    corrupt manifest => exit 2, aborted before any write, healthy home intact"

SYNC_FATAL_B=$(mktemp -d); SYNC_FRESH_B=$(mktemp -d)
cp -a "$SYNC_HOME/." "$SYNC_FATAL_B/"
printf '{"projects":["%s"]}\n' "$SYNC_FRESH_B" > "$SYNC_FATAL_B/.config/orbeat/projects.json"
seed_sync_token "$SYNC_FATAL_B"

# F4 — a traversal entry in the manifest aborts AFTER the write loop. The write loop
# precedes the remove loop, and a files entry escaping the sync root is caught by
# resolveContained in the REMOVE loop — so the artifact is rewritten and THEN the run
# aborts. Opposite on-disk signature to F3, which is why both exist.
#
# Append to the real manifest rather than replacing it: a manifest containing only the
# traversal entry leaves the artifact unmanaged, and it is skipped as a collision
# instead of being written.
#
# `.rules = []` is the other half of the isolation, and rewriting projects.json alone
# does NOT provide it. The copied manifest still lists the LIVE project roots under
# `rules`, and de-registering a project is exactly what triggers ReconcileRules' strip
# pass — which would delete the ORBEAT-RULES block out of $SYNC_PROJ and $SYNC_BAD,
# the healthy state the assertions above depend on. Today that is unreachable because
# Reconcile aborts first, but the guard must not depend on the abort's position: move
# the abort later and this scenario would start corrupting the run it shares a stack
# with.
rm "$SYNC_FATAL_B/.claude/agents/smoke-gov.md"
jq '.files += ["../../escaped.md"] | .rules = []' "$SYNC_FATAL_B/.claude/.orbeat-sync-manifest.json" > "$SYNC_FATAL_B/manifest.tmp"
mv "$SYNC_FATAL_B/manifest.tmp" "$SYNC_FATAL_B/.claude/.orbeat-sync-manifest.json"
set +e
fatal_b_json=$(HOME="$SYNC_FATAL_B" "$SYNC_BIN/orbeat-sync" sync --json 2>/dev/null)
fatal_b_rc=$?
set -e
[ "$fatal_b_rc" = "2" ] || { echo "FAIL: a path escaping the sync root must exit 2, got $fatal_b_rc"; exit 1; }
# The JSON must exist AT ALL on a failing run: renderJSON runs before `return err`,
# and a mutant lifting the error return above the render block leaves every Go test green.
echo "$fatal_b_json" | jq -e . >/dev/null || { echo "FAIL: sync --json emitted no parseable JSON on a FATAL run:"; echo "$fatal_b_json"; exit 1; }
[ "$(echo "$fatal_b_json" | jq -r '.exitCode')" = "2" ] \
  || { echo "FAIL: .exitCode was $(echo "$fatal_b_json" | jq -r '.exitCode'), want 2"; exit 1; }
echo "$fatal_b_json" | jq -e '.fatal != null' >/dev/null \
  || { echo "FAIL: a fatal run must set .fatal"; exit 1; }
# The write happened before the abort. .artifacts.updated (not .added) because the entry
# is in oldSet. FIXTURE COUPLING: this is 1 because /v1/sync/artifacts serves alice
# exactly ONE file-backed artifact (smoke-gov) — smoke-skill is never submitted and
# smoke-selfapprove is never approved, so approval gating excludes both. If someone
# approves smoke-skill, this breaks for a reason unrelated to the abort path.
[ "$(echo "$fatal_b_json" | jq -r '.artifacts.updated')" = "1" ] \
  || { echo "FAIL: .artifacts.updated was $(echo "$fatal_b_json" | jq -r '.artifacts.updated'), want 1"; exit 1; }
test -f "$SYNC_FATAL_B/.claude/agents/smoke-gov.md" \
  || { echo "FAIL: the write loop was supposed to run before the abort, but nothing was written"; exit 1; }
# null means "this reconciler never ran" — the cascade stopped.
echo "$fatal_b_json" | jq -e '.seeds == null and .rules == null' >/dev/null \
  || { echo "FAIL: seeds/rules must be null after a fatal abort; got $(echo "$fatal_b_json" | jq -c '{seeds,rules}')"; exit 1; }
echo "$fatal_b_json" | jq -e '.restartRequired == false' >/dev/null \
  || { echo "FAIL: restartRequired must be false on an aborted run"; exit 1; }
echo "    escaping path => exit 2 after the writes, cascade stopped, JSON emitted on a failing run"

# tree_digest <path>… — a stable digest of every regular file under the given paths.
# GNU coreutils ships sha256sum; BSD/macOS ships shasum. Same GNU-vs-BSD split this
# script already handles for `date` in rfc3339_in_minutes.
tree_digest() {
  local sum
  if command -v sha256sum >/dev/null 2>&1; then sum=sha256sum; else sum="shasum -a 256"; fi
  find "$@" -type f 2>/dev/null | sort | xargs $sum 2>/dev/null | $sum | cut -d' ' -f1
}

# F5 — `sync --dry-run` must change nothing. Runs against SYNC_HOME (the healthy
# home), NOT a fatal copy: both copies abort before writing anything, which would make
# every assertion here vacuous twice over.
#
# ABLATE BEFORE MEASURING. Against the steady state a real sync writes zero changed
# bytes (unchanged-file skip, equal-hash block skip, byte-identical manifest), so a
# bare checksum would pass on the very mutant this exists to catch.
rm "$SYNC_HOME/.claude/agents/smoke-gov.md"
sed -i.bak '/ORBEAT-RULES:BEGIN/,/ORBEAT-RULES:END/d' "$SYNC_PROJ/AGENTS.md" && rm -f "$SYNC_PROJ/AGENTS.md.bak"
grep -q 'ORBEAT-RULES' "$SYNC_PROJ/AGENTS.md" \
  && { echo "FAIL: the ablation did not remove the managed block — F5 would be vacuous"; exit 1; }
# $SYNC_HOME/.config/orbeat is excluded from both digests: today's --dry-run
# returns before loadValidToken, but a real one must fetch the catalog and may
# therefore refresh the access token and rewrite credentials.json there. That
# refresh is a legitimate write, not a change a sync would make, so it does not
# belong in the "nothing changed" claim — the sync root and both project trees
# below carry the actual claim.
dry_before=$(tree_digest "$SYNC_HOME/.claude" "$SYNC_PROJ" "$SYNC_BAD")
set +e
dry_out=$(HOME="$SYNC_HOME" "$SYNC_BIN/orbeat-sync" sync --dry-run 2>&1)
dry_rc=$?
set -e
dry_after=$(tree_digest "$SYNC_HOME/.claude" "$SYNC_PROJ" "$SYNC_BAD")
# THE DURABLE ASSERTION: true today (the flag errors before touching anything) and
# still true the day a real --dry-run ships, because a preview writes nothing.
[ "$dry_before" = "$dry_after" ] \
  || { echo "FAIL: sync --dry-run modified files on disk; output: $dry_out"; exit 1; }
test ! -f "$SYNC_HOME/.claude/agents/smoke-gov.md" \
  || { echo "FAIL: sync --dry-run re-created the ablated artifact — it performed a REAL sync"; exit 1; }
# The digest assertions above do NOT invert: a preview writes nothing, before or after
# this feature shipped. Only the two assertions below did.
#
# $dry_out is captured with 2>&1, which is now belt-and-braces rather than necessary:
# renderHuman writes the header and the plan to STDOUT, and main.go reaches stderr only
# on a non-nil error, which a successful dry run no longer produces. Merging stderr
# still costs nothing and keeps a diagnostic visible if the run does fail. (It was
# genuinely required when --dry-run errored: the message went to stderr and runSync
# returned before any renderer touched stdout.)
[ "$dry_rc" = "0" ] || { echo "FAIL: sync --dry-run must succeed now that the feature has shipped, got exit $dry_rc"; exit 1; }
echo "$dry_out" | grep -q 'DRY RUN' \
  || { echo "FAIL: sync --dry-run did not print the DRY RUN header; output: $dry_out"; exit 1; }
# Without this, an EMPTY plan passes every assertion above: the digest is unchanged
# and the ablated file stays absent precisely because nothing was planned. This is
# what makes the red-proof in step 5 fail for the right reason.
echo "$dry_out" | grep -q 'smoke-gov.md' \
  || { echo "FAIL: the plan did not name the ablated artifact it would recreate; output: $dry_out"; exit 1; }
echo "    sync --dry-run => succeeds, names the ablated artifact in its plan, and changed nothing on disk (ablated first)"

# ── doctor sanity check ─────────────────────────────────────────────────────
#
# F5 deliberately leaves SYNC_HOME ablated on exit — it ran last, and nothing
# downstream needed smoke-gov.md or the ORBEAT-RULES block restored. Asking
# doctor anything here without repairing that first would report findings F5
# caused, not anything doctor exists to catch. Costs no extra stack cycle:
# the stack is already up.
set +e
restore_out=$(HOME="$SYNC_HOME" "$SYNC_BIN/orbeat-sync" sync 2>&1)
restore_rc=$?
set -e
[ "$restore_rc" = "0" ] \
  || { echo "FAIL: restoring sync after F5's ablation exited $restore_rc (want 0); output: $restore_out"; exit 1; }

# D1 — on the healthy tree the gate has already built, doctor (offline,
# read-only, always exit 0) reports no problems. Use --json + jq for the
# count rather than grepping prose: the script already relies on jq
# unconditionally in this block, and a prose grep would break the day
# doctor's wording changes.
set +e
doctor_json=$(HOME="$SYNC_HOME" "$SYNC_BIN/orbeat-sync" doctor --json 2>&1)
doctor_rc=$?
set -e
[ "$doctor_rc" = "0" ] \
  || { echo "FAIL: orbeat-sync doctor exited $doctor_rc (want 0); output: $doctor_json"; exit 1; }
echo "$doctor_json" | jq -e . >/dev/null \
  || { echo "FAIL: doctor --json emitted no parseable JSON: $doctor_json"; exit 1; }
doctor_problems=$(echo "$doctor_json" | jq -r '.problems')
[ "$doctor_problems" = "0" ] \
  || { echo "FAIL: doctor reported $doctor_problems problem(s) on the healthy tree: $doctor_json"; exit 1; }
echo "    orbeat-sync doctor --json => exit 0, .problems == 0 on the restored, healthy tree"

echo "==> SMOKE PASS: api + gateway healthy, postgres up, Keycloak auth /v1/me 200 + no-token 401, /v1/catalog 200 + no-token 401, admin CRUD + entitlement→catalog + 403 RBAC + audit pagination + audit export (json/csv/400) + structured JSON logs + audit dual-emit, gateway /mcp 401 no-token + metadata + real upstream tool round-trip (smoke-upstream__echo), portal healthz + SPA shell, marketplace artifact validated, artifact publish round-trip, sync/config 200 + no-token 401, artifact approval gate (unapproved hidden from sync, boss2 approval distributes, self-approve 403), artifact rollback (v2 approved+distributed, roll back to rev 1 restores v1 content), revision pruning gate (KEEP=$ORBEAT_ARTIFACT_REVISION_KEEP enforced end-to-end through cmd/api: $PRUNE_APPROVALS approvals on a dedicated throwaway artifact leave exactly $ORBEAT_ARTIFACT_REVISION_KEEP revisions), rule artifact gate (Phase 3 Slice B: create → submit → boss2 approve → entitled rule present in sync with type:rule + verbatim content), orbeat-sync BINARY gate (the real client reconciles the live stack's artifacts: rule → AGENTS.md + CLAUDE.md import with dev content preserved, file-backed subagent delivered alongside it — the v1.14.0 regression — idempotent re-sync, and the v1.15.0 partial-failure contract: a broken project exits 1 with a 'failed:' line while healthy work still lands, plus the failure-path contract (partial --json with section-pinned failures, a repaired project retried, a corrupt manifest exiting 2 before any write, an escaping path exiting 2 after the writes with the cascade stopped and JSON still emitted, and --dry-run changing nothing), and orbeat-sync doctor --json on the restored healthy tree reporting exit 0 with zero problems)"
