#!/usr/bin/env bash
# Local production-stack smoke: brings up docker-compose.prod.yml with the
# prod-smoke override (build-from-source + Caddy internal TLS + *.localhost),
# and asserts the prod topology WITHOUT ACME or /etc/hosts (curl --resolve).
set -euo pipefail
cd "$(dirname "$0")/.."

ENVFILE="$(mktemp)"
cat > "$ENVFILE" <<'EOF'
ORBEAT_DOMAIN=orbeat.localhost
ACME_EMAIL=smoke@orbeat.localhost
POSTGRES_PASSWORD=smoke-postgres-pw
KC_BOOTSTRAP_ADMIN_USERNAME=admin
KC_BOOTSTRAP_ADMIN_PASSWORD=smoke-admin-pw
ORBEAT_BACKUP_INTERVAL=5
ORBEAT_BACKUP_KEEP=3
EOF
COMPOSE="docker compose -f deploy/docker-compose.prod.yml -f deploy/docker-compose.prod-smoke.yml --env-file $ENVFILE"

cleanup() {
  code=$?
  if [ $code -ne 0 ]; then
    echo "=== smoke-prod FAILED (exit $code) — container logs ==="
    $COMPOSE logs --tail=200 || true
  fi
  $COMPOSE down -v || true
  rm -f "$ENVFILE"
  exit $code
}
trap cleanup EXIT

echo "==> building + starting the prod stack (internal TLS)"
$COMPOSE up --build -d --wait

# --resolve maps the *.localhost host to 127.0.0.1 (no /etc/hosts); -k trusts
# Caddy's internal self-signed CA (this is a local smoke). Caddy has no
# healthcheck, so `up --wait` doesn't gate on it — --retry --retry-connrefused
# rides out the brief window where Caddy is provisioning its internal CA /
# binding :443 (avoids a CI false-negative on connection-refused).
RETRY="--retry 5 --retry-connrefused --retry-delay 2"
cget() { curl -sk $RETRY --resolve "$1:443:127.0.0.1" "https://$1$2"; }         # body
ccode() { curl -sk $RETRY -o /dev/null -w '%{http_code}' --resolve "$1:443:127.0.0.1" "https://$1$2"; }  # status

echo "==> portal /config.json returns prod-shaped URLs"
cfg="$(cget orbeat.localhost /config.json)"
echo "$cfg" | grep -q '"oidcAuthority":"https://auth.orbeat.localhost/realms/orbeat"' \
  || { echo "FAIL: /config.json oidcAuthority wrong: $cfg"; exit 1; }
echo "$cfg" | grep -q '"apiBase":"https://orbeat.localhost"' \
  || { echo "FAIL: /config.json apiBase wrong: $cfg"; exit 1; }
echo "$cfg" | grep -q '"gatewayUrl":"https://mcp.orbeat.localhost"' \
  || { echo "FAIL: /config.json gatewayUrl wrong: $cfg"; exit 1; }

echo "==> Caddy routes /v1/* to the api (expect 401 — proves the route lands on the api, not the portal catch-all)"
code="$(ccode orbeat.localhost /v1/catalog)"
[ "$code" = "401" ] || { echo "FAIL: /v1/catalog routed wrong (HTTP $code, want 401)"; exit 1; }

echo "==> Keycloak realm discovery on the auth. host"
cget auth.orbeat.localhost /realms/orbeat/.well-known/openid-configuration \
  | grep -q '"issuer":"https://auth.orbeat.localhost/realms/orbeat"' \
  || { echo "FAIL: keycloak discovery/issuer"; exit 1; }

