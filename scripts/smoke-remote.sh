#!/usr/bin/env bash
# Remote-target marketplace publisher smoke: the failure→recovery gate for the
# v1.16.1 push-gate fix.
#
# The default `make smoke` publishes commit-in-place to a LOCAL path, so the
# publisher's remote path (fetch → reset → push) never runs there. This script
# points the api at a real git:// server and drives the exact regression the
# fix addresses, entirely through the real HTTP admin surface:
#
#   1. approve content → assert it reaches the remote        (remote push works)
#   2. stop the git server, approve more → assert honest red  (failure recorded,
#      last good commit PRESERVED, not wiped)                  + content stranded
#   3. restart the server, click Republish → assert the       (recovery — the
#      stranded content now reaches the remote                  ex-"guaranteed
#                                                                no-op" button)
#
# The load-bearing assertions read the REMOTE's actual content (git clone), not
# just the status endpoint — because the bug's signature is a false-green status
# with stale remote content. A status-only check could not tell them apart.
set -euo pipefail

COMPOSE="docker compose -f deploy/docker-compose.yml -f deploy/docker-compose.remote-smoke.yml"
API=http://localhost:8080
KC=http://localhost:8088
GIT_REMOTE=git://localhost:9418/marketplace.git

cleanup() {
  rc=$?
  if [ "$rc" != "0" ]; then
    echo "==> smoke-remote FAILED (exit $rc) — recent stack logs:"
    $COMPOSE logs --tail=120 2>&1 | sed 's/^/    | /' || true
  fi
  [ -n "${CLONE_DIR:-}" ] && rm -rf "$CLONE_DIR"
  $COMPOSE down -v >/dev/null 2>&1 || true
}
trap cleanup EXIT

fail() { echo "FAIL: $*" >&2; exit 1; }

# tok <user> — a FRESH access token (password grant). Fetched per phase because
# Keycloak access tokens are short-lived and the phases have real-time waits.
# `// empty` (not a bare .access_token, which yields the literal "null" on an
# error response) + a non-empty guard: an auth failure fails the run HERE rather
# than sending `Bearer null` downstream to surface as a confusing 401. Every
# call site is `VAR=$(tok …)`, so this non-zero return propagates via `set -e`.
tok() {
  local t
  t=$(curl -s -d grant_type=password -d client_id=orbeat-cli \
    -d "username=$1" -d "password=$1" \
    "$KC/realms/orbeat/protocol/openid-connect/token" | jq -r '.access_token // empty')
  [ -n "$t" ] || { echo "FAIL: empty access token for user '$1' (Keycloak auth failed?)" >&2; return 1; }
  printf '%s' "$t"
}

# status <field> — one field of GET /v1/admin/marketplace/status.
status() {
  curl -s -H "Authorization: Bearer $BOSS" "$API/v1/admin/marketplace/status" | jq -r ".$1 // \"\""
}

# clone_remote — fresh clone of the git:// remote into $CLONE_DIR.
CLONE_DIR=""
clone_remote() {
  [ -n "$CLONE_DIR" ] && rm -rf "$CLONE_DIR"
  CLONE_DIR=$(mktemp -d)
  git clone -q "$GIT_REMOTE" "$CLONE_DIR" 2>/dev/null
}

SKILL_PATH() { echo "plugins/orbeat-artifacts/skills/$1/SKILL.md"; }

# http_ok <label> <expected> <curl-args...> — run curl, fail unless the HTTP
# status equals <expected>, so a 4xx/5xx is localized here, not downstream.
http_ok() {
  local label="$1" want="$2"; shift 2
  local code
  code=$(curl -s -o /dev/null -w '%{http_code}' "$@")
  [ "$code" = "$want" ] || fail "$label: HTTP $code (want $want)"
}

