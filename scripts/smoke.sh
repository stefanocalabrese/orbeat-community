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

# Artifact deployment registry (docs/specs/2026-08-22-orbeat-artifact-
# deployment-registry-design.md, gates G1-G4). Exported BEFORE the stack comes
# up so it flows through the compose file's ${ORBEAT_DEPLOYMENT_REGISTRY:-}
# passthrough into cmd/api, exactly like the revision cap above. `make up` and
# the Playwright e2e job share that compose file WITHOUT this export, so they
# keep the shipped default: off, no report route, nothing collected.
#
# It is on HERE and only here because this script is the only place a client
# report can be observed end to end. Everything else stops at a boundary the
# defect can hide behind: the API tests never build the applied set, the
# syncclient tests never cross the wire, and a runbook that has not been run
# catches nothing (v1.14.0 shipped a dead feature green behind three such
# controls). The negotiation path this run therefore no longer exercises (the
# client asking a server that answers false and sending nothing) is covered in
# Go by internal/api's off-by-default gate and cmd/orbeat-sync's report tests.
export ORBEAT_DEPLOYMENT_REGISTRY=true

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
  [ -n "${SYNC_PROJ_UNTAGGED:-}" ] && rm -rf "$SYNC_PROJ_UNTAGGED"
  [ -n "${SYNC_BIN:-}" ] && rm -rf "$SYNC_BIN"
  [ -n "${SYNC_BAD:-}" ] && rm -rf "$SYNC_BAD"
  [ -n "${SYNC_FATAL_A:-}" ] && rm -rf "$SYNC_FATAL_A"
  [ -n "${SYNC_FATAL_B:-}" ] && rm -rf "$SYNC_FATAL_B"
  [ -n "${SYNC_FRESH_A:-}" ] && rm -rf "$SYNC_FRESH_A"
  [ -n "${SYNC_FRESH_B:-}" ] && rm -rf "$SYNC_FRESH_B"
  [ -n "${SYNC_G2:-}" ] && rm -rf "$SYNC_G2"
  [ -n "${SYNC_G2_PROJ:-}" ] && rm -rf "$SYNC_G2_PROJ"
  [ -n "${SYNC_PIN:-}" ] && rm -rf "$SYNC_PIN"
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

# deployments_json <token> <artifact-id>: the aggregate deployment body for one
# artifact (GET /v1/admin/artifacts/{id}/deployments), guarded so a 401, a 404 or
# an unregistered route fails HERE with the body printed, rather than feeding
# `null` into a jq comparison twenty lines later that then reads as a registry
# bug. jq is used unqualified, as it already is throughout the sync gates below.
#
# The guard checks a field every response carries, not `.installs != null`: the
# interesting answers are zeroes, and a check that a zero satisfies is a check
# that an absent body also satisfies.
deployments_json() {
  local tok="$1" aid="$2" body
  body=$(curl -s -H "Authorization: Bearer $tok" "http://localhost:8080/v1/admin/artifacts/$aid/deployments")
  echo "$body" | jq -e 'has("installs") and has("latestRevision") and has("byRevision") and has("observable")' >/dev/null \
    || { echo "FAIL: no deployment aggregate for $aid: $body" >&2; exit 1; }
  echo "$body"
}

