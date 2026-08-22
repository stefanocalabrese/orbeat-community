#!/bin/sh
# Scheduled Postgres logical backup for the orbeat prod stack. Runs pg_dump in a
# loop, keeping the newest $ORBEAT_BACKUP_KEEP compressed custom-format dumps in
# /backups (a named volume). Connection via the standard PG* env vars.
set -eu

INTERVAL="${ORBEAT_BACKUP_INTERVAL:-86400}"
KEEP="${ORBEAT_BACKUP_KEEP:-7}"
DIR=/backups

mkdir -p "$DIR"
echo "orbeat backup: interval=${INTERVAL}s keep=${KEEP} dir=${DIR}"

while true; do
  ts="$(date -u +%Y%m%dT%H%M%SZ)"
  out="$DIR/orbeat-$ts.dump"
  if pg_dump -Fc -f "$out"; then
    echo "orbeat backup: wrote $out"
    # Rotate: keep the newest $KEEP orbeat-*.dump, delete the rest. Never runs on
    # a failed dump (a bad run must not delete a good backup).
    ls -1t "$DIR"/orbeat-*.dump 2>/dev/null | tail -n +"$((KEEP + 1))" | while IFS= read -r old; do
      rm -f "$old" && echo "orbeat backup: rotated out $old"
    done
  else
    echo "orbeat backup: pg_dump FAILED (keeping existing backups)" >&2
  fi
  sleep "$INTERVAL"
done
