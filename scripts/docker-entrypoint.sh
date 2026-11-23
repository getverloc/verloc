#!/bin/sh
# Container entrypoint for verloc: prepares private storage permissions and
# writes an initial bootstrap.json when none exists, then execs verloc as PID 1.
set -eu

DATA_DIR="${VERLOC_DATA_DIR:-/var/lib/verloc}"
STATE_DIR="${DATA_DIR}/verloc-data"
CONFIG_FILE="${VERLOC_CONFIG:-${DATA_DIR}/bootstrap.json}"
LISTEN="${VERLOC_LISTEN:-:8080}"
SECURE_COOKIES="${VERLOC_SECURE_COOKIES:-false}"

umask 077
mkdir -p "$DATA_DIR" "$STATE_DIR"
chmod 700 "$DATA_DIR" "$STATE_DIR"

json_escape() {
  printf '%s' "$1" | sed 's/\\/\\\\/g; s/"/\\"/g'
}

json_string_array() {
  # Convert a comma-separated string into a JSON array of trimmed strings.
  raw="$1"
  out="["
  first=1
  old_ifs="$IFS"
  IFS=','
  for item in $raw; do
    trimmed="$(printf '%s' "$item" | sed 's/^[[:space:]]*//; s/[[:space:]]*$//')"
    [ -n "$trimmed" ] || continue
    if [ "$first" -eq 0 ]; then
      out="${out}, "
    fi
    out="${out}\"$(json_escape "$trimmed")\""
    first=0
  done
  IFS="$old_ifs"
  printf '%s]' "$out"
}

if [ ! -f "$CONFIG_FILE" ]; then
  case "$SECURE_COOKIES" in
    true|TRUE|1|yes|YES) secure_json="true" ;;
    *) secure_json="false" ;;
  esac

  extra=""
  if [ -n "${VERLOC_ALLOWED_HOSTS:-}" ]; then
    extra="${extra}
  \"allowed_hosts\": $(json_string_array "$VERLOC_ALLOWED_HOSTS"),"
  fi
  if [ -n "${VERLOC_TRUSTED_PROXIES:-}" ]; then
    extra="${extra}
  \"trusted_proxies\": $(json_string_array "$VERLOC_TRUSTED_PROXIES"),"
  fi

  cat > "$CONFIG_FILE" <<EOF
{
  "listen": "$(json_escape "$LISTEN")",
  "management_db": "$(json_escape "${STATE_DIR}/control.db")",
  "master_key_file": "$(json_escape "${STATE_DIR}/master.key")",
  "setup_token_file": "$(json_escape "${STATE_DIR}/setup.token")",
  "secure_cookies": ${secure_json},${extra}
  "dashboard": {
    "db_path": "$(json_escape "${DATA_DIR}/analytics.db")"
  }
}
EOF
fi

if [ "$#" -gt 0 ]; then
  case "$1" in
    -*)
      exec /usr/local/bin/verloc "$@"
      ;;
    *)
      exec "$@"
      ;;
  esac
fi

exec /usr/local/bin/verloc -config "$CONFIG_FILE"
