#!/bin/sh
# verloc one-command installer and updater.
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/getverloc/verloc/master/install.sh | sh
#   curl -fsSL https://raw.githubusercontent.com/getverloc/verloc/master/install.sh | sh -s -- --service
#   ./install.sh [options]
#
# Options:
#   --dir <path>        Runtime data directory (default: ~/.local/share/verloc, or /var/lib/verloc with --system)
#   --bin-dir <path>    Binary install directory (default: ~/.local/bin, or /usr/local/bin with --system)
#   --listen <addr>     Listen address for initial bootstrap.json (default: 127.0.0.1:8080)
#   --port <port>       Shorthand for --listen 127.0.0.1:<port>
#   --version <tag>     Release tag to install (default: latest)
#   --from-source       Build from source instead of downloading a release binary
#   --build-dash        When building from source, rebuild the React dashboard with npm
#   --service           Install and start a background service (systemd user unit on Linux, launchd on macOS)
#   --start             Start verloc in the foreground after installation (ignored with --service)
#   --system            System-wide Linux install (/usr/local/bin, /etc/verloc, /var/lib/verloc, systemd service)
#   --uninstall         Stop service and remove binary/service unit (preserves data unless --purge is also set)
#   --purge             With --uninstall, also delete configuration and data directories
#   -h, --help          Show this help message
set -eu

REPO="${VERLOC_REPO:-getverloc/verloc}"
BRANCH="${VERLOC_BRANCH:-master}"
VERSION="${VERLOC_VERSION:-latest}"
LISTEN="${VERLOC_LISTEN:-}"
PORT="${VERLOC_PORT:-}"
DATA_DIR="${VERLOC_DIR:-}"
BIN_DIR="${VERLOC_BIN_DIR:-}"
FROM_SOURCE=0
BUILD_DASH=0
INSTALL_SERVICE=0
START_NOW=0
SYSTEM_INSTALL=0
UNINSTALL=0
PURGE=0
TMP_DIR=""

cleanup() {
  if [ -n "$TMP_DIR" ] && [ -d "$TMP_DIR" ]; then
    rm -rf "$TMP_DIR"
  fi
}
trap cleanup EXIT INT TERM

log() {
  printf '==> %s\n' "$*"
}

warn() {
  printf 'warning: %s\n' "$*" >&2
}

die() {
  printf 'error: %s\n' "$*" >&2
  exit 1
}

usage() {
  sed -n '2,23p' "$0" 2>/dev/null || cat <<'EOF'
verloc one-command installer and updater.

Usage:
  curl -fsSL https://raw.githubusercontent.com/getverloc/verloc/master/install.sh | sh
  curl -fsSL https://raw.githubusercontent.com/getverloc/verloc/master/install.sh | sh -s -- --service
  ./install.sh [--dir PATH] [--bin-dir PATH] [--port PORT] [--listen ADDR] [--service] [--start] [--system] [--from-source] [--build-dash] [--uninstall [--purge]]
EOF
}

need_cmd() {
  command -v "$1" >/dev/null 2>&1 || die "required command not found: $1"
}

json_escape() {
  printf '%s' "$1" | sed 's/\\/\\\\/g; s/"/\\"/g'
}

parse_args() {
  while [ $# -gt 0 ]; do
    case "$1" in
      --dir)
        [ $# -ge 2 ] || die "--dir requires a path"
        DATA_DIR="$2"
        shift 2
        ;;
      --bin-dir)
        [ $# -ge 2 ] || die "--bin-dir requires a path"
        BIN_DIR="$2"
        shift 2
        ;;
      --listen)
        [ $# -ge 2 ] || die "--listen requires an address (e.g. 127.0.0.1:8080)"
        LISTEN="$2"
        shift 2
        ;;
      --port)
        [ $# -ge 2 ] || die "--port requires a port number"
        PORT="$2"
        shift 2
        ;;
      --version)
        [ $# -ge 2 ] || die "--version requires a tag (e.g. v1.0.0)"
        VERSION="$2"
        shift 2
        ;;
      --from-source)
        FROM_SOURCE=1
        shift
        ;;
      --build-dash)
        BUILD_DASH=1
        FROM_SOURCE=1
        shift
        ;;
      --service)
        INSTALL_SERVICE=1
        shift
        ;;
      --start)
        START_NOW=1
        shift
        ;;
      --system)
        SYSTEM_INSTALL=1
        INSTALL_SERVICE=1
        shift
        ;;
      --uninstall)
        UNINSTALL=1
        shift
        ;;
      --purge)
        PURGE=1
        shift
        ;;
      -y|--yes)
        shift
        ;;
      -h|--help)
        usage
        exit 0
        ;;
      *)
        die "unknown option: $1 (use --help for usage)"
        ;;
    esac
  done
}

