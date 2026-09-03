#!/bin/bash
set -e

LABEL="com.user.claude-lens"
PLIST_PATH="$HOME/Library/LaunchAgents/${LABEL}.plist"
CONFIG_DIR="$HOME/Library/Application Support/claude-lens"
ENV_FILE="${CONFIG_DIR}/claude-lens.env"
DOWNLOAD_URL="https://github.com/lfsc09/claude-lens/releases/latest/download/claude-lens-darwin-amd64"
CHECKSUM_URL="${DOWNLOAD_URL}.sha256"

SCRIPT_NAME="install_macos.sh"
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
  if command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$1" | awk '{print $1}'
  else
    sha256sum "$1" | awk '{print $1}'
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
Usage: install_macos.sh [options]

Safe to re-run at any time to update the binary and/or reconfigure the
service. Any option you omit keeps whatever was set on a previous run
(or the default shown, on a first install).

  --proxy-base-url URL      Upstream API URL (default: https://api.anthropic.com)
  --proxy-auth-token TOKEN  Authorization value forwarded upstream
  --proxy-custom-header "H: v"  Extra header forwarded upstream (repeatable)
  --proxy-addr PORT         Proxy listen port (default: 7801)
  --admin-addr PORT         Admin listen port (default: 7802)
  --install-dir PATH        Base directory for the binary (REQUIRED)
  --data-dir PATH           SQLite database directory (default: {--install-dir}/data)
  --log-dir PATH            Log directory (default: {--install-dir}/logs)
  --as-service              Configure and start claude-lens as a launchd agent
                             (default: off - just downloads/updates the binary)
  -h, --help                Show this help
USAGE
}

xml_escape() {
  local s="$1"
  s="${s//&/&amp;}"
  s="${s//</&lt;}"
  s="${s//>/&gt;}"
  printf '%s' "$s"
}

check_install_dir_writable() {
  local dir="$INSTALL_DIR"
  while [ ! -d "$dir" ]; do
    dir="$(dirname "$dir")"
  done
  if [ ! -w "$dir" ]; then
    log error "'${INSTALL_DIR}' is not writable by $(whoami) (nearest existing directory '${dir}' is not writable)."
    log error "This installer does not use sudo. Fix the permission once, then re-run it:"
    log error "  sudo mkdir -p '${INSTALL_DIR}'"
    log error "  sudo chown \"\$(whoami)\" '${INSTALL_DIR}'"
    exit 1
  fi
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
# entries), then unescaped back to a real newline below before going into
# the plist, since XML <string> content accepts a literal newline directly.
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
# --install-dir must be set here or on a previous run (carried forward via the env file).
if [ -z "$CLENS_INSTALL_DIR" ]; then
  log error "--install-dir is required (no first-run default on macOS)."
  usage
  exit 1
fi
INSTALL_DIR="$CLENS_INSTALL_DIR"
CLENS_DATA_DIR="${CLENS_DATA_DIR:-${INSTALL_DIR}/data}"
CLENS_LOG_DIR="${CLENS_LOG_DIR:-${INSTALL_DIR}/logs}"

# ── Verify ANTHROPIC_BASE_URL, the var Claude Code itself reads ────────
# claude-lens never sees this variable - Claude Code does, directly from
# the OS environment - so it has to be set independently of everything
# below. We only check it points at this install's proxy port.
proxy_port="${CLENS_PROXY_ADDR##*:}"
expected_anthropic_url="http://localhost:${proxy_port}"

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

check_install_dir_writable
mkdir -p "$INSTALL_DIR"
mkdir -p "$CLENS_DATA_DIR" "$CLENS_LOG_DIR" "$CONFIG_DIR"

if [ "$CLENS_AS_SERVICE" = "true" ]; then
  mkdir -p "$HOME/Library/LaunchAgents"

  # ── Unload and remove existing service if present ──────────────────────
  if launchctl list | grep -q "$LABEL" 2>/dev/null; then
    log info "Existing ${LABEL} service detected. Stopping service..."
    launchctl bootout "gui/$(id -u)" "$PLIST_PATH" 2>/dev/null || launchctl unload "$PLIST_PATH" 2>/dev/null || true
  fi

  if [ -f "$PLIST_PATH" ]; then
    log info "Removing old plist configuration..."
    rm -f "$PLIST_PATH"
  fi
fi

log info "Downloading latest claude-lens binary and checksum..."
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

mv "$tmp_bin" "${INSTALL_DIR}/claude-lens"
chmod +x "${INSTALL_DIR}/claude-lens"

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

if [ "$CLENS_AS_SERVICE" = "true" ]; then
  real_headers="${_CLENS_CUSTOM_HEADERS_ESCAPED//\\n/$'\n'}"

  log info "Creating launchd property list..."
  cat <<EOF > "$PLIST_PATH"
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>${LABEL}</string>
    <key>ProgramArguments</key>
    <array>
        <string>${INSTALL_DIR}/claude-lens</string>
    </array>
    <key>WorkingDirectory</key>
    <string>$(xml_escape "$CLENS_DATA_DIR")</string>
    <key>EnvironmentVariables</key>
    <dict>
        <key>CLENS_PROXY_BASE_URL</key>
        <string>$(xml_escape "$CLENS_PROXY_BASE_URL")</string>
        <key>CLENS_PROXY_AUTH_TOKEN</key>
        <string>$(xml_escape "$CLENS_PROXY_AUTH_TOKEN")</string>
        <key>CLENS_PROXY_CUSTOM_HEADERS</key>
        <string>$(xml_escape "$real_headers")</string>
        <key>CLENS_PROXY_ADDR</key>
        <string>$(xml_escape "$CLENS_PROXY_ADDR")</string>
        <key>CLENS_ADMIN_ADDR</key>
        <string>$(xml_escape "$CLENS_ADMIN_ADDR")</string>
        <key>CLENS_DATA_DIR</key>
        <string>$(xml_escape "$CLENS_DATA_DIR")</string>
        <key>CLENS_LOG_DIR</key>
        <string>$(xml_escape "$CLENS_LOG_DIR")</string>
    </dict>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>StandardOutPath</key>
    <string>$(xml_escape "$CLENS_LOG_DIR")/launchd-stdout.log</string>
    <key>StandardErrorPath</key>
    <string>$(xml_escape "$CLENS_LOG_DIR")/launchd-stderr.log</string>
</dict>
</plist>
EOF
  chmod 600 "$PLIST_PATH"

  log info "Loading launchd service..."
  launchctl bootstrap "gui/$(id -u)" "$PLIST_PATH" 2>/dev/null || launchctl load -w "$PLIST_PATH"

  log info "${LABEL} installed/updated and started as a launchd agent!"
  log info "Proxy listening on ${CLENS_PROXY_ADDR}, admin UI on ${CLENS_ADMIN_ADDR}."
  log info "Config saved to ${ENV_FILE} - it's re-read on every install, but changing it by hand has no effect until you re-run this installer (launchd only reads env vars from the plist at load time)."
else
  log info "claude-lens binary installed/updated at ${INSTALL_DIR}/claude-lens."
  log info "Config saved to ${ENV_FILE}."
  log info "Re-run this installer with --as-service to configure and start it as a launchd agent."
fi
