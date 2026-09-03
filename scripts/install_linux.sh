#!/bin/bash
set -e

SERVICE_NAME="claude-lens"
DEFAULT_INSTALL_DIR="/usr/local/bin"
SERVICE_FILE="/etc/systemd/system/${SERVICE_NAME}.service"
CONFIG_DIR="/etc/claude-lens"
ENV_FILE="${CONFIG_DIR}/claude-lens.env"
DOWNLOAD_URL="https://github.com/lfsc09/claude-lens/releases/latest/download/claude-lens-linux-amd64"
CHECKSUM_URL="${DOWNLOAD_URL}.sha256"
SERVICE_USER="claude-lens"
SERVICE_GROUP="claude-lens"

SCRIPT_NAME="install_linux.sh"
_COLOR_YELLOW=$'\033[33m'
_COLOR_RED=$'\033[31m'
_COLOR_RESET=$'\033[0m'

# log prints a message to the terminal, prefixed with the script name.
# level selects the channel and coloring: info (stdout, plain),
# warn (stdout, yellow) or error (stderr, red).
log() {
  local level="$1"
  shift
  local message="$*"
  case "$level" in
    info) printf '%s: %s\n' "$SCRIPT_NAME" "$message" ;;
    warn) printf '%s: %s%s%s\n' "$SCRIPT_NAME" "$_COLOR_YELLOW" "$message" "$_COLOR_RESET" ;;
    error) printf '%s: %s%s%s\n' "$SCRIPT_NAME" "$_COLOR_RED" "$message" "$_COLOR_RESET" >&2 ;;
  esac
}

sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  else
    shasum -a 256 "$1" | awk '{print $1}'
  fi
}

# normalize_port strips an optional leading ":" from value and prints the
# remaining port number, or exits with an error if it isn't one.
normalize_port() {
  local flag="$1" port="${2#:}"
  case "$port" in
    ''|*[!0-9]*)
      log error "${flag} must be a plain port number (e.g. 7801), got '${2}'."
      exit 1
      ;;
  esac
  printf '%s' "$port"
}

usage() {
  cat <<'USAGE'
Usage: install_linux.sh [options]

Safe to re-run at any time to update the binary and/or reconfigure the
service. Any option you omit keeps whatever was set on a previous run
(or the default shown, on a first install).

  --proxy-base-url URL      Upstream API URL (default: https://api.anthropic.com)
  --proxy-auth-token TOKEN  Authorization value forwarded upstream
  --proxy-custom-header "H: v"  Extra header forwarded upstream (repeatable)
  --proxy-addr PORT         Proxy listen port (default: 7801)
  --admin-addr PORT         Admin listen port (default: 7802)
  --install-dir PATH        Base directory for the binary (default: /usr/local/bin)
  --data-dir PATH           SQLite database directory (default: /var/lib/claude-lens,
                             or {--install-dir}/data if --install-dir is set)
  --log-dir PATH            Log directory (default: /var/log/claude-lens,
                             or {--install-dir}/logs if --install-dir is set)
  --as-service              Configure and start claude-lens as a systemd service
                             (default: off - just downloads/updates the binary)
  -h, --help                Show this help
USAGE
}

# ── Defaults ──────────────────────────────────────────────────────────
CLENS_PROXY_BASE_URL="https://api.anthropic.com"
CLENS_PROXY_AUTH_TOKEN=""
CLENS_PROXY_ADDR="7801"
CLENS_ADMIN_ADDR="7802"
CLENS_INSTALL_DIR=""
CLENS_DATA_DIR=""
CLENS_LOG_DIR=""
CLENS_AS_SERVICE="false"
_CLENS_CUSTOM_HEADERS_ESCAPED=""

# ── Carry forward settings from a previous install, if any ─────────────
if [ -f "$ENV_FILE" ]; then
  # shellcheck disable=SC1090
  source "$ENV_FILE"
fi

# ── Parse flags (override persisted values/defaults) ────────────────────
PROXY_CUSTOM_HEADERS_ARR=()
while [ $# -gt 0 ]; do
  case "$1" in
    --proxy-base-url) CLENS_PROXY_BASE_URL="$2"; shift 2 ;;
    --proxy-base-url=*) CLENS_PROXY_BASE_URL="${1#*=}"; shift ;;
    --proxy-auth-token) CLENS_PROXY_AUTH_TOKEN="$2"; shift 2 ;;
    --proxy-auth-token=*) CLENS_PROXY_AUTH_TOKEN="${1#*=}"; shift ;;
    --proxy-custom-header) PROXY_CUSTOM_HEADERS_ARR+=("$2"); shift 2 ;;
    --proxy-custom-header=*) PROXY_CUSTOM_HEADERS_ARR+=("${1#*=}"); shift ;;
    --proxy-addr) CLENS_PROXY_ADDR="$2"; shift 2 ;;
    --proxy-addr=*) CLENS_PROXY_ADDR="${1#*=}"; shift ;;
    --admin-addr) CLENS_ADMIN_ADDR="$2"; shift 2 ;;
    --admin-addr=*) CLENS_ADMIN_ADDR="${1#*=}"; shift ;;
    --install-dir) CLENS_INSTALL_DIR="$2"; shift 2 ;;
    --install-dir=*) CLENS_INSTALL_DIR="${1#*=}"; shift ;;
    --data-dir) CLENS_DATA_DIR="$2"; shift 2 ;;
    --data-dir=*) CLENS_DATA_DIR="${1#*=}"; shift ;;
    --log-dir) CLENS_LOG_DIR="$2"; shift 2 ;;
    --log-dir=*) CLENS_LOG_DIR="${1#*=}"; shift ;;
    --as-service) CLENS_AS_SERVICE="true"; shift ;;
    -h|--help) usage; exit 0 ;;
    *) log error "Unknown option: $1"; usage; exit 1 ;;
  esac