detect_platform() {
  uname_s="$(uname -s)"
  uname_m="$(uname -m)"
  case "$uname_s" in
    Linux)  OS="linux" ;;
    Darwin) OS="darwin" ;;
    *)      die "unsupported OS: $uname_s (supported: Linux, macOS)" ;;
  esac
  case "$uname_m" in
    x86_64|amd64)   ARCH="amd64" ;;
    arm64|aarch64)  ARCH="arm64" ;;
    *)              die "unsupported architecture: $uname_m (supported: amd64, arm64)" ;;
  esac
}

validate_listen() {
  if [ -n "$PORT" ] && [ -n "$LISTEN" ]; then
    die "specify either --port or --listen, not both"
  fi
  if [ -n "$PORT" ]; then
    case "$PORT" in
      ''|*[!0-9]*) die "invalid port: $PORT" ;;
    esac
    if [ "$PORT" -lt 1 ] || [ "$PORT" -gt 65535 ]; then
      die "port must be between 1 and 65535"
    fi
    LISTEN="127.0.0.1:${PORT}"
  fi
  if [ -z "$LISTEN" ]; then
    LISTEN="127.0.0.1:8080"
  fi
}

resolve_paths() {
  if [ "$SYSTEM_INSTALL" -eq 1 ]; then
    [ "$OS" = "linux" ] || die "--system is only supported on Linux with systemd"
    [ "$(id -u)" -eq 0 ] || die "--system requires root privileges (re-run with sudo)"
    [ -n "$BIN_DIR" ] || BIN_DIR="/usr/local/bin"
    [ -n "$DATA_DIR" ] || DATA_DIR="/var/lib/verloc"
    CONFIG_DIR="/etc/verloc"
    CONFIG_FILE="${CONFIG_DIR}/bootstrap.json"
    STATE_DIR="${DATA_DIR}"
    ANALYTICS_DB="${DATA_DIR}/analytics.db"
  else
    [ -n "$BIN_DIR" ] || BIN_DIR="${HOME}/.local/bin"
    [ -n "$DATA_DIR" ] || DATA_DIR="${HOME}/.local/share/verloc"
    CONFIG_DIR="${DATA_DIR}"
    CONFIG_FILE="${DATA_DIR}/bootstrap.json"
    STATE_DIR="${DATA_DIR}/verloc-data"
    ANALYTICS_DB="${DATA_DIR}/verloc.db"
  fi
}

http_fetch() {
  url="$1"
  out="$2"
  if command -v curl >/dev/null 2>&1; then
    curl -fsSL "$url" -o "$out"
  elif command -v wget >/dev/null 2>&1; then
    wget -qO "$out" "$url"
  else
    die "neither curl nor wget is installed"
  fi
}

verify_sha256() {
  file="$1"
  expected="$2"
  if command -v sha256sum >/dev/null 2>&1; then
    actual="$(sha256sum "$file" | awk '{print $1}')"
  elif command -v shasum >/dev/null 2>&1; then
    actual="$(shasum -a 256 "$file" | awk '{print $1}')"
  else
    warn "sha256sum/shasum not found; skipping checksum verification"
    return 0
  fi
  [ "$actual" = "$expected" ] || die "checksum mismatch for $(basename "$file") (expected $expected, got $actual)"
}

check_go_version() {
  command -v go >/dev/null 2>&1 || return 1
  go_ver="$(go env GOVERSION 2>/dev/null || go version | awk '{print $3}')"
  major="$(printf '%s' "$go_ver" | sed -n 's/^go\([0-9][0-9]*\)\.\([0-9][0-9]*\).*/\1/p')"
  minor="$(printf '%s' "$go_ver" | sed -n 's/^go\([0-9][0-9]*\)\.\([0-9][0-9]*\).*/\2/p')"
  if [ -z "$major" ] || [ -z "$minor" ]; then
    return 1
  fi
  if [ "$major" -gt 1 ] || { [ "$major" -eq 1 ] && [ "$minor" -ge 25 ]; }; then
    return 0
  fi
  return 1
}

