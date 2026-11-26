#!/bin/sh
# Nightly Postgres backup.
#   backup.sh        loop forever: one backup a day at BACKUP_HOUR_UTC
#   backup.sh once   run one backup now and exit (non-zero on failure)
#
# Layout under $BACKUP_REMOTE:  daily/karyakita-YYYYmmdd-HHMMSS.dump  (newest $BACKUP_KEEP_DAILY kept)
#                               weekly/...                            (Sunday's dump; newest $BACKUP_KEEP_WEEKLY kept)
# Dumps are pg_dump custom format (-Fc); restore with pg_restore, see deploy/README.md.
# PG* connection vars and the rclone remote come from the environment.
set -u

: "${BACKUP_REMOTE:?BACKUP_REMOTE is not set}"
HOUR="${BACKUP_HOUR_UTC:-19}"
KEEP_DAILY="${BACKUP_KEEP_DAILY:-14}"
KEEP_WEEKLY="${BACKUP_KEEP_WEEKLY:-8}"
REMOTE="${BACKUP_REMOTE%/}"

log() { echo "$(date -u +%FT%TZ) backup: $*"; }

# $1 = folder, $2 = how many newest dumps to keep
prune() {
  rclone lsf --files-only "$REMOTE/$1" 2>/dev/null | grep '^karyakita-.*\.dump$' | sort -r | tail -n +"$(($2 + 1))" |
    while read -r f; do
      log "prune $1/$f"
      rclone deletefile "$REMOTE/$1/$f" || log "could not delete $1/$f"
    done
}

ping_monitor() { # $1 = "" on success, "/fail" on failure (healthchecks.io style dead-man's switch)
  [ -n "${BACKUP_PING_URL:-}" ] && wget -q -T 10 -O /dev/null "${BACKUP_PING_URL%/}$1" || true
}

run_backup() {
  ts="$(date -u +%Y%m%d-%H%M%S)"
  name="karyakita-$ts.dump"
  tmp="/tmp/$name"
  log "dumping $PGDATABASE@$PGHOST"
  if ! pg_dump -Fc -f "$tmp"; then log "pg_dump FAILED"; rm -f "$tmp"; return 1; fi
  # a dump pg_restore cannot even list is worthless: catch that tonight, not on restore day
  if ! pg_restore --list "$tmp" >/dev/null; then log "dump is unreadable"; rm -f "$tmp"; return 1; fi
  if ! rclone copyto "$tmp" "$REMOTE/daily/$name"; then log "upload FAILED"; rm -f "$tmp"; return 1; fi
  log "uploaded daily/$name ($(wc -c <"$tmp") bytes)"
  rm -f "$tmp"
  if [ "$(date -u +%u)" = 7 ]; then # Sunday
    rclone copyto "$REMOTE/daily/$name" "$REMOTE/weekly/$name" && log "copied to weekly/$name" || log "weekly copy FAILED"
  fi
  prune daily "$KEEP_DAILY"
  prune weekly "$KEEP_WEEKLY"
}

if [ "${1:-}" = once ]; then
  if run_backup; then ping_monitor ""; exit 0; fi
  ping_monitor /fail
  exit 1
fi

log "scheduled daily at ${HOUR}:00 UTC -> $REMOTE (keep $KEEP_DAILY daily + $KEEP_WEEKLY weekly)"
while :; do
  cur=$(($(date -u +%s) % 86400))
  wait=$(((HOUR * 3600 - cur + 86400) % 86400))
  [ "$wait" -eq 0 ] && wait=86400
  sleep "$wait"
  if run_backup; then ping_monitor ""; else ping_monitor /fail; fi
done