# latest_revision_num <token> <artifact-id>: MAX(revision) as the artifact's own
# revision history reports it (GET /v1/admin/artifacts/{id}/revisions).
#
# EVERY REVISION ASSERTION IN THIS FILE IS DERIVED THROUGH HERE, NEVER WRITTEN AS
# A LITERAL. What that buys, precisely: the expected number changes when the
# artifact's history changes, so a registry storing a constant fails the moment
# the fixture approves anything, and the assertion cannot be quietly "fixed" by
# editing a number to match what the code did.
#
# What it does NOT buy, measured rather than assumed: it does not catch a server
# that stores MAX(revision_num) instead of the value it was sent. The client is
# only ever SERVED MAX, so the two agree on every report a real binary can
# produce. That mutant was run through this whole script and passed. G1b exists
# for it, and needs a caller the real client cannot be.
#
# `max` over the list rather than the first element: the ordering is the
# endpoint's business, and this gate must not silently start measuring something
# else the day it changes.
latest_revision_num() {
  local tok="$1" aid="$2" body n
  body=$(curl -s -H "Authorization: Bearer $tok" "http://localhost:8080/v1/admin/artifacts/$aid/revisions")
  n=$(echo "$body" | jq -r '[.revisions[].revision] | max')
  [ -n "$n" ] && [ "$n" != "null" ] || { echo "FAIL: could not resolve a revision number for $aid: $body" >&2; exit 1; }
  echo "$n"
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
# down -v FIRST, unconditionally. This script's fixtures are not idempotent
# against a populated database: the very first admin create asserts 201, and a
# leftover smoke-github row makes it a 409 on the slug-collision check added in
# v1.17.0. Measured 2026-08-23: a `git push` killed mid-hook left a stack
# running, and the next push's smoke job failed on exactly that 409, a red that
# described nothing about the code. The converse is worse and untested, since a
# stale row could equally let an assertion pass that should have failed.
# Costs nothing on a clean host or in CI, where there is no stack to remove.
$COMPOSE down -v >/dev/null 2>&1 || true
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
# failure returns before the render block (cmd/orbeat-sync/main.go:249-252), so the run
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

# ── Org-visibility rule gate ───────────────────────────────────────────────────
# A rule is the one artifact type with no Channel 1: marketplace.RenderArtifactsPlugin
# has no `rule` case. So before ListActiveOrgRules an org-visibility rule was dropped
# by Channel 1 and excluded from Channel 2 for being org visibility, and reached
# NOBODY, starting from the default value of `visibility`. This block creates one with
# NO entitlement at all: alice can only receive it if org rules are universal.
echo "==> admin creates an ORG-visibility rule (no entitlement, reaches everyone or nobody)"
org_rule_resp=$(curl -s -w '\n%{http_code}' -X POST \
  -H "Authorization: Bearer $ADMIN_TOKEN" -H 'Content-Type: application/json' \
  -d '{"type":"rule","name":"smoke-org-rule","description":"d","content":"Everyone follows this. ORG-RULE-BODY-EVERYONE","visibility":"org"}' \
  http://localhost:8080/v1/admin/artifacts)
org_rule_status=$(printf '%s' "$org_rule_resp" | tail -n1)
org_rule_body=$(printf '%s' "$org_rule_resp" | sed '$d')
[ "$org_rule_status" = "201" ] || { echo "FAIL: create org rule $org_rule_status: $org_rule_body"; exit 1; }
if command -v jq >/dev/null 2>&1; then
  ORG_RULE_ID=$(echo "$org_rule_body" | jq -r '.id')
else
  ORG_RULE_ID=$(echo "$org_rule_body" | grep -o '"id":"[^"]*"' | head -n1 | sed 's/"id":"//;s/"//')
fi
[ -n "$ORG_RULE_ID" ] || { echo "FAIL: could not resolve org rule id: $org_rule_body"; exit 1; }
org_rule_submit_status=$(curl -s -o /dev/null -w '%{http_code}' -X POST \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  "http://localhost:8080/v1/admin/artifacts/$ORG_RULE_ID/submit")
[ "$org_rule_submit_status" = "200" ] || { echo "FAIL: submit smoke-org-rule status $org_rule_submit_status"; exit 1; }
ORG_RULE_RV=$(current_row_version "$ADMIN_TOKEN" "$ORG_RULE_ID")
org_rule_approve_status=$(curl -s -o /dev/null -w '%{http_code}' -X POST \
  -H "Authorization: Bearer $BOSS2_TOKEN" -H "If-Match: \"$ORG_RULE_RV\"" \
  "http://localhost:8080/v1/admin/artifacts/$ORG_RULE_ID/approve")
[ "$org_rule_approve_status" = "200" ] || { echo "FAIL: approve smoke-org-rule (boss2) status $org_rule_approve_status"; exit 1; }
echo "    smoke-org-rule created, submitted, approved, and entitled to NOBODY"

# The control for the assertion further down. smoke-skill (created ~line 479) is
# never submitted, so asserting IT is absent from sync proves nothing: approval
# gating already excludes it. This one is approved, so its absence from Channel 2
# can only be the visibility rule doing its job.
echo "==> admin creates and approves an ORG-visibility SKILL (must stay on Channel 1 only)"
org_skill_resp=$(curl -s -w '\n%{http_code}' -X POST \
  -H "Authorization: Bearer $ADMIN_TOKEN" -H 'Content-Type: application/json' \
  -d '{"type":"skill","name":"smoke-org-skill","description":"d","content":"---\nname: smoke-org-skill\ndescription: d\n---\nORG-SKILL-BODY-CHANNEL1","visibility":"org"}' \
  http://localhost:8080/v1/admin/artifacts)
org_skill_status=$(printf '%s' "$org_skill_resp" | tail -n1)
org_skill_body=$(printf '%s' "$org_skill_resp" | sed '$d')
[ "$org_skill_status" = "201" ] || { echo "FAIL: create org skill $org_skill_status: $org_skill_body"; exit 1; }
if command -v jq >/dev/null 2>&1; then
  ORG_SKILL_ID=$(echo "$org_skill_body" | jq -r '.id')
else
  ORG_SKILL_ID=$(echo "$org_skill_body" | grep -o '"id":"[^"]*"' | head -n1 | sed 's/"id":"//;s/"//')
fi
[ -n "$ORG_SKILL_ID" ] || { echo "FAIL: could not resolve org skill id: $org_skill_body"; exit 1; }
org_skill_submit_status=$(curl -s -o /dev/null -w '%{http_code}' -X POST \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  "http://localhost:8080/v1/admin/artifacts/$ORG_SKILL_ID/submit")
[ "$org_skill_submit_status" = "200" ] || { echo "FAIL: submit smoke-org-skill status $org_skill_submit_status"; exit 1; }
ORG_SKILL_RV=$(current_row_version "$ADMIN_TOKEN" "$ORG_SKILL_ID")
org_skill_approve_status=$(curl -s -o /dev/null -w '%{http_code}' -X POST \
  -H "Authorization: Bearer $BOSS2_TOKEN" -H "If-Match: \"$ORG_SKILL_RV\"" \
  "http://localhost:8080/v1/admin/artifacts/$ORG_SKILL_ID/approve")
[ "$org_skill_approve_status" = "200" ] || { echo "FAIL: approve smoke-org-skill (boss2) status $org_skill_approve_status"; exit 1; }
echo "    smoke-org-skill approved (Channel 1 only)"

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

# The decisive one: alice holds no grant on this artifact, so it can only be here
# because org rules are universal on this channel.
echo "$sync_rule_body" | grep -q '"name":"smoke-org-rule"' \
  || { echo "FAIL: approved ORG-visibility rule missing from sync (it reaches nobody): $sync_rule_body"; exit 1; }
echo "$sync_rule_body" | grep -q 'ORG-RULE-BODY-EVERYONE' \
  || { echo "FAIL: org rule verbatim content missing from sync: $sync_rule_body"; exit 1; }
# An APPROVED org skill must still NOT be here: it has a Channel 1, and duplicating
# it would install it twice. This is what stops the fix from becoming "org
# everything", and it is keyed on smoke-org-skill rather than smoke-skill because
# smoke-skill is never submitted, so its absence would prove only that approval
# gating works.
echo "$sync_rule_body" | grep -q '"name":"smoke-org-skill"' \
  && { echo "FAIL: an approved org-visibility SKILL leaked onto Channel 2: $sync_rule_body"; exit 1; }
echo "    smoke-org-rule present for an unentitled user, and no org skill leaked"

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
go build -o "$SYNC_BIN/orbeat-sync" ./cmd/orbeat-sync
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
grep -q 'ORG-RULE-BODY-EVERYONE' "$SYNC_PROJ/AGENTS.md" \
  || { echo "FAIL: the org-visibility rule never reached $SYNC_PROJ/AGENTS.md via the real binary"; exit 1; }
grep -q '@AGENTS.md' "$SYNC_PROJ/CLAUDE.md" \
  || { echo "FAIL: no @AGENTS.md import in $SYNC_PROJ/CLAUDE.md"; exit 1; }
echo "    rule distributed to AGENTS.md + CLAUDE.md import; dev content preserved"

# THE v1.14.0 REGRESSION ASSERTION: a file-backed artifact must ALSO land. Under
# the v1.14.0 bug this is where it died — the rule aborted the sync before the
# subagent was ever written.
test -f "$SYNC_HOME/.claude/agents/smoke-gov.md" \
  || { echo "FAIL: subagent smoke-gov.md not written — a rule-entitled sync must still deliver file-backed artifacts (this is the v1.14.0 defect)"; exit 1; }
echo "    file-backed subagent delivered alongside the rule (the v1.14.0 regression)"

# ── Per-rule project targeting gate (migration 00024) ─────────────────────────
#
# A rule can name the PROJECT TAGS it applies to; the tags themselves are local,
# declared by the developer. The decisive shape is two projects that differ ONLY
# in their tags, plus an untargeted rule as the control: without that control, a
# missing rule in the untagged project would equally prove the project is simply
# not being managed, which is the assertion-that-cannot-fail this repo keeps
# rediscovering.
echo "==> tagging the existing project [go] and registering a second, untagged one"
HOME="$SYNC_HOME" "$SYNC_BIN/orbeat-sync" project add "$SYNC_PROJ" --tag go >/dev/null \
  || { echo "FAIL: project add --tag go"; exit 1; }
SYNC_PROJ_UNTAGGED=$(mktemp -d)
HOME="$SYNC_HOME" "$SYNC_BIN/orbeat-sync" project add "$SYNC_PROJ_UNTAGGED" >/dev/null \
  || { echo "FAIL: registering the untagged project"; exit 1; }
HOME="$SYNC_HOME" "$SYNC_BIN/orbeat-sync" project list | grep -q "$SYNC_PROJ \[go\]" \
  || { echo "FAIL: project list does not show the tag: $(HOME="$SYNC_HOME" "$SYNC_BIN/orbeat-sync" project list)"; exit 1; }

echo "==> admin creates a rule targeted at [go], entitled to orbeat-user"
tgt_resp=$(curl -s -w '\n%{http_code}' -X POST \
  -H "Authorization: Bearer $ADMIN_TOKEN" -H 'Content-Type: application/json' \
  -d '{"type":"rule","name":"smoke-targeted-rule","description":"d","content":"Go projects only. TARGETED-RULE-BODY","visibility":"role","targetTags":["go"]}' \
  http://localhost:8080/v1/admin/artifacts)
tgt_status=$(printf '%s' "$tgt_resp" | tail -n1)
tgt_body=$(printf '%s' "$tgt_resp" | sed '$d')
[ "$tgt_status" = "201" ] || { echo "FAIL: create targeted rule $tgt_status: $tgt_body"; exit 1; }
if command -v jq >/dev/null 2>&1; then
  TGT_ID=$(echo "$tgt_body" | jq -r '.id')
else
  TGT_ID=$(echo "$tgt_body" | grep -o '"id":"[^"]*"' | head -n1 | sed 's/"id":"//;s/"//')
fi
[ -n "$TGT_ID" ] || { echo "FAIL: could not resolve targeted rule id: $tgt_body"; exit 1; }
tgt_ent_status=$(curl -s -o /dev/null -w '%{http_code}' -X POST \
  -H "Authorization: Bearer $ADMIN_TOKEN" -H 'Content-Type: application/json' \
  -d "{\"roleId\":\"$ROLE_ID\",\"artifactId\":\"$TGT_ID\"}" \
  http://localhost:8080/v1/admin/artifact-entitlements)
case "$tgt_ent_status" in 201|409) ;; *) echo "FAIL: entitle targeted rule status $tgt_ent_status"; exit 1 ;; esac
tgt_submit_status=$(curl -s -o /dev/null -w '%{http_code}' -X POST \
  -H "Authorization: Bearer $ADMIN_TOKEN" "http://localhost:8080/v1/admin/artifacts/$TGT_ID/submit")
[ "$tgt_submit_status" = "200" ] || { echo "FAIL: submit targeted rule status $tgt_submit_status"; exit 1; }
TGT_RV=$(current_row_version "$ADMIN_TOKEN" "$TGT_ID")
tgt_approve_status=$(curl -s -o /dev/null -w '%{http_code}' -X POST \
  -H "Authorization: Bearer $BOSS2_TOKEN" -H "If-Match: \"$TGT_RV\"" \
  "http://localhost:8080/v1/admin/artifacts/$TGT_ID/approve")
[ "$tgt_approve_status" = "200" ] || { echo "FAIL: approve targeted rule status $tgt_approve_status"; exit 1; }

echo "==> sync: the targeted rule reaches ONLY the [go] project, the untargeted one reaches both"
HOME="$SYNC_HOME" "$SYNC_BIN/orbeat-sync" sync >/dev/null 2>&1 \
  || { echo "FAIL: sync after targeting exited non-zero"; exit 1; }
grep -q 'TARGETED-RULE-BODY' "$SYNC_PROJ/AGENTS.md" \
  || { echo "FAIL: the [go]-targeted rule never reached the tagged project"; exit 1; }
grep -q 'TARGETED-RULE-BODY' "$SYNC_PROJ_UNTAGGED/AGENTS.md" 2>/dev/null \
  && { echo "FAIL: a [go]-targeted rule reached an UNTAGGED project"; exit 1; }
# The control. Without it, the assertion above passes on a project that is not
# being managed at all.
grep -q 'RULE-BODY-NO-SECRETS' "$SYNC_PROJ_UNTAGGED/AGENTS.md" \
  || { echo "FAIL: the untargeted rule never reached the untagged project, so its lack of the targeted rule proves nothing"; exit 1; }
echo "    targeting held: tagged project has both rules, untagged project has only the untargeted one"

# ── Global-scope rule gate (migration 00025) ─────────────────────────────────
#
# A global rule belongs in the user-level instruction files every project
# inherits, not in any project. The decisive pair is again both directions: it
# must appear in ~/.claude/CLAUDE.md AND be absent from the project's AGENTS.md,
# because a client that wrote every rule everywhere would satisfy the first half.
echo "==> admin creates a GLOBAL-scope rule entitled to orbeat-user"
glob_resp=$(curl -s -w '\n%{http_code}' -X POST \
  -H "Authorization: Bearer $ADMIN_TOKEN" -H 'Content-Type: application/json' \
  -d '{"type":"rule","name":"smoke-global-rule","description":"d","content":"Ask before force-pushing. GLOBAL-RULE-BODY","visibility":"role","ruleScope":"global"}' \
  http://localhost:8080/v1/admin/artifacts)
glob_status=$(printf '%s' "$glob_resp" | tail -n1)
glob_body=$(printf '%s' "$glob_resp" | sed '$d')
[ "$glob_status" = "201" ] || { echo "FAIL: create global rule $glob_status: $glob_body"; exit 1; }
if command -v jq >/dev/null 2>&1; then
  GLOB_ID=$(echo "$glob_body" | jq -r '.id')
else
  GLOB_ID=$(echo "$glob_body" | grep -o '"id":"[^"]*"' | head -n1 | sed 's/"id":"//;s/"//')
fi
[ -n "$GLOB_ID" ] || { echo "FAIL: could not resolve global rule id: $glob_body"; exit 1; }
# A global rule with target tags is refused: tags select projects and a global
# rule is written into none.
glob_bad_status=$(curl -s -o /dev/null -w '%{http_code}' -X POST \
  -H "Authorization: Bearer $ADMIN_TOKEN" -H 'Content-Type: application/json' \
  -d '{"type":"rule","name":"smoke-bad-global","description":"d","content":"x","ruleScope":"global","targetTags":["go"]}' \
  http://localhost:8080/v1/admin/artifacts)
[ "$glob_bad_status" = "400" ] || { echo "FAIL: a global rule with targetTags returned $glob_bad_status, want 400"; exit 1; }
glob_ent_status=$(curl -s -o /dev/null -w '%{http_code}' -X POST \
  -H "Authorization: Bearer $ADMIN_TOKEN" -H 'Content-Type: application/json' \
  -d "{\"roleId\":\"$ROLE_ID\",\"artifactId\":\"$GLOB_ID\"}" \
  http://localhost:8080/v1/admin/artifact-entitlements)
case "$glob_ent_status" in 201|409) ;; *) echo "FAIL: entitle global rule status $glob_ent_status"; exit 1 ;; esac
glob_submit_status=$(curl -s -o /dev/null -w '%{http_code}' -X POST \
  -H "Authorization: Bearer $ADMIN_TOKEN" "http://localhost:8080/v1/admin/artifacts/$GLOB_ID/submit")
[ "$glob_submit_status" = "200" ] || { echo "FAIL: submit global rule status $glob_submit_status"; exit 1; }
GLOB_RV=$(current_row_version "$ADMIN_TOKEN" "$GLOB_ID")
glob_approve_status=$(curl -s -o /dev/null -w '%{http_code}' -X POST \
  -H "Authorization: Bearer $BOSS2_TOKEN" -H "If-Match: \"$GLOB_RV\"" \
  "http://localhost:8080/v1/admin/artifacts/$GLOB_ID/approve")
[ "$glob_approve_status" = "200" ] || { echo "FAIL: approve global rule status $glob_approve_status"; exit 1; }

echo "==> sync: the global rule lands in the user-level file, never in a project"
HOME="$SYNC_HOME" "$SYNC_BIN/orbeat-sync" sync >/dev/null 2>&1 \
  || { echo "FAIL: sync after adding a global rule exited non-zero"; exit 1; }
grep -q 'GLOBAL-RULE-BODY' "$SYNC_HOME/.claude/CLAUDE.md" \
  || { echo "FAIL: the global rule never reached $SYNC_HOME/.claude/CLAUDE.md"; exit 1; }
grep -q 'GLOBAL-RULE-BODY' "$SYNC_PROJ/AGENTS.md" \
  && { echo "FAIL: a global rule was ALSO written into a project, so it applies twice"; exit 1; }
# The control: the project rule is still in the project, so the absence above is
# scope doing its job rather than the project having stopped being managed.
grep -q 'RULE-BODY-NO-SECRETS' "$SYNC_PROJ/AGENTS.md" \
  || { echo "FAIL: the project rule vanished, so the global-rule assertion proves nothing"; exit 1; }
echo "    global rule in ~/.claude/CLAUDE.md, absent from the project, project rule still in place"

# Put the fixture back: later gates in this script assume one registered project
# with no tags. Removing strips the managed blocks from the throwaway directory,
# which is also the documented behaviour of `project remove`.
HOME="$SYNC_HOME" "$SYNC_BIN/orbeat-sync" project remove "$SYNC_PROJ_UNTAGGED" >/dev/null \
  || { echo "FAIL: deregistering the untagged project"; exit 1; }
HOME="$SYNC_HOME" "$SYNC_BIN/orbeat-sync" project add "$SYNC_PROJ" >/dev/null \
  || { echo "FAIL: clearing the tag from the primary project"; exit 1; }
rm -rf "$SYNC_PROJ_UNTAGGED"

# ── G1a: the deployment registry recorded what the real binary just applied ───
#
# The registry is on for this run (ORBEAT_DEPLOYMENT_REGISTRY, exported at the
# top of this file). One sync has happened, from one machine, so the aggregate
# for smoke-gov must be exactly one install.
#
# ASSERT THE NUMBER, NOT THE PRESENCE. "A row landed" is the easy assertion and
# it passes on a registry recording the wrong version, which is the way this
# feature is most likely to ship broken: the whole product question is "who is
# still on the old one". So the histogram is compared as a WHOLE value, with a
# revision read out of the artifact's revision history rather than typed here.
GOV_REV1=$(latest_revision_num "$ADMIN_TOKEN" "$GOV_ID")
gov_dep1=$(deployments_json "$ADMIN_TOKEN" "$GOV_ID")
echo "$gov_dep1" | jq -e '.installs == 1 and .users == 1' >/dev/null \
  || { echo "FAIL: one real sync from one machine must record 1 install / 1 user for smoke-gov; got $gov_dep1"; exit 1; }
echo "$gov_dep1" | jq -e --argjson r "$GOV_REV1" '.latestRevision == $r' >/dev/null \
  || { echo "FAIL: latestRevision is not the newest revision the history reports ($GOV_REV1); got $gov_dep1"; exit 1; }
# The whole array, not a lookup into it: `any(.revision == $r)` passes on a
# histogram that ALSO carries bars nobody reported.
echo "$gov_dep1" | jq -e --argjson r "$GOV_REV1" '.byRevision == [{"revision":$r,"installs":1}]' >/dev/null \
  || { echo "FAIL: the histogram must be exactly one install at revision $GOV_REV1; got $gov_dep1"; exit 1; }
echo "$gov_dep1" | jq -e '.behindLatest == 0 and .observable == true' >/dev/null \
  || { echo "FAIL: a machine on the newest revision is not behind, and a role-visibility artifact is observable; got $gov_dep1"; exit 1; }
echo "$gov_dep1" | jq -e '.oldestReportedAt != null' >/dev/null \
  || { echo "FAIL: a recorded install must carry a reported_at, or its count is unfalsifiable; got $gov_dep1"; exit 1; }
echo "    G1a: the real client's report landed. smoke-gov at 1 install / 1 user / revision $GOV_REV1, behindLatest 0"

# ── G1b: the number the client SENT is the number the registry STORED ─────────
#
# THE ONE THING NO REAL-BINARY GATE IN THIS FILE CAN OBSERVE, WHICH IS WHY THIS
# ONE DOES NOT USE THE BINARY. The sync payload's revision IS
# MAX(revision_num): it is a correlated subquery inside distArtifactCols
# (internal/store/artifact.go), so every report a real client can produce
# carries exactly the latest revision. A server that ignored the payload and
# stored MAX(revision_num) itself is therefore indistinguishable from a correct
# one on every other assertion here. That is measured, not argued: exactly that
# mutant was run through this whole script and it PASSED, including the
# behind-latest window below.
#
# So the report that carries a NON-latest revision has to come from a caller the
# real client cannot be. It still goes through the real route, the real router
# and alice's real token, and the divergence it creates is the whole point: a
# registry recording the wrong version is the way this feature ships broken, and
# "a row landed" cannot see it.
#
# It cleans up after itself with a second report from the same install carrying
# an EMPTY artifact list, which is how an install says it holds none of them,
# and asserts the aggregate comes back to the exact body G1a read. That equality
# is a free second red-proof of the replace's DELETE half.
G1B_INSTALL=11111111-1111-4111-8111-111111111111
g1b_post=$(curl -s -w '\n%{http_code}' -X POST \
  -H "Authorization: Bearer $ACCESS_TOKEN" -H "Content-Type: application/json" \
  -d "{\"installId\":\"$G1B_INSTALL\",\"artifacts\":[{\"artifactId\":\"$GOV_ID\",\"revision\":1}]}" \
  http://localhost:8080/v1/sync/deployments)
g1b_status=$(printf '%s' "$g1b_post" | tail -n1)
g1b_body=$(printf '%s' "$g1b_post" | sed '$d')
[ "$g1b_status" = "200" ] || { echo "FAIL: POST /v1/sync/deployments returned $g1b_status: $g1b_body"; exit 1; }
echo "$g1b_body" | jq -e '.recorded == 1 and .dropped == 0' >/dev/null \
  || { echo "FAIL: an entitled artifact at a real revision must be recorded, not dropped; got $g1b_body"; exit 1; }
gov_dep1b=$(deployments_json "$ADMIN_TOKEN" "$GOV_ID")
# The histogram must carry a bar at 1 AND a bar at $GOV_REV1. A server storing
# MAX collapses both installs onto $GOV_REV1 and reports behindLatest 0.
echo "$gov_dep1b" | jq -e --argjson r "$GOV_REV1" \
  '.installs == 2 and .byRevision == [{"revision":$r,"installs":1},{"revision":1,"installs":1}] and .behindLatest == 1' >/dev/null \
  || { echo "FAIL: a report carrying revision 1 must be stored as revision 1 and counted behind the latest; got $gov_dep1b"; exit 1; }
# Two installs, one user: the only place in this file where the two counts
# differ, which is what makes them separately meaningful rather than a pair of
# names for the same number.
echo "$gov_dep1b" | jq -e '.users == 1' >/dev/null \
  || { echo "FAIL: both installs belong to alice, so users must be 1; got $gov_dep1b"; exit 1; }
g1b_clear_status=$(curl -s -o /dev/null -w '%{http_code}' -X POST \
  -H "Authorization: Bearer $ACCESS_TOKEN" -H "Content-Type: application/json" \
  -d "{\"installId\":\"$G1B_INSTALL\",\"artifacts\":[]}" \
  http://localhost:8080/v1/sync/deployments)
[ "$g1b_clear_status" = "200" ] \
  || { echo "FAIL: clearing the throwaway install returned $g1b_clear_status"; exit 1; }
gov_dep1_again=$(deployments_json "$ADMIN_TOKEN" "$GOV_ID")
[ "$gov_dep1_again" = "$gov_dep1" ] \
  || { echo "FAIL: an empty report must clear exactly that install's rows and leave every other row alone; want $gov_dep1 got $gov_dep1_again"; exit 1; }
echo "    G1b: a report carrying revision 1 was stored as revision 1 (2 installs, 1 user, behindLatest 1), and an empty report cleared exactly that install"

# ── G2: APPLIED, not served ──────────────────────────────────────────────────
#
# A second install in a fresh HOME with agents/smoke-gov.md pre-created as an
# UNMANAGED file (no manifest entry). Reconcile refuses to clobber a file it
# does not manage and lands it in res.Skipped, so this machine is SERVED
# smoke-gov and never applies it, while smoke-rule does land in its project.
#
# BOTH DELTAS, BECAUSE NEITHER ONE DISCRIMINATES ALONE. "smoke-gov did not move"
# is equally true of a client that reports nothing at all; "smoke-rule went up"
# is equally true of a client reporting everything it was served. Deltas rather
# than absolutes because this install's rows share the aggregate with G1a's.
SYNC_G2=$(mktemp -d); SYNC_G2_PROJ=$(mktemp -d)
mkdir -p "$SYNC_G2/.claude/agents" "$SYNC_G2/.config/orbeat"
seed_sync_token "$SYNC_G2"
printf 'UNMANAGED-LOCAL-EDIT\n' > "$SYNC_G2/.claude/agents/smoke-gov.md"
HOME="$SYNC_G2" "$SYNC_BIN/orbeat-sync" project add "$SYNC_G2_PROJ" >/dev/null

g2_gov_before=$(deployments_json "$ADMIN_TOKEN" "$GOV_ID" | jq -r '.installs')
g2_rule_before=$(deployments_json "$ADMIN_TOKEN" "$RULE_ID" | jq -r '.installs')
set +e
g2_out=$(HOME="$SYNC_G2" "$SYNC_BIN/orbeat-sync" sync 2>&1)
g2_rc=$?
set -e
[ "$g2_rc" = "0" ] \
  || { echo "FAIL: the G2 sync exited $g2_rc (want 0: an unmanaged collision is a skip, not a failure); output: $g2_out"; exit 1; }
# The two premises, asserted rather than assumed. Had Reconcile clobbered the
# file, smoke-gov WOULD have been applied and the first delta would be measuring
# nothing; had the rule not landed, the sibling delta would be vacuous.
grep -q 'UNMANAGED-LOCAL-EDIT' "$SYNC_G2/.claude/agents/smoke-gov.md" \
  || { echo "FAIL: the unmanaged file was clobbered, so G2 has no skip left to observe"; exit 1; }
grep -q 'RULE-BODY-NO-SECRETS' "$SYNC_G2_PROJ/AGENTS.md" \
  || { echo "FAIL: the G2 install's rule never landed, so its sibling delta would be vacuous"; exit 1; }
g2_gov_after=$(deployments_json "$ADMIN_TOKEN" "$GOV_ID" | jq -r '.installs')
g2_rule_after=$(deployments_json "$ADMIN_TOKEN" "$RULE_ID" | jq -r '.installs')
[ "$g2_gov_after" = "$g2_gov_before" ] \
  || { echo "FAIL: a SKIPPED artifact was recorded as deployed: smoke-gov installs went $g2_gov_before to $g2_gov_after"; exit 1; }
[ "$g2_rule_after" = "$((g2_rule_before + 1))" ] \
  || { echo "FAIL: the applied rule was not recorded: smoke-rule installs went $g2_rule_before to $g2_rule_after, want $((g2_rule_before + 1))"; exit 1; }
echo "    G2: applied not served, the skipped smoke-gov held at $g2_gov_after installs while smoke-rule rose to $g2_rule_after"

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
# All three sections ran: null means "this reconciler never ran" (cmd/orbeat-sync/outcome.go:16-20),
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
# assertion surface (cmd/orbeat-sync/main_test.go covers "no block after a fatal abort" in
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

# G3: A FATAL RUN REPORTS NOTHING, AND THEREFORE DELETES NOTHING. A report is a
# REPLACE: it removes every row this install previously reported that is not in
# the body. On this path Reconcile returns before its write loop runs at all, so
# the applied set is empty for a reason that has nothing to do with what is on
# disk, and a client that reported it anyway would wipe a healthy machine's rows
# and read as a de-entitlement that never happened.
#
# The fixture was built with `cp -a "$SYNC_HOME/."`, so it inherits SYNC_HOME's
# install.json and would report under the SAME install id. That inheritance is
# the only reason this gate can observe the wipe, so it is asserted rather than
# relied on: a future change that stopped copying the state dir would leave the
# comparison below passing while proving nothing.
[ "$(jq -r '.installId' "$SYNC_FATAL_A/.config/orbeat/install.json")" \
   = "$(jq -r '.installId' "$SYNC_HOME/.config/orbeat/install.json")" ] \
  || { echo "FAIL: the fatal fixture does not share SYNC_HOME's install id, so G3 could not observe a wipe"; exit 1; }
g3_before=$(deployments_json "$ADMIN_TOKEN" "$GOV_ID")

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

# Byte-identical, not "still non-zero": reported_at moves on every upsert, so an
# unconditional report is visible here even in the case where it re-sent exactly
# the same set. Nothing between the g3_before capture and this line touches the
# registry: the assertions above only stat files.
g3_after=$(deployments_json "$ADMIN_TOKEN" "$GOV_ID")
[ "$g3_before" = "$g3_after" ] \
  || { echo "FAIL: a fatal run changed the registry; before=$g3_before after=$g3_after"; exit 1; }
echo "    G3: the fatal run filed no report, so the smoke-gov aggregate is byte-identical across it"

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

# ── Artifact identity through approval (spec 2026-08-22) ──────────────────────
#
# A rename on an approved artifact is now accepted and DEFERRED. The working
# copy drops to draft, and every entitled developer keeps receiving the OLD
# name carrying the OLD body until a second admin approves the change. The name
# is the file path on a developer's disk (agents/<name>.md), so a distribution
# query reading the live row instead of the approved snapshot moves a file on
# every machine with no reviewer in the loop.
#
# WHY THIS BLOCK RUNS LAST, and not inside the governance section around line
# 504 where the plan for this slice placed it. The claim is about what the
# CLIENT receives, and the real orbeat-sync binary is not built until the top of
# the binary gate far above. Renaming smoke-gov up there would also have to
# survive every scenario in between, and two of them are coupled to its file
# name by construction: F4 asserts .artifacts.updated == 1 on the stated fixture
# "alice is served exactly ONE file-backed artifact", and F3/F4/F5 each ablate
# agents/smoke-gov.md by that literal path. Running here costs no extra stack
# cycle, the doctor block above just restored SYNC_HOME to a healthy fully
# synced tree, and nothing downstream reads smoke-gov again.
#
# jq is used unqualified below, as it already is throughout the binary gate: a
# run without it has failed long before reaching this line.
echo "==> identity gate: a rename on an approved artifact is deferred to approval, end to end"

# alice's ACCESS_TOKEN was minted at the top of this script, and Keycloak's
# default access-token lifetime is 5 minutes, so by this point in a run it may
# already be expired. A 401 here would read as a distribution bug. Mint a fresh
# one for the reads below, and re-seed the sync home's credential cache for the
# same reason (seed_sync_token's own doc comment describes that window).
IDENT_TOKEN=$(get_token alice alice) || true
[ -n "$IDENT_TOKEN" ] || { echo "FAIL: could not mint a fresh alice token for the identity gate"; exit 1; }
seed_sync_token "$SYNC_HOME"

GOV_NEW_NAME=smoke-gov-renamed

# The state every claim below is measured against, asserted so a later failure
# cannot be blamed on a precondition nobody checked.
test -f "$SYNC_HOME/.claude/agents/smoke-gov.md" \
  || { echo "FAIL: precondition: agents/smoke-gov.md is not on disk before the identity gate"; exit 1; }

# 1. Rename through the real API. The PUT carries a new BODY as well as a new
#    name, so identity and content stay separable in every assertion below: an
#    implementation that defers one but not the other is visible rather than
#    hidden behind a single "unchanged" check.
GOV_RV3=$(current_row_version "$ADMIN_TOKEN" "$GOV_ID")
ident_rename_status=$(curl -s -o /dev/null -w '%{http_code}' -X PUT \
  -H "Authorization: Bearer $ADMIN_TOKEN" -H "Content-Type: application/json" \
  -H "If-Match: \"$GOV_RV3\"" \
  -d "{\"type\":\"subagent\",\"name\":\"$GOV_NEW_NAME\",\"description\":\"d\",\"content\":\"---\nname: $GOV_NEW_NAME\ndescription: d\n---\nRENAMED-BODY\",\"visibility\":\"role\"}" \
  "http://localhost:8080/v1/admin/artifacts/$GOV_ID")
[ "$ident_rename_status" = "200" ] \
  || { echo "FAIL: renaming an approved artifact returned $ident_rename_status, want 200 (the identity lock should be gone)"; exit 1; }

# 2. Preconditions INSIDE the gate, never the gate itself. A run where the
#    rename silently did nothing would satisfy every "still the old name"
#    assertion that follows, which is precisely the shape of a gate that cannot
#    fail. Prove the live row actually moved, that the working copy went back to
#    draft, and that the API reports the identity it is still distributing.
ident_after_rename=$(curl -s -H "Authorization: Bearer $ADMIN_TOKEN" "http://localhost:8080/v1/admin/artifacts/$GOV_ID")
[ "$(echo "$ident_after_rename" | jq -r '.name')" = "$GOV_NEW_NAME" ] \
  || { echo "FAIL: the live name did not change, so nothing below can be evidence: $ident_after_rename"; exit 1; }
[ "$(echo "$ident_after_rename" | jq -r '.approvalState')" = "draft" ] \
  || { echo "FAIL: an identity edit must return the working copy to draft: $ident_after_rename"; exit 1; }
[ "$(echo "$ident_after_rename" | jq -r '.approvedName')" = "smoke-gov" ] \
  || { echo "FAIL: approvedName must still report the distributed name: $ident_after_rename"; exit 1; }
echo "    rename accepted => 200, live name=$GOV_NEW_NAME, state=draft, approvedName=smoke-gov"

# 3. The API still serves the OLD pair. Both halves, and both directions: the
#    old name and body present, the new name and body absent. Asserting only
#    the presence of the old name passes on a payload carrying BOTH.
ident_sync_pending=$(curl -s -H "Authorization: Bearer $IDENT_TOKEN" http://localhost:8080/v1/sync/artifacts)
echo "$ident_sync_pending" | jq -e '[.artifacts[] | select(.name=="smoke-gov")] | length == 1' >/dev/null \
  || { echo "FAIL: an unapproved rename changed what /v1/sync/artifacts serves: $ident_sync_pending"; exit 1; }
echo "$ident_sync_pending" | jq -e --arg n "$GOV_NEW_NAME" '[.artifacts[] | select(.name==$n)] | length == 0' >/dev/null \
  || { echo "FAIL: the new name reached distribution before approval: $ident_sync_pending"; exit 1; }
echo "$ident_sync_pending" | jq -e '.artifacts[] | select(.name=="smoke-gov") | .content | contains("APPROVED-BODY")' >/dev/null \
  || { echo "FAIL: sync serves the old name but not the approved body: $ident_sync_pending"; exit 1; }
echo "$ident_sync_pending" | jq -e '[.artifacts[] | select(.content | contains("RENAMED-BODY"))] | length == 0' >/dev/null \
  || { echo "FAIL: the unapproved body reached distribution: $ident_sync_pending"; exit 1; }
echo "    /v1/sync/artifacts still serves smoke-gov + APPROVED-BODY, and neither the new name nor the new body"

# 4. THE REAL BINARY still receives the old pair. ABLATE BEFORE MEASURING, the
#    same discipline F5 documents: without the rm, "agents/smoke-gov.md is
#    present" is true whether the sync ran or not. After it, the old path can
#    only come back from a real reconcile, while a distribution reading the LIVE
#    row would create agents/smoke-gov-renamed.md and leave the old path
#    missing. Both assertions therefore discriminate, in opposite directions.
rm "$SYNC_HOME/.claude/agents/smoke-gov.md"
set +e
ident_sync1_out=$(HOME="$SYNC_HOME" "$SYNC_BIN/orbeat-sync" sync 2>&1)
ident_sync1_rc=$?
set -e
[ "$ident_sync1_rc" = "0" ] \
  || { echo "FAIL: orbeat-sync sync exited $ident_sync1_rc while a rename was pending; output: $ident_sync1_out"; exit 1; }
test -f "$SYNC_HOME/.claude/agents/smoke-gov.md" \
  || { echo "FAIL: the real client stopped delivering agents/smoke-gov.md while the rename is unapproved; output: $ident_sync1_out"; exit 1; }
test ! -f "$SYNC_HOME/.claude/agents/$GOV_NEW_NAME.md" \
  || { echo "FAIL: the real client wrote agents/$GOV_NEW_NAME.md from an UNAPPROVED rename"; exit 1; }
grep -q 'APPROVED-BODY' "$SYNC_HOME/.claude/agents/smoke-gov.md" \
  || { echo "FAIL: agents/smoke-gov.md came back without the approved body"; exit 1; }
grep -q 'RENAMED-BODY' "$SYNC_HOME/.claude/agents/smoke-gov.md" \
  && { echo "FAIL: the unapproved body landed on disk under the old name"; exit 1; }
echo "    the real orbeat-sync binary still writes agents/smoke-gov.md with APPROVED-BODY"

# 5. Approve the rename. boss submitted, boss2 approves, so separation of duties
#    is genuinely exercised. A fresh rowVersion at each step: the 00013 trigger
#    bumped it on the PUT and again on the submit.
ident_submit_status=$(curl -s -o /dev/null -w '%{http_code}' -X POST \
  -H "Authorization: Bearer $ADMIN_TOKEN" "http://localhost:8080/v1/admin/artifacts/$GOV_ID/submit")
[ "$ident_submit_status" = "200" ] || { echo "FAIL: submit the renamed smoke-gov status $ident_submit_status"; exit 1; }
GOV_RV4=$(current_row_version "$ADMIN_TOKEN" "$GOV_ID")
ident_approve_status=$(curl -s -o /dev/null -w '%{http_code}' -X POST \
  -H "Authorization: Bearer $BOSS2_TOKEN" -H "If-Match: \"$GOV_RV4\"" \
  "http://localhost:8080/v1/admin/artifacts/$GOV_ID/approve")
[ "$ident_approve_status" = "200" ] \
  || { echo "FAIL: approving the rename (boss2) returned $ident_approve_status, want 200"; exit 1; }

# G1c: THE TARGET MOVED AND THE MACHINE HAS NOT. Approval appended a revision;
# the sync home has not run since, so latestRevision and the machine's recorded
# revision are required to differ, and by exactly the artifact's own history.
# This is the state an operator actually opens this page in: somebody approved
# something and the fleet has not caught up.
#
# WHAT IT CATCHES, stated narrowly because the neighbouring claim was wrong once
# already: a READ that renders the histogram from the newest approved revision
# rather than from the stored rows, and any implementation that can only ever
# answer "everybody is current". It does NOT catch a WRITE that substitutes
# MAX(revision_num) for the reported value: that row was written while MAX was
# still $GOV_REV1, so the mutant records $GOV_REV1 too. Measured, by running it.
# G1b is the gate for that one.
#
# It runs BEFORE step 6's sync deliberately: the window is one approval wide and
# closes the moment the client reports again.
GOV_REV2=$(latest_revision_num "$ADMIN_TOKEN" "$GOV_ID")
[ "$GOV_REV2" -gt "$GOV_REV1" ] \
  || { echo "FAIL: approving the rename did not append a revision ($GOV_REV1 then $GOV_REV2), so G1c has nothing to measure"; exit 1; }
gov_dep2=$(deployments_json "$ADMIN_TOKEN" "$GOV_ID")
echo "$gov_dep2" | jq -e --argjson r "$GOV_REV2" '.latestRevision == $r' >/dev/null \
  || { echo "FAIL: latestRevision did not follow the new approval to $GOV_REV2; got $gov_dep2"; exit 1; }
echo "$gov_dep2" | jq -e --argjson r "$GOV_REV1" '.byRevision == [{"revision":$r,"installs":1}]' >/dev/null \
  || { echo "FAIL: the install has not re-synced, so it must still stand recorded at revision $GOV_REV1; got $gov_dep2"; exit 1; }
echo "$gov_dep2" | jq -e '.behindLatest == 1 and .installs == 1' >/dev/null \
  || { echo "FAIL: one install on an older revision is one install behind; got $gov_dep2"; exit 1; }
echo "    G1c: approval moved latestRevision to $GOV_REV2 while the machine stayed recorded at $GOV_REV1 (behindLatest 1)"

# 6. Only now does the pair flip, in the API and on disk together.
ident_sync_flipped=$(curl -s -H "Authorization: Bearer $IDENT_TOKEN" http://localhost:8080/v1/sync/artifacts)
echo "$ident_sync_flipped" | jq -e --arg n "$GOV_NEW_NAME" '[.artifacts[] | select(.name==$n)] | length == 1' >/dev/null \
  || { echo "FAIL: the approved rename did not reach /v1/sync/artifacts: $ident_sync_flipped"; exit 1; }
echo "$ident_sync_flipped" | jq -e '[.artifacts[] | select(.name=="smoke-gov")] | length == 0' >/dev/null \
  || { echo "FAIL: the old name is still being distributed after approval: $ident_sync_flipped"; exit 1; }
echo "$ident_sync_flipped" | jq -e --arg n "$GOV_NEW_NAME" '.artifacts[] | select(.name==$n) | .content | contains("RENAMED-BODY")' >/dev/null \
  || { echo "FAIL: the new name is distributed without the newly approved body: $ident_sync_flipped"; exit 1; }
set +e
ident_sync2_out=$(HOME="$SYNC_HOME" "$SYNC_BIN/orbeat-sync" sync 2>&1)
ident_sync2_rc=$?
set -e
[ "$ident_sync2_rc" = "0" ] \
  || { echo "FAIL: orbeat-sync sync exited $ident_sync2_rc after the rename was approved; output: $ident_sync2_out"; exit 1; }
test -f "$SYNC_HOME/.claude/agents/$GOV_NEW_NAME.md" \
  || { echo "FAIL: the approved rename never reached the real client's disk; output: $ident_sync2_out"; exit 1; }
grep -q 'RENAMED-BODY' "$SYNC_HOME/.claude/agents/$GOV_NEW_NAME.md" \
  || { echo "FAIL: agents/$GOV_NEW_NAME.md does not carry the newly approved body"; exit 1; }
# The rename MOVES the file. Leaving the old path behind would give every
# developer two subagents with the same description, one of them frozen at the
# pre-rename body forever, since nothing would ever manage it again.
test ! -f "$SYNC_HOME/.claude/agents/smoke-gov.md" \
  || { echo "FAIL: the old path survived the approved rename; a rename must move the file, not duplicate it"; exit 1; }
echo "    approval flips the pair: sync serves $GOV_NEW_NAME + RENAMED-BODY, and the real client moved agents/smoke-gov.md to agents/$GOV_NEW_NAME.md"

# G1d: the re-sync closes the gap, at the NEW number. Together with G1c
# this is the assertion a constant-storing registry cannot satisfy: the same
# install is required to be recorded at two different revisions across one
# approval and one sync.
gov_dep3=$(deployments_json "$ADMIN_TOKEN" "$GOV_ID")
echo "$gov_dep3" | jq -e --argjson r "$GOV_REV2" '.installs == 1 and .byRevision == [{"revision":$r,"installs":1}] and .behindLatest == 0' >/dev/null \
  || { echo "FAIL: after re-syncing, the install must stand at revision $GOV_REV2 with nothing behind; got $gov_dep3"; exit 1; }
echo "    G1d: the re-synced machine moved to revision $GOV_REV2, behindLatest 0"

# ── G4: replace semantics make a de-entitlement visible ───────────────────────
#
# Revoke alice's grant and sync. The client no longer applies the artifact, so
# it is absent from the report, and a report is a REPLACE: the row goes and the
# aggregate falls to zero installs. An upsert-only implementation would leave a
# row no later run could ever clear, and this page would go on counting a
# machine that gave the artifact back, which is the failure mode a registry
# exists to not have.
#
# LAST IN THE FILE, deliberately. It takes the artifact out of alice's catalog
# and its file off the sync home's disk, and nothing below reads either.
echo "==> G4: revoking the grant, and the registry following it to zero"
gov_ent_list=$(curl -s -H "Authorization: Bearer $ADMIN_TOKEN" http://localhost:8080/v1/admin/artifact-entitlements)
GOV_ENT_ID=$(echo "$gov_ent_list" | jq -r --arg a "$GOV_ID" --arg r "$ROLE_ID" \
  'first(.artifactEntitlements[] | select(.artifactId==$a and .roleId==$r) | .id)')
[ -n "$GOV_ENT_ID" ] && [ "$GOV_ENT_ID" != "null" ] \
  || { echo "FAIL: could not find the smoke-gov artifact entitlement to revoke: $gov_ent_list"; exit 1; }
gov_revoke_status=$(curl -s -o /dev/null -w '%{http_code}' -X DELETE \
  -H "Authorization: Bearer $ADMIN_TOKEN" "http://localhost:8080/v1/admin/artifact-entitlements/$GOV_ENT_ID")
[ "$gov_revoke_status" = "204" ] \
  || { echo "FAIL: revoking the artifact entitlement returned $gov_revoke_status, want 204"; exit 1; }
seed_sync_token "$SYNC_HOME"
set +e
g4_out=$(HOME="$SYNC_HOME" "$SYNC_BIN/orbeat-sync" sync 2>&1)
g4_rc=$?
set -e
[ "$g4_rc" = "0" ] || { echo "FAIL: the post-revocation sync exited $g4_rc (want 0); output: $g4_out"; exit 1; }
# The premise: the client really did stop holding it. Without this, "zero
# installs" is also what a sync that never ran would produce.
test ! -f "$SYNC_HOME/.claude/agents/$GOV_NEW_NAME.md" \
  || { echo "FAIL: the revoked artifact is still on disk, so a zero here would not be about a de-entitlement; output: $g4_out"; exit 1; }
gov_dep4=$(deployments_json "$ADMIN_TOKEN" "$GOV_ID")
echo "$gov_dep4" | jq -e '.installs == 0 and .users == 0 and .byRevision == [] and .behindLatest == 0' >/dev/null \
  || { echo "FAIL: the revoked artifact must fall to zero installs; got $gov_dep4"; exit 1; }
echo "$gov_dep4" | jq -e '.oldestReportedAt == null' >/dev/null \
  || { echo "FAIL: with no rows left there is no oldest report; got $gov_dep4"; exit 1; }
# THE ZERO IS A REAL ZERO, NOT A BLIND SPOT. latestRevision still names the
# artifact's newest approved version and observable is still true, so this is
# not the "orbeat cannot see this artifact" answer wearing the same numbers,
# which is the one confusion the response shape exists to prevent.
echo "$gov_dep4" | jq -e --argjson r "$GOV_REV2" '.latestRevision == $r and .observable == true' >/dev/null \
  || { echo "FAIL: the artifact should still be observable at revision $GOV_REV2; got $gov_dep4"; exit 1; }
echo "    G4: the revoked grant took the install with it, 0 installs on an artifact still observable at revision $GOV_REV2"

# ── Artifact version pinning (docs/specs/2026-08-22-orbeat-artifact-version-
# pinning-design.md) ────────────────────────────────────────────────────────
#
# LAST IN THE FILE, after G4, for the same reason the identity gate above runs
# last: F3, F4 and F5 ablate agents/smoke-gov.md by that literal path, and F4
# asserts .artifacts.updated == 1 on the stated fixture "alice is served
# exactly ONE file-backed artifact" (smoke-gov). A role-visibility
# file-backed artifact entitled to orbeat-user, placed before that block,
# would break both. This section owns its own artifacts (smoke-pin,
# smoke-pin-prune) and its own sync HOME (SYNC_PIN, not SYNC_HOME), so
# nothing above it needs to change and nothing here can perturb an assertion
# above it.
echo "==> artifact version pinning: client pin, admin floor, pruned degradation, prune ignoring the registry"

SYNC_PIN=$(mktemp -d)
mkdir -p "$SYNC_PIN/.claude/agents" "$SYNC_PIN/.config/orbeat"
seed_sync_token "$SYNC_PIN"

# Fresh tokens: this section runs after every gate above it, several minutes
# into the run, and boss/boss2's tokens from earlier sections may already be
# past Keycloak's ~5-min access-token lifetime (the identity gate's own
# comment records the same concern).
PIN_ADMIN_TOKEN=$(get_token boss boss)
[ -n "$PIN_ADMIN_TOKEN" ] || { echo "FAIL: could not fetch a fresh admin token for the pinning section"; exit 1; }
PIN_BOSS2_TOKEN=$(get_token boss2 boss2)
[ -n "$PIN_BOSS2_TOKEN" ] || { echo "FAIL: could not fetch a fresh boss2 token for the pinning section"; exit 1; }

echo "==> admin creates a role-visibility subagent (draft) entitled to orbeat-user, for gates 1 and 2"
pin_resp=$(curl -s -w '\n%{http_code}' -X POST \
  -H "Authorization: Bearer $PIN_ADMIN_TOKEN" -H "Content-Type: application/json" \
  -d '{"type":"subagent","name":"smoke-pin","description":"d","content":"---\nname: smoke-pin\ndescription: d\n---\nPIN-V1-BODY","visibility":"role"}' \
  http://localhost:8080/v1/admin/artifacts)
pin_status=$(printf '%s' "$pin_resp" | tail -n1)
pin_body=$(printf '%s' "$pin_resp" | sed '$d')
[ "$pin_status" = "201" ] || { echo "FAIL: create smoke-pin $pin_status: $pin_body"; exit 1; }
PIN_ID=$(echo "$pin_body" | jq -r '.id')
[ -n "$PIN_ID" ] && [ "$PIN_ID" != "null" ] || { echo "FAIL: could not resolve smoke-pin id: $pin_body"; exit 1; }
echo "    draft subagent smoke-pin created id=$PIN_ID"

pin_ent_status=$(curl -s -o /dev/null -w '%{http_code}' -X POST \
  -H "Authorization: Bearer $PIN_ADMIN_TOKEN" -H "Content-Type: application/json" \
  -d "{\"roleId\":\"$ROLE_ID\",\"artifactId\":\"$PIN_ID\"}" \
  http://localhost:8080/v1/admin/artifact-entitlements)
case "$pin_ent_status" in
  201|409) : ;;
  *) echo "FAIL: create smoke-pin entitlement status $pin_ent_status"; exit 1 ;;
esac
echo "    orbeat-user entitled to smoke-pin ($pin_ent_status)"

echo "==> approving smoke-pin twice, with distinguishable bodies (PIN-V1-BODY, then PIN-V2-BODY)"
pin_submit1_status=$(curl -s -o /dev/null -w '%{http_code}' -X POST \
  -H "Authorization: Bearer $PIN_ADMIN_TOKEN" "http://localhost:8080/v1/admin/artifacts/$PIN_ID/submit")
[ "$pin_submit1_status" = "200" ] || { echo "FAIL: submit smoke-pin v1 status $pin_submit1_status"; exit 1; }
PIN_RV1=$(current_row_version "$PIN_ADMIN_TOKEN" "$PIN_ID")
pin_approve1_status=$(curl -s -o /dev/null -w '%{http_code}' -X POST \
  -H "Authorization: Bearer $PIN_BOSS2_TOKEN" -H "If-Match: \"$PIN_RV1\"" \
  "http://localhost:8080/v1/admin/artifacts/$PIN_ID/approve")
[ "$pin_approve1_status" = "200" ] || { echo "FAIL: approve smoke-pin v1 (boss2) status $pin_approve1_status"; exit 1; }
PIN_REV1=$(latest_revision_num "$PIN_ADMIN_TOKEN" "$PIN_ID")
echo "    smoke-pin PIN-V1-BODY approved => revision $PIN_REV1"

PIN_RV2A=$(current_row_version "$PIN_ADMIN_TOKEN" "$PIN_ID")
pin_put2_status=$(curl -s -o /dev/null -w '%{http_code}' -X PUT \
  -H "Authorization: Bearer $PIN_ADMIN_TOKEN" -H "Content-Type: application/json" \
  -H "If-Match: \"$PIN_RV2A\"" \
  -d '{"type":"subagent","name":"smoke-pin","description":"d","content":"---\nname: smoke-pin\ndescription: d\n---\nPIN-V2-BODY","visibility":"role"}' \
  "http://localhost:8080/v1/admin/artifacts/$PIN_ID")
[ "$pin_put2_status" = "200" ] || { echo "FAIL: edit smoke-pin to v2 status $pin_put2_status"; exit 1; }
pin_submit2_status=$(curl -s -o /dev/null -w '%{http_code}' -X POST \
  -H "Authorization: Bearer $PIN_ADMIN_TOKEN" "http://localhost:8080/v1/admin/artifacts/$PIN_ID/submit")
[ "$pin_submit2_status" = "200" ] || { echo "FAIL: resubmit smoke-pin v2 status $pin_submit2_status"; exit 1; }
PIN_RV2B=$(current_row_version "$PIN_ADMIN_TOKEN" "$PIN_ID")
pin_approve2_status=$(curl -s -o /dev/null -w '%{http_code}' -X POST \
  -H "Authorization: Bearer $PIN_BOSS2_TOKEN" -H "If-Match: \"$PIN_RV2B\"" \
  "http://localhost:8080/v1/admin/artifacts/$PIN_ID/approve")
[ "$pin_approve2_status" = "200" ] || { echo "FAIL: approve smoke-pin v2 (boss2) status $pin_approve2_status"; exit 1; }
PIN_REV2=$(latest_revision_num "$PIN_ADMIN_TOKEN" "$PIN_ID")
[ "$PIN_REV2" -gt "$PIN_REV1" ] \
  || { echo "FAIL: the v2 approval did not append a revision ($PIN_REV1 then $PIN_REV2), so gate 1 has nothing to pin below"; exit 1; }
echo "    smoke-pin PIN-V2-BODY approved => revision $PIN_REV2"

echo "==> gate 1: an unpinned sync serves the newest revision (PIN-V2-BODY)"
set +e
unpinned_out=$(HOME="$SYNC_PIN" "$SYNC_BIN/orbeat-sync" sync 2>&1)
unpinned_rc=$?
set -e
[ "$unpinned_rc" = "0" ] || { echo "FAIL: the unpinned sync exited $unpinned_rc; output: $unpinned_out"; exit 1; }
grep -q 'PIN-V2-BODY' "$SYNC_PIN/.claude/agents/smoke-pin.md" \
  || { echo "FAIL: an unpinned sync did not serve the newest revision (PIN-V2-BODY)"; exit 1; }
echo "    unpinned sync => PIN-V2-BODY on disk"

echo "==> gate 1: orbeat-sync pin subagent/smoke-pin --revision 1"
seed_sync_token "$SYNC_PIN"
set +e
pinset_out=$(HOME="$SYNC_PIN" "$SYNC_BIN/orbeat-sync" pin subagent/smoke-pin --revision 1 2>&1)
pinset_rc=$?
set -e
[ "$pinset_rc" = "0" ] || { echo "FAIL: orbeat-sync pin --revision 1 exited $pinset_rc; output: $pinset_out"; exit 1; }
echo "$pinset_out" | grep -q 'Pinned subagent/smoke-pin to revision 1' \
  || { echo "FAIL: pin set did not confirm the pin; output: $pinset_out"; exit 1; }
echo "    smoke-pin locally pinned to revision 1"

# Remove the target file BEFORE the sync that is supposed to restore V1: "V1
# is present" must not be able to pass on the V2 file the unpinned sync above
# left behind. F5 (:1209-1219) documents the same discipline for the same
# reason.
rm "$SYNC_PIN/.claude/agents/smoke-pin.md"
seed_sync_token "$SYNC_PIN"
set +e
gate1_out=$(HOME="$SYNC_PIN" "$SYNC_BIN/orbeat-sync" sync 2>&1)
gate1_rc=$?
set -e
[ "$gate1_rc" = "0" ] || { echo "FAIL: sync after pinning smoke-pin to revision 1 exited $gate1_rc; output: $gate1_out"; exit 1; }
grep -q 'PIN-V1-BODY' "$SYNC_PIN/.claude/agents/smoke-pin.md" \
  || { echo "FAIL: the pinned sync did not serve revision 1 (PIN-V1-BODY); output: $gate1_out"; exit 1; }
# THE HALF THAT MAKES THIS NON-VACUOUS: "V1 present" alone also passes on an
# implementation that writes BOTH V1 and V2, or one that never touched the
# file at all had the previous step not removed it.
grep -q 'PIN-V2-BODY' "$SYNC_PIN/.claude/agents/smoke-pin.md" \
  && { echo "FAIL: the pinned sync served revision 1 but the file still carries V2 content"; exit 1; }
echo "    gate 1: pin --revision 1 => sync serves PIN-V1-BODY and does not contain PIN-V2-BODY"

echo "==> gate 1: raising smoke-pin's admin floor to revision $PIN_REV2 overrides the client pin"
PIN_FLOOR_RV=$(current_row_version "$PIN_ADMIN_TOKEN" "$PIN_ID")
floor_status=$(curl -s -o /dev/null -w '%{http_code}' -X PUT \
  -H "Authorization: Bearer $PIN_ADMIN_TOKEN" -H "Content-Type: application/json" \
  -H "If-Match: \"$PIN_FLOOR_RV\"" \
  -d "{\"minRevision\":$PIN_REV2}" \
  "http://localhost:8080/v1/admin/artifacts/$PIN_ID/min-revision")
[ "$floor_status" = "200" ] || { echo "FAIL: raising smoke-pin's floor to $PIN_REV2 status $floor_status"; exit 1; }
echo "    smoke-pin floor raised to revision $PIN_REV2"

rm "$SYNC_PIN/.claude/agents/smoke-pin.md"
seed_sync_token "$SYNC_PIN"
set +e
gate1b_out=$(HOME="$SYNC_PIN" "$SYNC_BIN/orbeat-sync" sync 2>&1)
gate1b_rc=$?
set -e
# An overridden pin is a warning, never a failure (spec §9.3, the sync guide's
# own exit-contract line): the mutant this catches is one that turns an
# admin's floor override into a retryable failure the developer did nothing
# to cause.
[ "$gate1b_rc" = "0" ] \
  || { echo "FAIL: sync under the floor override exited $gate1b_rc (want 0: an overridden pin is a warning, never a failure); output: $gate1b_out"; exit 1; }
grep -q 'PIN-V2-BODY' "$SYNC_PIN/.claude/agents/smoke-pin.md" \
  || { echo "FAIL: the floor did not raise the served revision back to $PIN_REV2 (PIN-V2-BODY); output: $gate1b_out"; exit 1; }
grep -q 'PIN-V1-BODY' "$SYNC_PIN/.claude/agents/smoke-pin.md" \
  && { echo "FAIL: the file still carries the below-floor content after the floor was raised"; exit 1; }
echo "$gate1b_out" | grep -q "smoke-pin: held at revision 1, this sync served revision $PIN_REV2 instead (floor)" \
  || { echo "FAIL: the floor override did not print a warning naming the pin, the served revision and floor; output: $gate1b_out"; exit 1; }
echo "    gate 1: the floor moved the served revision to $PIN_REV2, exit 0, warning names the pin, the served revision and floor"

echo "==> gate 2: --json for the same floor-overridden run reports the pin as structured data"
rm "$SYNC_PIN/.claude/agents/smoke-pin.md"
seed_sync_token "$SYNC_PIN"
set +e
gate2_json=$(HOME="$SYNC_PIN" "$SYNC_BIN/orbeat-sync" sync --json 2>/dev/null)
gate2_rc=$?
set -e
# THE PAIRING IS THE POINT (smoke.sh's own F1 convention, :1180-1181): a
# mutant that stops emitting JSON would still pass a bare rc check, and a
# mutant that hardcodes .exitCode would still pass a bare rc check too if the
# process itself silently swallowed a real failure, so both are asserted,
# independently, from the SAME run.
[ "$gate2_rc" = "0" ] || { echo "FAIL: sync --json under the floor override exited $gate2_rc (want 0); output: $gate2_json"; exit 1; }
echo "$gate2_json" | jq -e . >/dev/null \
  || { echo "FAIL: sync --json emitted no parseable JSON: $gate2_json"; exit 1; }
[ "$(echo "$gate2_json" | jq -r '.exitCode')" = "0" ] \
  || { echo "FAIL: .exitCode was $(echo "$gate2_json" | jq -r '.exitCode'), want 0"; exit 1; }
# Selected by name, not asserted as the whole array: .artifacts.pins carries
# one entry per HELD pin, and smoke-pin-prune's own pin (gate 3/11, below)
# will add a second entry to this same array once it exists. Asserting the
# whole array as a singleton here would pass now and break the moment gate
# 3/11's fixture is created, for a reason that has nothing to do with what
# this gate is testing.
echo "$gate2_json" | jq -e --argjson r "$PIN_REV2" --arg n "subagent/smoke-pin" \
  '[.artifacts.pins[] | select(.name==$n)] == [{"name":$n,"requested":1,"served":$r,"reason":"floor"}]' >/dev/null \
  || { echo "FAIL: .artifacts.pins did not carry the exact floor override for smoke-pin; got $gate2_json"; exit 1; }
grep -q 'PIN-V2-BODY' "$SYNC_PIN/.claude/agents/smoke-pin.md" \
  || { echo "FAIL: the --json run did not also write the floored revision to disk"; exit 1; }
# A WARNING, AND SPECIFICALLY NOT A FAILURE. Measured 2026-08-23: routing the
# pin lines into .artifacts.failures instead of .artifacts.warnings leaves the
# exit code at 0, because reconcileAll owns the exit code and this list does
# not feed it, so every assertion above stays green while --json reports a
# routine floor override as a partial failure. That contradicts the v1.15.0
# exit contract on the one surface a CI loop reads: exit 0 says "nothing to
# retry" and a non-empty failures array says the opposite. The pins array
# alone cannot catch it either, since it is populated identically both ways.
echo "$gate2_json" | jq -e '.artifacts.failures == []' >/dev/null \
  || { echo "FAIL: an overridden pin landed in .artifacts.failures; it is a warning at exit 0, never a failure: $gate2_json"; exit 1; }
echo "$gate2_json" | jq -e --arg n "subagent/smoke-pin" \
  '[.artifacts.warnings[] | select(contains($n))] | length == 1' >/dev/null \
  || { echo "FAIL: the floor override did not appear as exactly one .artifacts.warnings line naming subagent/smoke-pin: $gate2_json"; exit 1; }
echo "    gate 2: .artifacts.pins == [{requested:1, served:$PIN_REV2, reason:floor}], process rc and .exitCode both 0, the override a warning and .artifacts.failures empty"

# ── Gates 3 and 11 share one fixture, smoke-pin-prune, and are interleaved on
# purpose: gate 11 needs a real artifact_deployment row naming a revision
# WHILE that revision still exists, which is only true through the third
# approval (KEEP=3 means the fourth approval's prune is the first one that
# deletes anything); gate 3 needs the fixture pruned past what the pin names,
# which is only true once five approvals have landed. One fixture, one
# ordered story, rather than two fixtures that would have to agree on when
# pruning starts by construction rather than by reading the same code this
# section is proving.
echo "==> admin creates a second role-visibility subagent, smoke-pin-prune, entitled to orbeat-user, for gates 3 and 11"
PIN_ADMIN_TOKEN=$(get_token boss boss)
[ -n "$PIN_ADMIN_TOKEN" ] || { echo "FAIL: could not refresh the admin token before the prune fixture"; exit 1; }
PIN_BOSS2_TOKEN=$(get_token boss2 boss2)
[ -n "$PIN_BOSS2_TOKEN" ] || { echo "FAIL: could not refresh the boss2 token before the prune fixture"; exit 1; }

prune2_resp=$(curl -s -w '\n%{http_code}' -X POST \
  -H "Authorization: Bearer $PIN_ADMIN_TOKEN" -H "Content-Type: application/json" \
  -d '{"type":"skill","name":"smoke-pin-prune","description":"d","content":"---\nname: smoke-pin-prune\ndescription: d\n---\nPIN-PRUNE-V1","visibility":"role"}' \
  http://localhost:8080/v1/admin/artifacts)
prune2_status=$(printf '%s' "$prune2_resp" | tail -n1)
prune2_body=$(printf '%s' "$prune2_resp" | sed '$d')
[ "$prune2_status" = "201" ] || { echo "FAIL: create smoke-pin-prune $prune2_status: $prune2_body"; exit 1; }
PRUNE2_ID=$(echo "$prune2_body" | jq -r '.id')
[ -n "$PRUNE2_ID" ] && [ "$PRUNE2_ID" != "null" ] || { echo "FAIL: could not resolve smoke-pin-prune id: $prune2_body"; exit 1; }
echo "    draft subagent smoke-pin-prune created id=$PRUNE2_ID"

prune2_ent_status=$(curl -s -o /dev/null -w '%{http_code}' -X POST \
  -H "Authorization: Bearer $PIN_ADMIN_TOKEN" -H "Content-Type: application/json" \
  -d "{\"roleId\":\"$ROLE_ID\",\"artifactId\":\"$PRUNE2_ID\"}" \
  http://localhost:8080/v1/admin/artifact-entitlements)
case "$prune2_ent_status" in
  201|409) : ;;
  *) echo "FAIL: create smoke-pin-prune entitlement status $prune2_ent_status"; exit 1 ;;
esac
echo "    orbeat-user entitled to smoke-pin-prune ($prune2_ent_status)"

# submit (boss) + approve (boss2) revision 1, already created as a draft above.
prune2_submit1_status=$(curl -s -o /dev/null -w '%{http_code}' -X POST \
  -H "Authorization: Bearer $PIN_ADMIN_TOKEN" "http://localhost:8080/v1/admin/artifacts/$PRUNE2_ID/submit")
[ "$prune2_submit1_status" = "200" ] || { echo "FAIL: submit smoke-pin-prune v1 status $prune2_submit1_status"; exit 1; }
PRUNE2_RV1=$(current_row_version "$PIN_ADMIN_TOKEN" "$PRUNE2_ID")
prune2_approve1_status=$(curl -s -o /dev/null -w '%{http_code}' -X POST \
  -H "Authorization: Bearer $PIN_BOSS2_TOKEN" -H "If-Match: \"$PRUNE2_RV1\"" \
  "http://localhost:8080/v1/admin/artifacts/$PRUNE2_ID/approve")
[ "$prune2_approve1_status" = "200" ] || { echo "FAIL: approve smoke-pin-prune v1 (boss2) status $prune2_approve1_status"; exit 1; }

echo "==> approving smoke-pin-prune to a total of 3 (revision 1 not yet prunable at KEEP=$ORBEAT_ARTIFACT_REVISION_KEEP)"
for i in 2 3; do
  prune2_rv=$(current_row_version "$PIN_ADMIN_TOKEN" "$PRUNE2_ID")
  prune2_put_status=$(curl -s -o /dev/null -w '%{http_code}' -X PUT \
    -H "Authorization: Bearer $PIN_ADMIN_TOKEN" -H "Content-Type: application/json" \
    -H "If-Match: \"$prune2_rv\"" \
    -d "{\"type\":\"skill\",\"name\":\"smoke-pin-prune\",\"description\":\"d\",\"content\":\"---\nname: smoke-pin-prune\ndescription: d\n---\nPIN-PRUNE-V$i\",\"visibility\":\"role\"}" \
    "http://localhost:8080/v1/admin/artifacts/$PRUNE2_ID")
  [ "$prune2_put_status" = "200" ] || { echo "FAIL: edit smoke-pin-prune iteration $i status $prune2_put_status"; exit 1; }
  prune2_submit_status=$(curl -s -o /dev/null -w '%{http_code}' -X POST \
    -H "Authorization: Bearer $PIN_ADMIN_TOKEN" "http://localhost:8080/v1/admin/artifacts/$PRUNE2_ID/submit")
  [ "$prune2_submit_status" = "200" ] || { echo "FAIL: submit smoke-pin-prune iteration $i status $prune2_submit_status"; exit 1; }
  prune2_rv2=$(current_row_version "$PIN_ADMIN_TOKEN" "$PRUNE2_ID")
  prune2_approve_status=$(curl -s -o /dev/null -w '%{http_code}' -X POST \
    -H "Authorization: Bearer $PIN_BOSS2_TOKEN" -H "If-Match: \"$prune2_rv2\"" \
    "http://localhost:8080/v1/admin/artifacts/$PRUNE2_ID/approve")
  [ "$prune2_approve_status" = "200" ] || { echo "FAIL: approve smoke-pin-prune iteration $i status $prune2_approve_status"; exit 1; }
done
PRUNE2_REV3=$(latest_revision_num "$PIN_ADMIN_TOKEN" "$PRUNE2_ID")
echo "    smoke-pin-prune approved 3 times (revisions 1..$PRUNE2_REV3, none pruned yet)"

echo "==> gate 11: pin smoke-pin-prune to revision 1 while it still exists, and sync: this files a real artifact_deployment row naming revision 1"
seed_sync_token "$SYNC_PIN"
set +e
prune2_pinset_out=$(HOME="$SYNC_PIN" "$SYNC_BIN/orbeat-sync" pin skill/smoke-pin-prune --revision 1 2>&1)
prune2_pinset_rc=$?
set -e
[ "$prune2_pinset_rc" = "0" ] || { echo "FAIL: orbeat-sync pin skill/smoke-pin-prune --revision 1 exited $prune2_pinset_rc; output: $prune2_pinset_out"; exit 1; }
rm -f "$SYNC_PIN/.claude/skills/smoke-pin-prune/SKILL.md"
seed_sync_token "$SYNC_PIN"
set +e
g11_sync_out=$(HOME="$SYNC_PIN" "$SYNC_BIN/orbeat-sync" sync 2>&1)
g11_sync_rc=$?
set -e
[ "$g11_sync_rc" = "0" ] || { echo "FAIL: the pinned sync for smoke-pin-prune exited $g11_sync_rc; output: $g11_sync_out"; exit 1; }
grep -q 'PIN-PRUNE-V1' "$SYNC_PIN/.claude/skills/smoke-pin-prune/SKILL.md" \
  || { echo "FAIL: smoke-pin-prune pinned to revision 1 did not serve PIN-PRUNE-V1; output: $g11_sync_out"; exit 1; }
# THE PRECONDITION GATE 11 DEPENDS ON: the report really did land, and really
# does name revision 1, before the prune below has any chance to touch it.
# Asserted rather than assumed, the same discipline the identity gate's own
# preconditions (:1324-1327) follow.
prune2_dep_before=$(deployments_json "$PIN_ADMIN_TOKEN" "$PRUNE2_ID")
echo "$prune2_dep_before" | jq -e '.installs == 1 and .byRevision == [{"revision":1,"installs":1}]' >/dev/null \
  || { echo "FAIL: precondition: no real artifact_deployment row names smoke-pin-prune at revision 1 before the prune; got $prune2_dep_before"; exit 1; }
echo "    gate 11 precondition: the real client's report recorded smoke-pin-prune at revision 1 (1 install)"

echo "==> gate 11: one more approval prunes revision 1 out from under that report"
prune2_rv4=$(current_row_version "$PIN_ADMIN_TOKEN" "$PRUNE2_ID")
prune2_put4_status=$(curl -s -o /dev/null -w '%{http_code}' -X PUT \
  -H "Authorization: Bearer $PIN_ADMIN_TOKEN" -H "Content-Type: application/json" \
  -H "If-Match: \"$prune2_rv4\"" \
  -d '{"type":"skill","name":"smoke-pin-prune","description":"d","content":"---\nname: smoke-pin-prune\ndescription: d\n---\nPIN-PRUNE-V4","visibility":"role"}' \
  "http://localhost:8080/v1/admin/artifacts/$PRUNE2_ID")
[ "$prune2_put4_status" = "200" ] || { echo "FAIL: edit smoke-pin-prune iteration 4 status $prune2_put4_status"; exit 1; }
prune2_submit4_status=$(curl -s -o /dev/null -w '%{http_code}' -X POST \
  -H "Authorization: Bearer $PIN_ADMIN_TOKEN" "http://localhost:8080/v1/admin/artifacts/$PRUNE2_ID/submit")
[ "$prune2_submit4_status" = "200" ] || { echo "FAIL: submit smoke-pin-prune iteration 4 status $prune2_submit4_status"; exit 1; }
prune2_rv4b=$(current_row_version "$PIN_ADMIN_TOKEN" "$PRUNE2_ID")
prune2_approve4_status=$(curl -s -o /dev/null -w '%{http_code}' -X POST \
  -H "Authorization: Bearer $PIN_BOSS2_TOKEN" -H "If-Match: \"$prune2_rv4b\"" \
  "http://localhost:8080/v1/admin/artifacts/$PRUNE2_ID/approve")
[ "$prune2_approve4_status" = "200" ] || { echo "FAIL: approve smoke-pin-prune iteration 4 status $prune2_approve4_status"; exit 1; }

prune2_revs4=$(curl -s -H "Authorization: Bearer $PIN_ADMIN_TOKEN" "http://localhost:8080/v1/admin/artifacts/$PRUNE2_ID/revisions")
prune2_count4=$(echo "$prune2_revs4" | jq -r '.revisions | length')
[ "$prune2_count4" = "$ORBEAT_ARTIFACT_REVISION_KEEP" ] \
  || { echo "FAIL: smoke-pin-prune has $prune2_count4 revisions after 4 approvals, want exactly $ORBEAT_ARTIFACT_REVISION_KEEP; got $prune2_revs4"; exit 1; }
echo "$prune2_revs4" | jq -e '[.revisions[].revision] | index(1) == null' >/dev/null \
  || { echo "FAIL: revision 1 survived the prune despite being referenced by a real artifact_deployment row: pruning must not consult the registry: $prune2_revs4"; exit 1; }
echo "    gate 11: revision 1 is gone and exactly $ORBEAT_ARTIFACT_REVISION_KEEP revisions survive, even though a real deployment report names it: pruning ignores the registry"

echo "==> approving smoke-pin-prune once more (5 total) to set up gate 3's pruned-pin degradation"
prune2_rv5=$(current_row_version "$PIN_ADMIN_TOKEN" "$PRUNE2_ID")
prune2_put5_status=$(curl -s -o /dev/null -w '%{http_code}' -X PUT \
  -H "Authorization: Bearer $PIN_ADMIN_TOKEN" -H "Content-Type: application/json" \
  -H "If-Match: \"$prune2_rv5\"" \
  -d '{"type":"skill","name":"smoke-pin-prune","description":"d","content":"---\nname: smoke-pin-prune\ndescription: d\n---\nPIN-PRUNE-V5","visibility":"role"}' \
  "http://localhost:8080/v1/admin/artifacts/$PRUNE2_ID")
[ "$prune2_put5_status" = "200" ] || { echo "FAIL: edit smoke-pin-prune iteration 5 status $prune2_put5_status"; exit 1; }
prune2_submit5_status=$(curl -s -o /dev/null -w '%{http_code}' -X POST \
  -H "Authorization: Bearer $PIN_ADMIN_TOKEN" "http://localhost:8080/v1/admin/artifacts/$PRUNE2_ID/submit")
[ "$prune2_submit5_status" = "200" ] || { echo "FAIL: submit smoke-pin-prune iteration 5 status $prune2_submit5_status"; exit 1; }
prune2_rv5b=$(current_row_version "$PIN_ADMIN_TOKEN" "$PRUNE2_ID")
prune2_approve5_status=$(curl -s -o /dev/null -w '%{http_code}' -X POST \
  -H "Authorization: Bearer $PIN_BOSS2_TOKEN" -H "If-Match: \"$prune2_rv5b\"" \
  "http://localhost:8080/v1/admin/artifacts/$PRUNE2_ID/approve")
[ "$prune2_approve5_status" = "200" ] || { echo "FAIL: approve smoke-pin-prune iteration 5 status $prune2_approve5_status"; exit 1; }
PRUNE2_LATEST=$(latest_revision_num "$PIN_ADMIN_TOKEN" "$PRUNE2_ID")
PRUNE2_OLDEST=$((PRUNE2_LATEST - ORBEAT_ARTIFACT_REVISION_KEEP + 1))
echo "    smoke-pin-prune approved 5 times total, latest=$PRUNE2_LATEST"

echo "==> gate 3: the pin (still requesting revision 1, held from gate 11) now degrades to the oldest surviving revision ($PRUNE2_OLDEST), reason pruned"
rm -f "$SYNC_PIN/.claude/skills/smoke-pin-prune/SKILL.md"
seed_sync_token "$SYNC_PIN"
set +e
gate3_json=$(HOME="$SYNC_PIN" "$SYNC_BIN/orbeat-sync" sync --json 2>/dev/null)
gate3_rc=$?
set -e
[ "$gate3_rc" = "0" ] || { echo "FAIL: sync under the pruned pin exited $gate3_rc (want 0); output: $gate3_json"; exit 1; }
echo "$gate3_json" | jq -e . >/dev/null \
  || { echo "FAIL: sync --json emitted no parseable JSON for gate 3: $gate3_json"; exit 1; }
[ "$(echo "$gate3_json" | jq -r '.exitCode')" = "0" ] \
  || { echo "FAIL: gate 3's .exitCode was $(echo "$gate3_json" | jq -r '.exitCode'), want 0"; exit 1; }
# Selected by name, not asserted as the whole array: smoke-pin's own pin
# (gate 1/2, above) is still held, and this same sync still reports its
# floor override too, so .artifacts.pins carries two entries here.
echo "$gate3_json" | jq -e --argjson s "$PRUNE2_OLDEST" --arg n "skill/smoke-pin-prune" \
  '[.artifacts.pins[] | select(.name==$n)] == [{"name":$n,"requested":1,"served":$s,"reason":"pruned"}]' >/dev/null \
  || { echo "FAIL: .artifacts.pins did not report the pruned degradation to revision $PRUNE2_OLDEST; got $gate3_json"; exit 1; }
grep -q "PIN-PRUNE-V$PRUNE2_OLDEST" "$SYNC_PIN/.claude/skills/smoke-pin-prune/SKILL.md" \
  || { echo "FAIL: the served bytes are not revision $PRUNE2_OLDEST's (PIN-PRUNE-V$PRUNE2_OLDEST); output: $gate3_json"; exit 1; }
grep -q 'PIN-PRUNE-V1' "$SYNC_PIN/.claude/skills/smoke-pin-prune/SKILL.md" \
  && { echo "FAIL: the requested-but-pruned revision 1 content is on disk, not the degraded revision"; exit 1; }
prune2_revs_final=$(curl -s -H "Authorization: Bearer $PIN_ADMIN_TOKEN" "http://localhost:8080/v1/admin/artifacts/$PRUNE2_ID/revisions")
echo "$prune2_revs_final" | jq -e --argjson lo "$PRUNE2_OLDEST" --argjson hi "$PRUNE2_LATEST" \
  '([.revisions[].revision] | sort) == [$lo, ($lo+1), $hi]' >/dev/null \
  || { echo "FAIL: the surviving revisions are not the contiguous suffix $PRUNE2_OLDEST..$PRUNE2_LATEST; got $prune2_revs_final"; exit 1; }
echo "    gate 3: served bytes are revision $PRUNE2_OLDEST's, reason pruned, surviving revisions are the contiguous suffix $PRUNE2_OLDEST..$PRUNE2_LATEST"

echo "==> SMOKE PASS: api + gateway healthy, postgres up, Keycloak auth /v1/me 200 + no-token 401, /v1/catalog 200 + no-token 401, admin CRUD + entitlement→catalog + 403 RBAC + audit pagination + audit export (json/csv/400) + structured JSON logs + audit dual-emit, gateway /mcp 401 no-token + metadata + real upstream tool round-trip (smoke-upstream__echo), portal healthz + SPA shell, marketplace artifact validated, artifact publish round-trip, sync/config 200 + no-token 401, artifact approval gate (unapproved hidden from sync, boss2 approval distributes, self-approve 403), artifact rollback (v2 approved+distributed, roll back to rev 1 restores v1 content), revision pruning gate (KEEP=$ORBEAT_ARTIFACT_REVISION_KEEP enforced end-to-end through cmd/api: $PRUNE_APPROVALS approvals on a dedicated throwaway artifact leave exactly $ORBEAT_ARTIFACT_REVISION_KEEP revisions), rule artifact gate (Phase 3 Slice B: create → submit → boss2 approve → entitled rule present in sync with type:rule + verbatim content), org-visibility rule gate (an approved rule entitled to NOBODY reaches an unentitled user on Channel 2 and lands in their AGENTS.md via the real binary, while an APPROVED org skill stays off Channel 2), per-rule project targeting gate (a rule targeted at [go] reaches only the project registered with --tag go, with an untargeted rule reaching both as the control that keeps the negative assertion honest), global-scope rule gate (a global rule lands in ~/.claude/CLAUDE.md and in no project, with the project rule still in place as the control, and a global rule carrying targetTags refused with 400), orbeat-sync BINARY gate (the real client reconciles the live stack's artifacts: rule → AGENTS.md + CLAUDE.md import with dev content preserved, file-backed subagent delivered alongside it (the v1.14.0 regression), idempotent re-sync, and the v1.15.0 partial-failure contract: a broken project exits 1 with a 'failed:' line while healthy work still lands, plus the failure-path contract (partial --json with section-pinned failures, a repaired project retried, a corrupt manifest exiting 2 before any write, an escaping path exiting 2 after the writes with the cascade stopped and JSON still emitted, and --dry-run changing nothing), and orbeat-sync doctor --json on the restored healthy tree reporting exit 0 with zero problems), artifact identity through approval (renaming an approved artifact returns 200 and DEFERS: /v1/sync/artifacts and the real orbeat-sync binary both keep delivering agents/smoke-gov.md with the approved body while the rename is unapproved, then boss2 approves and the pair flips, the client moving the file to agents/smoke-gov-renamed.md), artifact deployment registry (ORBEAT_DEPLOYMENT_REGISTRY on for this run only: G1a the real binary's report recorded 1 install at smoke-gov's own revision $GOV_REV1; G1b a report carrying revision 1 was stored as 1, not collapsed onto the latest, and an empty report cleared exactly that install; G1c approval moved latestRevision to $GOV_REV2 with the unsynced machine still at $GOV_REV1 and behindLatest 1; G1d the re-sync closed it to $GOV_REV2 with behindLatest 0; G2 a second install whose unmanaged-file collision skipped smoke-gov recorded its rule and NOT the artifact it was served; G3 the corrupt-manifest run at exit 2 filed no report and left the aggregate byte-identical; G4 revoking the grant took the row with it, 0 installs on an artifact still observable at revision $GOV_REV2), artifact version pinning (gate 1 orbeat-sync pin subagent/smoke-pin --revision 1 serves PIN-V1-BODY and not PIN-V2-BODY, then an admin floor at revision $PIN_REV2 overrides it back to PIN-V2-BODY at exit 0 with a warning naming the pin, the served revision and floor; gate 2 --json on that same floor-overridden run carries .artifacts.pins == [{requested:1, served:$PIN_REV2, reason:floor}] with process rc and .exitCode both asserted, the override standing as exactly one warning line with .artifacts.failures empty (an override is never a retryable failure); gate 11 a real artifact_deployment row filed while revision 1 still existed did not save it from the next approval's prune, which left exactly $ORBEAT_ARTIFACT_REVISION_KEEP revisions with revision 1 gone; gate 3 the same held pin degraded to the oldest surviving revision $PRUNE2_OLDEST, reason pruned, once 5 approvals left the contiguous suffix $PRUNE2_OLDEST..$PRUNE2_LATEST)"