in_local_repo() {
  [ -f "./cmd/verloc/main.go" ] && [ -f "./go.mod" ] && grep -q '^module verloc$' ./go.mod 2>/dev/null
}

build_from_dir() {
  src_dir="$1"
  out_bin="$2"
  check_go_version || die "Go 1.25 or newer is required to build from source (https://go.dev/dl/)"

  if [ "$BUILD_DASH" -eq 1 ]; then
    need_cmd npm
    log "building embedded dashboard (npm ci && npm run build)"
    (cd "$src_dir/dashboard" && npm ci && npm run build)
  fi

  ver="source"
  commit="none"
  if [ -f "$src_dir/VERSION" ]; then
    base_ver="$(tr -d ' \t\r\n' < "$src_dir/VERSION")"
    [ -n "$base_ver" ] && ver="v${base_ver}-dev"
  fi
  if command -v git >/dev/null 2>&1 && [ -e "$src_dir/.git" ]; then
    if exact_tag="$(git -C "$src_dir" describe --tags --exact-match 2>/dev/null)"; then
      ver="$exact_tag"
    elif [ "$ver" = "source" ]; then
      ver="$(git -C "$src_dir" describe --tags --always --dirty 2>/dev/null || printf 'source')"
    fi
    commit="$(git -C "$src_dir" rev-parse --short HEAD 2>/dev/null || printf 'none')"
  elif [ "$VERSION" != "latest" ]; then
    ver="$VERSION"
  fi
  build_time="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  ldflags="-s -w -X main.version=${ver} -X main.commit=${commit} -X main.buildTime=${build_time}"

  log "building verloc binary (CGO_ENABLED=0, ${OS}/${ARCH})"
  (cd "$src_dir" && CGO_ENABLED=0 go build -trimpath -ldflags="$ldflags" -o "$out_bin" ./cmd/verloc)
}

try_download_release() {
  out_bin="$1"
  asset="verloc_${OS}_${ARCH}.tar.gz"
  if [ "$VERSION" = "latest" ]; then
    base_url="https://github.com/${REPO}/releases/latest/download"
  else
    base_url="https://github.com/${REPO}/releases/download/${VERSION}"
  fi

  archive="${TMP_DIR}/${asset}"
  checksums="${TMP_DIR}/checksums.txt"

  log "checking GitHub Releases (${REPO}, ${VERSION}) for ${asset}"
  if ! http_fetch "${base_url}/${asset}" "$archive" 2>/dev/null; then
    return 1
  fi
  if http_fetch "${base_url}/checksums.txt" "$checksums" 2>/dev/null; then
    expected="$(awk -v f="$asset" '$2 == f || $2 == "*" f {print $1; exit}' "$checksums")"
    if [ -n "$expected" ]; then
      verify_sha256 "$archive" "$expected"
      log "verified SHA-256 checksum"
    fi
  fi

  tar -xzf "$archive" -C "$TMP_DIR"
  if [ -f "${TMP_DIR}/verloc" ]; then
    mv "${TMP_DIR}/verloc" "$out_bin"
  else
    found="$(find "$TMP_DIR" -maxdepth 3 -type f -name verloc | head -n 1)"
    [ -n "$found" ] || die "release archive did not contain a verloc binary"
    mv "$found" "$out_bin"
  fi
  chmod 755 "$out_bin"
  return 0
}

obtain_binary() {
  out_bin="$1"
  if in_local_repo && [ "$VERSION" = "latest" ]; then
    log "detected local verloc checkout; building from current directory"
    build_from_dir "$(pwd)" "$out_bin"
    return 0
  fi

  if [ "$FROM_SOURCE" -eq 0 ]; then
    if try_download_release "$out_bin"; then
      return 0
    fi
    log "no prebuilt release binary found for ${OS}/${ARCH}; falling back to source build"
  fi

  if [ "$VERSION" = "latest" ]; then
    src_url="https://github.com/${REPO}/archive/refs/heads/${BRANCH}.tar.gz"
  else
    src_url="https://github.com/${REPO}/archive/refs/tags/${VERSION}.tar.gz"
  fi
  src_archive="${TMP_DIR}/source.tar.gz"
  log "downloading source archive from ${src_url}"
  http_fetch "$src_url" "$src_archive"
  mkdir -p "${TMP_DIR}/src"
  tar -xzf "$src_archive" -C "${TMP_DIR}/src"
  src_root="$(find "${TMP_DIR}/src" -mindepth 1 -maxdepth 1 -type d | head -n 1)"
  [ -n "$src_root" ] || die "failed to extract source archive"
  build_from_dir "$src_root" "$out_bin"
}