done

# --proxy-addr/--admin-addr accept a bare port number; normalized here to
# the ":port" form CLENS_PROXY_ADDR/CLENS_ADMIN_ADDR carry at runtime.
CLENS_PROXY_ADDR=":$(normalize_port --proxy-addr "$CLENS_PROXY_ADDR")"
CLENS_ADMIN_ADDR=":$(normalize_port --admin-addr "$CLENS_ADMIN_ADDR")"

# Only replace the persisted custom headers if --proxy-custom-header was passed
# at least once this run. Stored pre-escaped (literal "\n" between
# entries) so it round-trips through `source` unambiguously and can be
# dropped straight into the unit file's Environment= line, whose own
# quoting rules (unlike EnvironmentFile's) reliably turn \n into a real
# newline regardless of systemd version.
if [ ${#PROXY_CUSTOM_HEADERS_ARR[@]} -gt 0 ]; then
  headers_escaped=""
  for h in "${PROXY_CUSTOM_HEADERS_ARR[@]}"; do
    if [ -z "$headers_escaped" ]; then
      headers_escaped="$h"
    else
      headers_escaped="${headers_escaped}\\n${h}"
    fi
  done
  _CLENS_CUSTOM_HEADERS_ESCAPED="$headers_escaped"
fi

# ── Resolve install/data/log directories ─────────────────────────────────
# --install-dir only changes the fallback for --data-dir/--log-dir when
# those are left unset; an explicit --data-dir/--log-dir always wins.
INSTALL_DIR="${CLENS_INSTALL_DIR:-$DEFAULT_INSTALL_DIR}"
if [ -z "$CLENS_DATA_DIR" ]; then
  if [ -n "$CLENS_INSTALL_DIR" ]; then
    CLENS_DATA_DIR="${INSTALL_DIR}/data"
  else
    CLENS_DATA_DIR="/var/lib/claude-lens"
  fi
fi
if [ -z "$CLENS_LOG_DIR" ]; then
  if [ -n "$CLENS_INSTALL_DIR" ]; then
    CLENS_LOG_DIR="${INSTALL_DIR}/logs"
  else
    CLENS_LOG_DIR="/var/log/claude-lens"
  fi
fi

if [ "$EUID" -ne 0 ]; then
  log error "Please run as root (e.g., sudo ./install_linux.sh)"
  exit 1
fi

# ── Verify ANTHROPIC_BASE_URL, the var Claude Code itself reads ────────
# claude-lens never sees this variable - Claude Code does, directly from
# the OS environment - so it has to be set independently of everything
# below. We only check it points at this install's proxy port.
proxy_port="${CLENS_PROXY_ADDR##*:}"
expected_anthropic_url="http://localhost:${proxy_port}"

# sudo resets the environment by default, so this script never inherits
# ANTHROPIC_BASE_URL from the invoking user's shell (e.g. via
# `curl ... | sudo bash`). Fall back to reading it straight from that
# user's own login shell, which is unaffected by anything sudo stripped.
if [ -z "${ANTHROPIC_BASE_URL:-}" ] && [ -n "${SUDO_USER:-}" ]; then
  invoking_shell="$(getent passwd "$SUDO_USER" | cut -d: -f7)"
  ANTHROPIC_BASE_URL="$(sudo -u "$SUDO_USER" -H "${invoking_shell:-/bin/bash}" -lc 'printf %s "$ANTHROPIC_BASE_URL"' 2>/dev/null || true)"
fi

if [ -n "${ANTHROPIC_BASE_URL:-}" ]; then
  if [ "$ANTHROPIC_BASE_URL" != "$expected_anthropic_url" ]; then
    log error "ANTHROPIC_BASE_URL is set to '${ANTHROPIC_BASE_URL}', but this install listens at '${expected_anthropic_url}' (from --proxy-addr=${proxy_port})."
    log error "Fix this manually before continuing - either:"
    log error "  export ANTHROPIC_BASE_URL=${expected_anthropic_url}"
    log error "or re-run this installer with --proxy-addr matching your existing ANTHROPIC_BASE_URL port."
    exit 1
  fi
  log info "ANTHROPIC_BASE_URL already points at ${expected_anthropic_url} - good."
else
  log warn "ANTHROPIC_BASE_URL is not set. Claude Code will not route through claude-lens until you set it and persist it in your shell profile:"
  log warn "  export ANTHROPIC_BASE_URL=${expected_anthropic_url}"
fi

# ── Stop and disable if already running/installed ───────────────────────
if [ "$CLENS_AS_SERVICE" = "true" ]; then
  if systemctl is-active --quiet "$SERVICE_NAME" || systemctl is-enabled --quiet "$SERVICE_NAME" 2>/dev/null; then
    log info "Existing ${SERVICE_NAME} service detected. Stopping service..."
    systemctl stop "$SERVICE_NAME" || true
    systemctl disable "$SERVICE_NAME" || true
  fi

  if [ -f "$SERVICE_FILE" ]; then
    log info "Removing existing service file..."
    rm -f "$SERVICE_FILE"
    systemctl daemon-reload
  fi
fi

log info "Downloading latest ${SERVICE_NAME} binary and checksum..."
tmp_bin="$(mktemp)"
tmp_sha="$(mktemp)"
trap 'rm -f "$tmp_bin" "$tmp_sha"' EXIT
curl -fsSL "$DOWNLOAD_URL" -o "$tmp_bin"
curl -fsSL "$CHECKSUM_URL" -o "$tmp_sha"

expected_sha="$(awk '{print $1}' "$tmp_sha")"
actual_sha="$(sha256_of "$tmp_bin")"
if [ "$expected_sha" != "$actual_sha" ]; then
  log error "checksum mismatch for downloaded binary (expected ${expected_sha}, got ${actual_sha})."
  log error "Aborting - the existing installation, if any, was left untouched."
  exit 1
fi

mkdir -p "$INSTALL_DIR"
mv "$tmp_bin" "${INSTALL_DIR}/${SERVICE_NAME}"
chmod +x "${INSTALL_DIR}/${SERVICE_NAME}"

if [ "$CLENS_AS_SERVICE" = "true" ]; then
  nologin_shell="$(command -v nologin || echo /usr/sbin/nologin)"
  if ! getent group "$SERVICE_GROUP" >/dev/null 2>&1; then
    log info "Creating system group ${SERVICE_GROUP}..."
    groupadd --system "$SERVICE_GROUP"
  fi
  if ! getent passwd "$SERVICE_USER" >/dev/null 2>&1; then
    log info "Creating system user ${SERVICE_USER}..."
    useradd --system --no-create-home --shell "$nologin_shell" --gid "$SERVICE_GROUP" "$SERVICE_USER"
  fi
fi

log info "Preparing data/log directories..."
mkdir -p "$CLENS_DATA_DIR" "$CLENS_LOG_DIR" "$CONFIG_DIR"
if [ "$CLENS_AS_SERVICE" = "true" ]; then
  chown -R "${SERVICE_USER}:${SERVICE_GROUP}" "$CLENS_DATA_DIR" "$CLENS_LOG_DIR"
else
  chown -R "${SUDO_USER:-root}" "$CLENS_DATA_DIR" "$CLENS_LOG_DIR"
fi

log info "Writing ${ENV_FILE}..."
cat <<EOF > "$ENV_FILE"
CLENS_PROXY_BASE_URL="${CLENS_PROXY_BASE_URL}"
CLENS_PROXY_AUTH_TOKEN="${CLENS_PROXY_AUTH_TOKEN}"
CLENS_PROXY_ADDR="${CLENS_PROXY_ADDR}"
CLENS_ADMIN_ADDR="${CLENS_ADMIN_ADDR}"
CLENS_INSTALL_DIR="${CLENS_INSTALL_DIR}"
CLENS_DATA_DIR="${CLENS_DATA_DIR}"
CLENS_LOG_DIR="${CLENS_LOG_DIR}"
CLENS_AS_SERVICE="${CLENS_AS_SERVICE}"
_CLENS_CUSTOM_HEADERS_ESCAPED="${_CLENS_CUSTOM_HEADERS_ESCAPED}"
EOF
chmod 600 "$ENV_FILE"
chown root:root "$ENV_FILE"

if [ "$CLENS_AS_SERVICE" = "true" ]; then
  headers_env_line=""
  if [ -n "$_CLENS_CUSTOM_HEADERS_ESCAPED" ]; then
    headers_env_line="Environment=\"CLENS_PROXY_CUSTOM_HEADERS=${_CLENS_CUSTOM_HEADERS_ESCAPED}\""
  fi

  log info "Creating systemd service unit..."
  cat <<EOF > "$SERVICE_FILE"
[Unit]
Description=Claude Lens Background Service
After=network.target

[Service]
Type=simple
ExecStart=${INSTALL_DIR}/${SERVICE_NAME}
EnvironmentFile=-${ENV_FILE}
${headers_env_line}
WorkingDirectory=${CLENS_DATA_DIR}
Restart=always
RestartSec=5
User=${SERVICE_USER}
Group=${SERVICE_GROUP}

[Install]
WantedBy=multi-user.target
EOF

  log info "Reloading systemd, enabling, and starting service..."
  systemctl daemon-reload
  systemctl enable "$SERVICE_NAME"
  systemctl start "$SERVICE_NAME"

  log info "${SERVICE_NAME} installed/updated and started as a systemd service!"
  log info "Proxy listening on ${CLENS_PROXY_ADDR}, admin UI on ${CLENS_ADMIN_ADDR}."
  log info "Config saved to ${ENV_FILE} - editing the simple values there and running 'systemctl restart ${SERVICE_NAME}' applies them (custom headers need a re-run of this installer)."
else
  log info "${SERVICE_NAME} binary installed/updated at ${INSTALL_DIR}/${SERVICE_NAME}."
  log info "Config saved to ${ENV_FILE}."
  log info "Re-run this installer with --as-service to configure and start it as a systemd service."
fi