# Keycloak's admin console is reachable on the public auth. host BY DESIGN —
# entry 17 of docs/threat-model.md, not an oversight. The auth. site block in
# deploy/caddy/Caddyfile is a bare `reverse_proxy keycloak:8080` with no path
# matcher because Keycloak must be browser-reachable for interactive SSO and
# the Dynamic Client Registration Claude Code requires, and its admin/asset
# paths are host-relative — exactly why the topology uses three subdomains
# instead of a path prefix on one. internal/deploy.TestAuthSiteProxiesKeycloakWithNoPathMatcher
# pins that shape in the config file; this probes that the RUNNING stack
# actually serves the console, which is a different claim (the house lesson:
# v1.20.0 shipped `cget >/dev/null`, which passes on a 404). Keycloak
# redirects /admin to its console app before it renders anything, so this
# follows that redirect and asserts the console page itself — a bare
# "got some HTTP response" check would be the exact vacuous shape this task
# exists to avoid.
#
# If this ever needs to fail here — a path matcher, an IP allow-list — that
# is a deliberate hardening change, and threat-model entry 17 must change in
# the same commit. This script does not gate that decision; it only proves
# the currently-accepted exposure is real, not just written down.
echo "==> Keycloak admin console reachable on the auth. host (accepted risk, threat-model entry 17 — see comment above)"
admin_code="$(ccode auth.orbeat.localhost /admin)"
[ "$admin_code" = "302" ] || { echo "FAIL: /admin HTTP $admin_code (want 302 — Keycloak's redirect into its console app)"; exit 1; }
admin_location="$(curl -sk $RETRY -o /dev/null -D - --resolve auth.orbeat.localhost:443:127.0.0.1 "https://auth.orbeat.localhost/admin" | tr -d '\r' | grep -i '^location:')"
case "$admin_location" in
  *"https://auth.orbeat.localhost/admin/master/console/"*) : ;;
  *) echo "FAIL: /admin redirected somewhere unexpected: $admin_location"; exit 1 ;;
esac
cget auth.orbeat.localhost /admin/master/console/ | grep -q "Keycloak Administration Console" \
  || { echo "FAIL: admin console page did not render (no 'Keycloak Administration Console' title)"; exit 1; }

echo "==> gateway RFC 9728 metadata reachable on the mcp. host (the topology-fix regression guard — must NOT 404)"
mcode="$(ccode mcp.orbeat.localhost /.well-known/oauth-protected-resource)"
[ "$mcode" = "200" ] || { echo "FAIL: gateway protected-resource metadata HTTP $mcode (want 200)"; exit 1; }
cget mcp.orbeat.localhost /.well-known/oauth-protected-resource | grep -q "mcp.orbeat.localhost" \
  || { echo "FAIL: gateway metadata resource does not reference mcp.orbeat.localhost"; exit 1; }

# Stack survives a postgres + keycloak restart cycle: Keycloak is restarted too
# (so it genuinely re-reads the realm from Postgres, not its in-memory cache) and
# we assert a real 200 via ccode — NOT `cget >/dev/null`, which passes even on a
# 404 (a lost realm) and would be a test that cannot fail. Strict volume
# durability is guaranteed by the declared `orbeat-pgdata` named volume (verified
# by `docker compose config`); this asserts the runtime resilience half.
echo "==> stack recovers after a postgres + keycloak restart (realm still served)"
$COMPOSE restart postgres keycloak
$COMPOSE up -d --wait
rcode="$(ccode auth.orbeat.localhost /realms/orbeat/.well-known/openid-configuration)"
[ "$rcode" = "200" ] || { echo "FAIL: realm not served after restart (HTTP $rcode)"; exit 1; }

echo "==> backup sidecar produces a restorable dump (restore into a scratch DB)"
# Wait (<=60s) for the sidecar's first dump in the orbeat-backups volume.
dump=""
for i in $(seq 1 20); do
  dump="$($COMPOSE exec -T backup sh -c 'ls -1t /backups/orbeat-*.dump 2>/dev/null | head -1' | tr -d "\r")"
  [ -n "$dump" ] && break
  sleep 3
done
[ -n "$dump" ] || { echo "FAIL: no backup dump produced"; exit 1; }
echo "    found dump: $dump"

# Restore it into a scratch database and verify a known row (the seeded tenant).
$COMPOSE exec -T postgres psql -U orbeat -d postgres -c "DROP DATABASE IF EXISTS orbeat_restore_test;" -c "CREATE DATABASE orbeat_restore_test;"
# pg_restore runs in the backup container (it has pg_restore + the /backups volume + PG* env).
$COMPOSE exec -T backup sh -c "pg_restore -d orbeat_restore_test '$dump'" || true  # benign notices may exit non-zero; the row check is the gate
tenants="$($COMPOSE exec -T postgres psql -U orbeat -d orbeat_restore_test -tAc 'SELECT count(*) FROM tenant;' | tr -d '\r ')"
$COMPOSE exec -T postgres psql -U orbeat -d postgres -c "DROP DATABASE IF EXISTS orbeat_restore_test;"
case "$tenants" in
  ''|*[!0-9]*) echo "FAIL: could not count tenants in restored scratch DB (got '$tenants')"; exit 1 ;;
esac
[ "$tenants" -ge 1 ] || { echo "FAIL: restored scratch DB has no tenant rows — dump not restorable"; exit 1; }
echo "    restore verified: $tenants tenant row(s) in the scratch DB"

echo "==> smoke-prod PASS"