write_bootstrap_if_missing() {
  if [ -f "$CONFIG_FILE" ]; then
    log "preserving existing configuration at ${CONFIG_FILE}"
    return 0
  fi
  log "writing initial loopback configuration to ${CONFIG_FILE}"
  (
    umask 077
    cat > "$CONFIG_FILE" <<EOF
{
  "listen": "$(json_escape "$LISTEN")",
  "management_db": "$(json_escape "${STATE_DIR}/control.db")",
  "master_key_file": "$(json_escape "${STATE_DIR}/master.key")",
  "setup_token_file": "$(json_escape "${STATE_DIR}/setup.token")",
  "dashboard": {
    "db_path": "$(json_escape "$ANALYTICS_DB")"
  }
}
EOF
  )
}

health_url_for_listen() {
  addr="$1"
  case "$addr" in
    :*)
      printf 'http://127.0.0.1%s/healthz' "$addr"
      ;;
    0.0.0.0:*)
      printf 'http://127.0.0.1:%s/healthz' "${addr#0.0.0.0:}"
      ;;
    "[::]:"*)
      printf 'http://127.0.0.1:%s/healthz' "${addr#\[::\]:}"
      ;;
    *)
      printf 'http://%s/healthz' "$addr"
      ;;
  esac
}

dash_url_for_listen() {
  addr="$1"
  case "$addr" in
    :*)
      printf 'http://127.0.0.1%s/dash/' "$addr"
      ;;
    0.0.0.0:*)
      printf 'http://127.0.0.1:%s/dash/' "${addr#0.0.0.0:}"
      ;;
    "[::]:"*)
      printf 'http://127.0.0.1:%s/dash/' "${addr#\[::\]:}"
      ;;
    *)
      printf 'http://%s/dash/' "$addr"
      ;;
  esac
}

effective_listen() {
  if [ -f "$CONFIG_FILE" ]; then
    parsed="$(sed -n 's/.*"listen"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$CONFIG_FILE" | head -n 1)"
    if [ -n "$parsed" ]; then
      printf '%s' "$parsed"
      return 0
    fi
  fi
  printf '%s' "$LISTEN"
}

wait_for_health() {
  url="$(health_url_for_listen "$(effective_listen)")"
  i=0
  while [ "$i" -lt 25 ]; do
    if command -v curl >/dev/null 2>&1; then
      if curl -fsS "$url" >/dev/null 2>&1; then
        return 0
      fi
    elif command -v wget >/dev/null 2>&1; then
      if wget -qO- "$url" >/dev/null 2>&1; then
        return 0
      fi
    else
      return 0
    fi
    sleep 0.2 2>/dev/null || sleep 1
    i=$((i + 1))
  done
  return 1
}

install_system_service() {
  need_cmd systemctl
  if ! id -u verloc >/dev/null 2>&1; then
    log "creating system user 'verloc'"
    if command -v useradd >/dev/null 2>&1; then
      useradd --system --home-dir "$DATA_DIR" --shell /usr/sbin/nologin verloc
    elif command -v adduser >/dev/null 2>&1; then
      adduser --system --group --home "$DATA_DIR" --no-create-home verloc
    else
      die "neither useradd nor adduser found to create system user 'verloc'"
    fi
  fi

  install -d -m 700 -o verloc -g verloc "$DATA_DIR"
  install -d -m 750 -o root -g verloc "$CONFIG_DIR"
  write_bootstrap_if_missing
  chown root:verloc "$CONFIG_FILE"
  chmod 640 "$CONFIG_FILE"

  unit="/etc/systemd/system/verloc.service"
  log "installing systemd service at ${unit}"
  cat > "$unit" <<EOF
[Unit]
Description=verloc LLM gateway
Wants=network-online.target
After=network-online.target

[Service]
Type=simple
User=verloc
Group=verloc
WorkingDirectory=${DATA_DIR}
ExecStart=${BIN_DIR}/verloc -config ${CONFIG_FILE}
Restart=on-failure
RestartSec=3
TimeoutStopSec=40
UMask=0077
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=${DATA_DIR}

[Install]
WantedBy=multi-user.target
EOF
  systemctl daemon-reload
  systemctl enable --now verloc.service
  systemctl restart verloc.service
  if wait_for_health; then
    log "verloc.service is running and healthy"
  else
    warn "verloc.service started, but health check did not respond yet; inspect with: journalctl -u verloc -n 50"
  fi
}