# create_approved <name> — create (boss) → submit (boss) → approve (boss2).
# Approval is a committed DB transaction independent of the async publish, so it
# succeeds even while the git server is down. Echoes nothing; exits on error.
create_approved() {
  local name="$1" boss boss2 resp id rv
  boss=$(tok boss); boss2=$(tok boss2)
  resp=$(curl -s -X POST -H "Authorization: Bearer $boss" -H "Content-Type: application/json" \
    -d "{\"type\":\"skill\",\"name\":\"$name\",\"description\":\"d\",\"content\":\"---\\nname: $name\\ndescription: d\\n---\\n$name body\"}" \
    "$API/v1/admin/artifacts")
  id=$(echo "$resp" | jq -r '.id // empty')
  [ -n "$id" ] || fail "create $name: $resp"
  http_ok "submit $name"  200 -X POST -H "Authorization: Bearer $boss"  "$API/v1/admin/artifacts/$id/submit"
  # approve now enforces If-Match (optimistic concurrency, spec
  # 2026-08-11-orbeat-optimistic-concurrency-design.md §5, §2): submit above
  # bumped row_version via the trigger, so fetch the current value fresh.
  rv=$(curl -s -H "Authorization: Bearer $boss" "$API/v1/admin/artifacts/$id" | jq -r '.rowVersion // empty')
  [ -n "$rv" ] || fail "resolve rowVersion for $name ($id)"
  http_ok "approve $name" 200 -X POST -H "Authorization: Bearer $boss2" -H "If-Match: \"$rv\"" "$API/v1/admin/artifacts/$id/approve"
}

# try_wait_field_changes <field> <prev> [halfsec-iterations] — poll until a status
# field advances past a captured value (a publish run recorded its outcome).
# Returns 0 on change, 1 on timeout (no exit). Polling the actual outcome
# (lastCommit on success, lastError on failure) rather than merely lastAttemptAt
# ties the wait to the assertion and removes any debounce-race doubt.
try_wait_field_changes() {
  local field="$1" prev="$2" iters="${3:-40}" now
  for _ in $(seq 1 "$iters"); do
    now=$(status "$field")
    [ "$now" != "$prev" ] && return 0
    sleep 0.5
  done
  return 1
}

# wait_field_changes <field> <prev> — as above, but fails the run on timeout.
wait_field_changes() {
  try_wait_field_changes "$1" "$2" || fail "status.$1 did not change within 20s (was: '$2')"
}

# wait_gitserver_healthy — block until the gitserver container's healthcheck
# reports healthy (the daemon is up AND serving), up to ~30s. More robust than a
# one-shot probe after a restart, and gives the daemon time to stabilise.
wait_gitserver_healthy() {
  local gsid; gsid=$($COMPOSE ps -q gitserver)
  [ -n "$gsid" ] || fail "gitserver container not found"
  for _ in $(seq 1 30); do
    [ "$(docker inspect -f '{{.State.Health.Status}}' "$gsid" 2>/dev/null)" = "healthy" ] && return 0
    sleep 1
  done
  fail "gitserver did not become healthy after restart"
}

# wait_gitserver_unreachable — block until the remote no longer answers, up to
# ~30s. `docker compose stop` returns before the daemon has actually stopped
# accepting connections, so without this the next publish can race in while the
# server is still up and push its content — defeating the failure injection.
wait_gitserver_unreachable() {
  for _ in $(seq 1 30); do
    git ls-remote "$GIT_REMOTE" >/dev/null 2>&1 || return 0
    sleep 1
  done
  fail "gitserver still reachable after stop"
}

# wait_publisher_quiescent — block until no publish has run for ~3s (lastAttemptAt
# stable), so the publisher has fully settled. A single create+submit+approve
# fires TWO publishes (create AND approve each enqueue); publishes run fast (they
# fail immediately while the server is down), so a stable window longer than the
# 750ms debounce means nothing is still pending. Without this, a beta publish left
# pending during the "down" window completes AFTER the server is healed and pushes
# beta before the Republish — a false recovery (the publisher git ops have no
# timeout, so a pending publish survives the whole down→up window).
wait_publisher_quiescent() {
  local prev="" now stable=0
  for _ in $(seq 1 60); do
    now=$(status lastAttemptAt)
    if [ -n "$prev" ] && [ "$now" = "$prev" ]; then
      stable=$((stable + 1)); [ "$stable" -ge 6 ] && return 0
    else
      stable=0
    fi
    prev="$now"
    sleep 0.5
  done
  fail "publisher did not settle within 30s"
}

