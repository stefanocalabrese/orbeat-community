#!/bin/sh
# Scheduled Postgres logical backup for the orbeat prod stack. Runs pg_dump in a
# loop over EVERY database this stack owns, keeping the newest
# $ORBEAT_BACKUP_KEEP backup sets in /backups (a named volume). Connection via
# the standard PG* env vars.
#
# TWO DATABASES, ONE SERVER (audit A13). Until 2026-08-30 this script ran a bare
# `pg_dump -Fc -f "$out"`, which dumps PGDATABASE and nothing else, so Keycloak's
# database was never in a backup at all. Restoring from such a backup after host
# loss brings the catalog, RBAC, artifacts and audit back while the identity
# store stays empty: `--import-realm` recreates the realm TEMPLATE, so what is
# gone is everything created after import, every SSO-provisioned user (and the
# users.id every entitlement, audit_event and artifact_deployment row references),
# every Claude Code client registered through DCR, and every Keycloak client
# behind a virtual key.
#
# A SET IS ALL-OR-NOTHING, deliberately. Each run dumps into a hidden working
# directory and renames it into place only after every database has succeeded,
# so a directory named orbeat-<timestamp> always holds a COMPLETE set. Restoring
# one of two databases is worse than restoring neither, because it looks like it
# worked; a half-written set that a later restore could pick up would be exactly
# that failure, arriving by accident.
set -eu

INTERVAL="${ORBEAT_BACKUP_INTERVAL:-86400}"
KEEP="${ORBEAT_BACKUP_KEEP:-7}"
DIR=/backups

# The databases to dump, both on this same server: the orbeat application
# database (PGDATABASE, pinned to `orbeat` by docker-compose.prod.yml) and
# Keycloak's, created by deploy/postgres/initdb/10-keycloak-db.sql and named in
# the compose file's KC_DB_URL. Written out rather than discovered from
# pg_database so that a database appearing on this server for some other reason
# does not silently join the backup set.
DATABASES="${PGDATABASE:-orbeat} keycloak"

# ORBEAT_BACKUP_KEEP must be a number, and 0 must mean "keep forever" (audit
# B10). It used to reach `tail -n +"$((KEEP + 1))"` unchecked, where 0 becomes
# `tail -n +1`, every dump, including the one just written, deleted on the
# first successful run. The other four retention knobs in this repo
# (ORBEAT_AUDIT_RETENTION_DAYS, ORBEAT_DEPLOYMENT_RETENTION_DAYS,
# ORBEAT_USAGE_RETENTION_DAYS, ORBEAT_ARTIFACT_REVISION_KEEP) all read 0 as
# "keep everything", so an operator carrying that convention across erased her
# backups. A non-numeric value was the same defect by another route: in POSIX
# sh arithmetic an unparseable value evaluates to 0.
case "$KEEP" in
'' | *[!0-9]*)
	echo "orbeat backup: ORBEAT_BACKUP_KEEP must be a non-negative integer, got '$KEEP'" >&2
	exit 1
	;;
esac

# ORBEAT_BACKUP_INTERVAL takes the same check, and needs it more. It reaches
# `sleep "$INTERVAL"` at the bottom of the loop, where busybox sleep refuses a
# non-numeric argument and returns non-zero; `set -e` then kills the script,
# and `restart: unless-stopped` brings it straight back, so a typo turns the
# sidecar into a dump loop that runs pg_dump against the live server as fast as
# the container can restart. 0 is permitted and means exactly what it says,
# back up continuously; it is a strange thing to ask for but it is not
# ambiguous, unlike KEEP=0, which had to be given a meaning.
case "$INTERVAL" in
'' | *[!0-9]*)
	echo "orbeat backup: ORBEAT_BACKUP_INTERVAL must be a non-negative integer number of seconds, got '$INTERVAL'" >&2
	exit 1
	;;
esac

mkdir -p "$DIR"
if [ "$KEEP" -eq 0 ]; then
	echo "orbeat backup: interval=${INTERVAL}s keep=0 (rotation OFF, every set is kept) dir=${DIR} databases='${DATABASES}'"
else
	echo "orbeat backup: interval=${INTERVAL}s keep=${KEEP} dir=${DIR} databases='${DATABASES}'"
fi

while true; do
	ts="$(date -u +%Y%m%dT%H%M%SZ)"
	work="$DIR/.partial-$ts"
	final="$DIR/orbeat-$ts"

	# Clear any working directory a killed run left behind. The `.partial-`
	# prefix keeps these out of the orbeat-* glob rotation and restores use,
	# so an interrupted run can never be mistaken for a backup.
	rm -rf "$DIR"/.partial-*
	mkdir -p "$work"

	complete=yes
	for db in $DATABASES; do
		if ! pg_dump -Fc -d "$db" -f "$work/$db.dump"; then
			echo "orbeat backup: pg_dump of database '$db' FAILED (keeping existing backups)" >&2
			complete=no
			break
		fi
	done

	if [ "$complete" = yes ]; then
		# A set name is second-granular, so two runs inside one second collide.
		# `mv` into an EXISTING directory does not fail, it nests, and it keeps
		# the SOURCE's name: measured, the second run produces
		# orbeat-<ts>/.partial-<ts>/*.dump and still logs "wrote
		# /backups/orbeat-<ts>". A dot-prefixed directory inside a set is
		# invisible to the `ls -1t /backups` the runbook's restore procedure
		# uses, so the operator sees one healthy set, a success line, and a
		# second run's dumps stashed where nothing will ever read them.
		# Refusing is the only honest outcome, since the set already there is
		# complete and this one has nothing to add.
		#
		# `mv -T` is NOT the tool for this, measured rather than assumed: on
		# busybox (postgres:18-alpine, the image this runs in) it refuses a
		# non-empty destination correctly, but BSD mv answers `illegal option
		# -- T` and exits 64, so any host that runs this script outside the
		# container, this repo's own gate included, would break on the flag
		# rather than on the condition. `mv -n` is worse than useless here: it
		# was measured NESTING silently and exiting 0, which is the exact
		# failure it looks like it prevents.
		if [ -e "$final" ]; then
			echo "orbeat backup: $final already exists, refusing to write a second set under the same timestamp (keeping the existing one)" >&2
			rm -rf "$work"
			sleep "$INTERVAL"
			continue
		fi
		mv "$work" "$final"
		echo "orbeat backup: wrote $final ($DATABASES)"
		# Rotate: keep the newest $KEEP sets, delete the rest. Never runs
		# on a failed dump (a bad run must not delete a good backup), and
		# never runs at all when KEEP is 0.
		if [ "$KEEP" -gt 0 ]; then
			ls -1dt "$DIR"/orbeat-* 2>/dev/null | tail -n +"$((KEEP + 1))" | while IFS= read -r old; do
				rm -rf "$old" && echo "orbeat backup: rotated out $old"
			done
		fi
	else
		rm -rf "$work"
	fi
	sleep "$INTERVAL"
done