install_user_service() {
  if [ "$OS" = "linux" ]; then
    need_cmd systemctl
    unit_dir="${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user"
    unit="${unit_dir}/verloc.service"
    mkdir -p "$unit_dir"
    log "installing user systemd service at ${unit}"
    cat > "$unit" <<EOF
[Unit]
Description=verloc LLM gateway
Wants=network-online.target
After=network-online.target

[Service]
Type=simple
WorkingDirectory=${DATA_DIR}
ExecStart=${BIN_DIR}/verloc -config ${CONFIG_FILE}
Restart=on-failure
RestartSec=3
TimeoutStopSec=40
UMask=0077
NoNewPrivileges=true

[Install]
WantedBy=default.target
EOF
    systemctl --user daemon-reload
    systemctl --user enable --now verloc.service
    systemctl --user restart verloc.service
    if wait_for_health; then
      log "verloc user service is running and healthy"
    else
      warn "service started, but health check did not respond yet; inspect with: journalctl --user -u verloc -n 50"
    fi
  elif [ "$OS" = "darwin" ]; then
    plist_dir="${HOME}/Library/LaunchAgents"
    plist="${plist_dir}/dev.verloc.gateway.plist"
    mkdir -p "$plist_dir"
    log "installing launchd agent at ${plist}"
    cat > "$plist" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>dev.verloc.gateway</string>
  <key>ProgramArguments</key>
  <array>
    <string>${BIN_DIR}/verloc</string>
    <string>-config</string>
    <string>${CONFIG_FILE}</string>
  </array>
  <key>WorkingDirectory</key>
  <string>${DATA_DIR}</string>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <dict>
    <key>SuccessfulExit</key>
    <false/>
  </dict>
  <key>StandardOutPath</key>
  <string>${DATA_DIR}/verloc.log</string>
  <key>StandardErrorPath</key>
  <string>${DATA_DIR}/verloc.log</string>
</dict>
</plist>
EOF
    uid="$(id -u)"
    launchctl bootout "gui/${uid}" "$plist" >/dev/null 2>&1 || true
    launchctl bootstrap "gui/${uid}" "$plist"
    if wait_for_health; then
      log "verloc launchd agent is running and healthy"
    else
      warn "agent loaded, but health check did not respond yet; inspect ${DATA_DIR}/verloc.log"
    fi
  fi
}

do_uninstall() {
  if [ "$SYSTEM_INSTALL" -eq 1 ]; then
    unit="/etc/systemd/system/verloc.service"
    if command -v systemctl >/dev/null 2>&1 && [ -f "$unit" ] && grep -Fq "${BIN_DIR}/verloc" "$unit" 2>/dev/null; then
      log "stopping and removing ${unit}"
      systemctl disable --now verloc.service >/dev/null 2>&1 || true
      rm -f "$unit"
      systemctl daemon-reload >/dev/null 2>&1 || true
    fi
  else
    if [ "$OS" = "linux" ] && command -v systemctl >/dev/null 2>&1; then
      unit="${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user/verloc.service"
      if [ -f "$unit" ] && grep -Fq "${BIN_DIR}/verloc" "$unit" 2>/dev/null; then
        log "stopping and removing ${unit}"
        systemctl --user disable --now verloc.service >/dev/null 2>&1 || true
        rm -f "$unit"
        systemctl --user daemon-reload >/dev/null 2>&1 || true
      fi
    elif [ "$OS" = "darwin" ]; then
      plist="${HOME}/Library/LaunchAgents/dev.verloc.gateway.plist"
      if [ -f "$plist" ] && grep -Fq "${BIN_DIR}/verloc" "$plist" 2>/dev/null; then
        log "stopping and removing ${plist}"
        launchctl bootout "gui/$(id -u)" "$plist" >/dev/null 2>&1 || true
        rm -f "$plist"
      fi
    fi
  fi

  if [ -f "${BIN_DIR}/verloc" ]; then
    log "removing ${BIN_DIR}/verloc"
    rm -f "${BIN_DIR}/verloc"
  fi

  if [ "$PURGE" -eq 1 ]; then
    log "purging data and configuration"
    rm -rf "$DATA_DIR"
    if [ "$SYSTEM_INSTALL" -eq 1 ] && [ -d "$CONFIG_DIR" ]; then
      rm -rf "$CONFIG_DIR"
    fi
  else
    log "preserved data in ${DATA_DIR} (pass --uninstall --purge to remove data as well)"
  fi
  log "uninstall complete"
}