# ── bring up the stack in remote mode ─────────────────────────────────────────
echo "==> bringing up stack (remote git:// marketplace target)"
$COMPOSE up --build -d

echo "==> waiting for api health"
for _ in $(seq 1 60); do
  curl -fsS "$API/healthz" >/dev/null 2>&1 && break || sleep 2
done
curl -fsS "$API/healthz" >/dev/null 2>&1 || fail "api never became healthy"
BOSS=$(tok boss)
[ -n "$BOSS" ] && [ "$BOSS" != "null" ] || fail "could not obtain admin token"

# ── Phase 1: a healthy publish reaches the remote ─────────────────────────────
echo "==> [1] approve 'alpha' → it must reach the git:// remote"
PREV_COMMIT=$(status lastCommit)
create_approved alpha
wait_field_changes lastCommit "$PREV_COMMIT" # healthy publish advances the commit
wait_publisher_quiescent                     # let both create+approve publishes settle before the next phase
BOSS=$(tok boss)
[ -z "$(status lastError)" ] || fail "healthy publish reported an error: $(status lastError)"
ALPHA_COMMIT=$(status lastCommit)
[ -n "$ALPHA_COMMIT" ] || fail "no lastCommit after healthy publish"
clone_remote
[ -f "$CLONE_DIR/$(SKILL_PATH alpha)" ] || fail "alpha did not reach the remote"
echo "    alpha on remote; lastCommit=$ALPHA_COMMIT lastError=''"

# ── Phase 2: a failed push is honest and preserves the last good commit ───────
echo "==> [2] stop git server, approve 'beta' → publish must fail honestly"
$COMPOSE stop gitserver >/dev/null
wait_gitserver_unreachable # ensure the server is truly down before beta's publish, so it must fail
create_approved beta
wait_field_changes lastError "" # a failed publish records a non-empty error
wait_publisher_quiescent        # drain BOTH beta publishes (create+approve) so none is left pending to push beta after the heal
BOSS=$(tok boss)
ERR=$(status lastError)
[ -n "$ERR" ] || fail "failed publish did NOT record an error (false green)"
PRESERVED=$(status lastCommit)
[ "$PRESERVED" = "$ALPHA_COMMIT" ] || fail "failed publish moved lastCommit: $ALPHA_COMMIT → $PRESERVED (should be preserved)"
echo "    honest red: lastError set, lastCommit preserved at $PRESERVED"

# ── Phase 3: heal + Republish recovers the stranded content ───────────────────
echo "==> [3] restart git server; beta must still be stranded until Republish"
$COMPOSE start gitserver >/dev/null
wait_gitserver_healthy
clone_remote
[ ! -f "$CLONE_DIR/$(SKILL_PATH beta)" ] || fail "beta reached the remote without a republish (unexpected)"

echo "==> [3] click Republish → beta must now reach the remote"
PREV_COMMIT=$(status lastCommit)
# The git server was just restarted; Republish is the operator's recovery action.
# Retry it a few times so a momentary post-restart hiccup can't flake the gate — a
# genuine push-gate regression fails EVERY attempt (beta never reaches the remote),
# so the retry cannot mask a real bug.
recovered=0
for attempt in 1 2 3; do
  BOSS=$(tok boss)
  http_ok "republish (attempt $attempt)" 202 -X POST -H "Authorization: Bearer $BOSS" "$API/v1/admin/marketplace/publish"
  if try_wait_field_changes lastCommit "$PREV_COMMIT" 24; then recovered=1; break; fi
  echo "    attempt $attempt did not advance the commit; retrying"
done
[ "$recovered" = 1 ] || fail "RECOVERY FAILED: Republish did not advance the commit after 3 attempts"
BOSS=$(tok boss)
[ -z "$(status lastError)" ] || fail "republish still reported an error: $(status lastError)"
clone_remote
[ -f "$CLONE_DIR/$(SKILL_PATH beta)"  ] || fail "RECOVERY FAILED: beta never reached the remote after Republish"
[ -f "$CLONE_DIR/$(SKILL_PATH alpha)" ] || fail "alpha vanished from the remote after Republish"
echo "    recovered: alpha + beta on remote; lastCommit=$(status lastCommit) lastError=''"

echo "==> smoke-remote PASSED"