main() {
  parse_args "$@"
  detect_platform
  validate_listen
  resolve_paths

  if [ "$UNINSTALL" -eq 1 ]; then
    do_uninstall
    exit 0
  fi

  TMP_DIR="$(mktemp -d "${TMPDIR:-/tmp}/verloc-install.XXXXXX")"
  staged_bin="${TMP_DIR}/verloc"
  obtain_binary "$staged_bin"

  mkdir -p "$BIN_DIR"
  log "installing binary to ${BIN_DIR}/verloc"
  install -m 755 "$staged_bin" "${BIN_DIR}/verloc.new"
  mv -f "${BIN_DIR}/verloc.new" "${BIN_DIR}/verloc"

  if [ "$SYSTEM_INSTALL" -eq 1 ]; then
    install_system_service
  else
    (
      umask 077
      mkdir -p "$DATA_DIR" "$STATE_DIR"
      chmod 700 "$DATA_DIR" "$STATE_DIR"
    )
    write_bootstrap_if_missing
    if [ "$INSTALL_SERVICE" -eq 1 ]; then
      install_user_service
    fi
  fi

  installed_ver="$("${BIN_DIR}/verloc" -version 2>/dev/null || printf 'verloc')"
  dash_url="$(dash_url_for_listen "$(effective_listen)")"
  token_path="${STATE_DIR}/setup.token"

  printf '\n'
  log "installed: ${installed_ver}"
  printf '  Binary:    %s/verloc\n' "$BIN_DIR"
  printf '  Config:    %s\n' "$CONFIG_FILE"
  printf '  Data dir:  %s\n' "$DATA_DIR"
  printf '  Dashboard: %s\n' "$dash_url"

  case ":${PATH}:" in
    *":${BIN_DIR}:"*) ;;
    *)
      printf '\n  Add verloc to your PATH:\n'
      printf '    export PATH="%s:$PATH"\n' "$BIN_DIR"
      ;;
  esac

  printf '\n'
  if [ "$INSTALL_SERVICE" -eq 1 ]; then
    printf 'Next steps:\n'
    printf '  1. Open %s\n' "$dash_url"
    if [ "$SYSTEM_INSTALL" -eq 1 ]; then
      printf '  2. Read the private first-run setup token:\n'
      printf '       sudo cat %s\n' "$token_path"
      printf '  3. Service logs: journalctl -u verloc -f\n'
    else
      printf '  2. Read the private first-run setup token:\n'
      printf '       cat "%s"\n' "$token_path"
      if [ "$OS" = "linux" ]; then
        printf '  3. Service logs: journalctl --user -u verloc -f\n'
      else
        printf '  3. Service logs: tail -f "%s/verloc.log"\n' "$DATA_DIR"
      fi
    fi
  elif [ "$START_NOW" -eq 1 ]; then
    printf 'Starting verloc (open %s and read "%s" on first run)...\n\n' "$dash_url" "$token_path"
    exec "${BIN_DIR}/verloc" -config "$CONFIG_FILE"
  else
    printf 'Next steps:\n'
    printf '  1. Start verloc:\n'
    printf '       "%s/verloc" -config "%s"\n' "$BIN_DIR" "$CONFIG_FILE"
    printf '  2. Open %s\n' "$dash_url"
    printf '  3. On first run, read the private setup token:\n'
    printf '       cat "%s"\n' "$token_path"
    printf '\nTip: re-run with --service to install and start verloc as a background service.\n'
  fi
}

main "$@"
