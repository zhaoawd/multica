#!/usr/bin/env bash
# Multica installer — installs the CLI and optionally provisions a self-host server.
#
# Install / upgrade CLI only:
#   curl -fsSL https://raw.githubusercontent.com/multica-ai/multica/main/scripts/install.sh | bash
#
# Install CLI + register this machine as a Computer in one shot (RFC v6.1 §6.4):
#   curl -fsSL https://raw.githubusercontent.com/multica-ai/multica/main/scripts/install.sh \
#       | bash -s -- --workspace <slug> --token <mit_…>
#   ( --server-url is optional; defaults to Multica Cloud. )
#
# Install CLI + provision self-host server:
#   curl -fsSL https://raw.githubusercontent.com/multica-ai/multica/main/scripts/install.sh | bash -s -- --with-server
#
# Diagnostics:
#   --diagnose       Run the one-liner install and write a redacted log bundle
#                    to $HOME/Library/Logs/Multica (macOS) or $HOME/.cache/Multica/logs
#                    (Linux). Token values are NEVER written to disk or stdout/stderr.
#
# Without --workspace/--token, this script falls back to the legacy
# install-only path. After installation, run `multica setup` to configure your
# environment manually.
#
set -euo pipefail

# ---------------------------------------------------------------------------
# Configuration
# ---------------------------------------------------------------------------
REPO_URL="https://github.com/multica-ai/multica.git"
REPO_WEB_URL="https://github.com/multica-ai/multica"  # without .git, for GitHub web APIs
INSTALL_DIR="${MULTICA_INSTALL_DIR:-$HOME/.multica/server}"
BREW_PACKAGE="multica-ai/tap/multica"

# Colors (disabled when not a terminal)
if [ -t 1 ] || [ -t 2 ]; then
  BOLD='\033[1m'
  GREEN='\033[0;32m'
  YELLOW='\033[0;33m'
  RED='\033[0;31m'
  CYAN='\033[0;36m'
  RESET='\033[0m'
else
  BOLD='' GREEN='' YELLOW='' RED='' CYAN='' RESET=''
fi

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------
info()  { printf "${BOLD}${CYAN}==> %s${RESET}\n" "$*"; }
ok()    { printf "${BOLD}${GREEN}✓ %s${RESET}\n" "$*"; }
warn()  { printf "${BOLD}${YELLOW}⚠ %s${RESET}\n" "$*" >&2; }
fail()  { printf "${BOLD}${RED}✗ %s${RESET}\n" "$*" >&2; exit 1; }

command_exists() { command -v "$1" >/dev/null 2>&1; }

detect_os() {
  case "$(uname -s)" in
    Darwin) OS="darwin" ;;
    Linux)  OS="linux" ;;
    MINGW*|MSYS*|CYGWIN*)
            fail "This script does not support Windows. Use the PowerShell installer instead:
  irm https://raw.githubusercontent.com/multica-ai/multica/main/scripts/install.ps1 | iex" ;;
    *)      fail "Unsupported operating system: $(uname -s). Multica supports macOS, Linux, and Windows." ;;
  esac

  ARCH="$(uname -m)"
  case "$ARCH" in
    x86_64)  ARCH="amd64" ;;
    aarch64) ARCH="arm64" ;;
    arm64)   ARCH="arm64" ;;
    *)       fail "Unsupported architecture: $ARCH" ;;
  esac
}

# ---------------------------------------------------------------------------
# CLI Installation
# ---------------------------------------------------------------------------
install_cli_brew() {
  info "Installing Multica CLI via Homebrew..."
  if ! brew tap multica-ai/tap 2>/dev/null; then
    fail "Failed to add Homebrew tap. Check your network connection."
  fi
  # brew install exits non-zero if already installed on older Homebrew versions
  if ! brew install "$BREW_PACKAGE" 2>/dev/null; then
    if brew list "$BREW_PACKAGE" >/dev/null 2>&1; then
      ok "Multica CLI already installed via Homebrew"
    else
      fail "Failed to install multica via Homebrew."
    fi
  else
    ok "Multica CLI installed via Homebrew"
  fi
}

install_cli_binary() {
  info "Installing Multica CLI from GitHub Releases..."

  # Get latest release tag
  local latest
  latest=$(curl -sI "$REPO_WEB_URL/releases/latest" 2>/dev/null | grep -i '^location:' | sed 's/.*tag\///' | tr -d '\r\n' || true)
  if [ -z "$latest" ]; then
    fail "Could not determine latest release. Check your network connection."
  fi

  local version="${latest#v}"
  local url="https://github.com/multica-ai/multica/releases/download/${latest}/multica-cli-${version}-${OS}-${ARCH}.tar.gz"
  local tmp_dir
  tmp_dir=$(mktemp -d)

  info "Downloading $url ..."
  if ! curl -fsSL "$url" -o "$tmp_dir/multica.tar.gz"; then
    rm -rf "$tmp_dir"
    fail "Failed to download CLI binary."
  fi

  tar -xzf "$tmp_dir/multica.tar.gz" -C "$tmp_dir" multica

  # Try /usr/local/bin first, fall back to ~/.local/bin
  local bin_dir="/usr/local/bin"
  if [ -w "$bin_dir" ]; then
    mv "$tmp_dir/multica" "$bin_dir/multica"
  elif command_exists sudo; then
    sudo mv "$tmp_dir/multica" "$bin_dir/multica"
  else
    bin_dir="$HOME/.local/bin"
    mkdir -p "$bin_dir"
    mv "$tmp_dir/multica" "$bin_dir/multica"
    chmod +x "$bin_dir/multica"
    # Add to PATH if not already there
    if ! echo "$PATH" | tr ':' '\n' | grep -q "^$bin_dir$"; then
      export PATH="$bin_dir:$PATH"
      add_to_path "$bin_dir"
    fi
  fi

  rm -rf "$tmp_dir"
  ok "Multica CLI installed to $bin_dir/multica"
}

add_to_path() {
  local dir="$1"
  local line="export PATH=\"$dir:\$PATH\""
  for rc in "$HOME/.bashrc" "$HOME/.zshrc"; do
    if [ -f "$rc" ] && ! grep -qF "$dir" "$rc"; then
      printf '\n# Added by Multica installer\n%s\n' "$line" >> "$rc"
    fi
  done
}

get_latest_version() {
  # grep exits 1 when no match; use `|| true` to avoid triggering pipefail
  curl -sI "$REPO_WEB_URL/releases/latest" 2>/dev/null | grep -i '^location:' | sed 's/.*tag\///' | tr -d '\r\n' || true
}

get_selfhost_ref() {
  if [ -n "${MULTICA_SELFHOST_REF:-}" ]; then
    printf '%s' "$MULTICA_SELFHOST_REF"
    return
  fi

  local latest
  latest=$(get_latest_version)
  if [ -n "$latest" ]; then
    printf '%s' "$latest"
    return
  fi

  printf '%s' "main"
}

checkout_server_ref() {
  local ref="$1"

  if [ "$ref" = "main" ]; then
    git fetch origin main --depth 1 2>/dev/null || true
    git checkout --force main 2>/dev/null || true
    git reset --hard origin/main 2>/dev/null || true
    return
  fi

  git fetch origin --tags --force 2>/dev/null || true
  if git rev-parse --verify --quiet "refs/tags/$ref" >/dev/null; then
    git checkout --force "$ref" 2>/dev/null || git checkout --force "tags/$ref" 2>/dev/null || true
    return
  fi

  git fetch origin "$ref" --depth 1 2>/dev/null || true
  git checkout --force "$ref" 2>/dev/null || true
}

pull_official_selfhost_images() {
  if docker compose -f docker-compose.selfhost.yml pull; then
    return
  fi

  echo ""
  warn "Official images for the selected self-host channel are not published yet."
  echo "This can happen before the first GHCR release is available."
  echo "From $INSTALL_DIR, build from source instead:"
  echo "  docker compose -f docker-compose.selfhost.yml -f docker-compose.selfhost.build.yml up -d --build"
  exit 1
}

upgrade_cli_brew() {
  info "Upgrading Multica CLI via Homebrew..."
  brew update 2>/dev/null || true
  if brew upgrade "$BREW_PACKAGE" 2>/dev/null; then
    ok "Multica CLI upgraded via Homebrew"
  else
    # brew upgrade exits non-zero if already up to date
    ok "Multica CLI is already the latest version"
  fi
}

install_cli() {
  if command_exists multica; then
    local current_ver
    # `multica version` outputs "multica v0.1.13 (commit: abc1234)" — extract just the version
    current_ver=$(multica version 2>/dev/null | awk '{print $2}' || echo "unknown")

    local latest_ver
    latest_ver=$(get_latest_version)

    # Normalize: strip leading 'v' for comparison
    local current_cmp="${current_ver#v}"
    local latest_cmp="${latest_ver#v}"

    if [ -z "$latest_ver" ] || [ "$current_cmp" = "$latest_cmp" ]; then
      ok "Multica CLI is up to date ($current_ver)"
      return 0
    fi

    info "Multica CLI $current_ver installed, latest is $latest_ver — upgrading..."
    if command_exists brew && brew list "$BREW_PACKAGE" >/dev/null 2>&1; then
      upgrade_cli_brew
    else
      install_cli_binary
    fi

    local new_ver
    new_ver=$(multica version 2>/dev/null | awk '{print $2}' || echo "unknown")
    ok "Multica CLI upgraded ($current_ver → $new_ver)"
    return 0
  fi

  if command_exists brew; then
    install_cli_brew
  else
    install_cli_binary
  fi

  # Verify
  if ! command_exists multica; then
    fail "CLI installed but 'multica' not found on PATH. You may need to restart your shell."
  fi
}

# ---------------------------------------------------------------------------
# Docker check
# ---------------------------------------------------------------------------
check_docker() {
  if ! command_exists docker; then
    printf "\n"
    fail "Docker is not installed. Multica self-hosting requires Docker and Docker Compose.

Install Docker:
  macOS:  https://docs.docker.com/desktop/install/mac-install/
  Linux:  https://docs.docker.com/engine/install/

After installing Docker, re-run this script with --with-server."
  fi

  if ! docker info >/dev/null 2>&1; then
    fail "Docker is installed but not running. Please start Docker and re-run this script."
  fi

  ok "Docker is available"
}

# ---------------------------------------------------------------------------
# Server setup (self-host / --with-server)
# ---------------------------------------------------------------------------
setup_server() {
  info "Setting up Multica server..."
  local server_ref
  server_ref=$(get_selfhost_ref)
  info "Using self-host assets from ${server_ref}..."

  if [ -d "$INSTALL_DIR/.git" ]; then
    info "Updating existing installation at $INSTALL_DIR..."
    cd "$INSTALL_DIR"
  else
    info "Cloning Multica repository..."
    if ! command_exists git; then
      fail "Git is not installed. Please install git and re-run."
    fi
    # Remove leftover directory from a previously interrupted clone
    if [ -d "$INSTALL_DIR" ]; then
      warn "Removing incomplete installation at $INSTALL_DIR..."
      rm -rf "$INSTALL_DIR"
    fi
    mkdir -p "$(dirname "$INSTALL_DIR")"
    git clone --depth 1 "$REPO_URL" "$INSTALL_DIR"
    cd "$INSTALL_DIR"
  fi

  checkout_server_ref "$server_ref"

  ok "Repository ready at $INSTALL_DIR ($server_ref)"

  # Generate .env if needed
  if [ ! -f .env ]; then
    info "Creating .env with random JWT_SECRET..."
    cp .env.example .env
    local jwt
    jwt=$(openssl rand -hex 32)
    if [ "$(uname -s)" = "Darwin" ]; then
      sed -i '' "s/^JWT_SECRET=.*/JWT_SECRET=$jwt/" .env
    else
      sed -i "s/^JWT_SECRET=.*/JWT_SECRET=$jwt/" .env
    fi
    ok "Generated .env with random JWT_SECRET"
  else
    ok "Using existing .env"
  fi

  # Start Docker Compose
  info "Pulling official Multica images..."
  pull_official_selfhost_images
  info "Starting Multica services (this may take a few minutes on first run)..."
  docker compose -f docker-compose.selfhost.yml up -d

  # Wait for health check
  info "Waiting for backend to be ready..."
  local ready=false
  for i in $(seq 1 45); do
    if curl -sf http://localhost:8080/health >/dev/null 2>&1; then
      ready=true
      break
    fi
    sleep 2
  done

  if [ "$ready" = true ]; then
    ok "Multica server is running"
  else
    warn "Server is still starting. You can check logs with:"
    echo "  cd $INSTALL_DIR && docker compose -f docker-compose.selfhost.yml logs"
    echo ""
  fi
}


# ---------------------------------------------------------------------------
# One-liner Computer install (RFC v6.1 §6.4)
# ---------------------------------------------------------------------------

# scrub_token removes any "mit_*" / "mdt_*" / "mul_*" token from a stream so
# diagnostic logs never leak a live credential. Matches whitespace-delimited
# token strings of the form prefix_<hex/base64-ish>. Conservative on purpose:
# false positives become "[redacted]"; we'd rather mask one extra phrase than
# leak a token.
scrub_token() {
  # POSIX-safe redaction:  prefix_<no whitespace>  →  prefix_<redacted>
  sed -E \
    -e 's/(mit_)[A-Za-z0-9._-]+/\1<redacted>/g' \
    -e 's/(mdt_)[A-Za-z0-9._-]+/\1<redacted>/g' \
    -e 's/(mul_)[A-Za-z0-9._-]+/\1<redacted>/g'
}

# diagnostics_dir returns the platform-appropriate log location for the
# install diagnostic bundle. macOS follows the user-readable
# ~/Library/Logs convention used by Console.app; Linux falls back to
# XDG_CACHE_HOME so the bundle survives reboots without polluting $HOME.
diagnostics_dir() {
  case "$(uname -s)" in
    Darwin) printf '%s/Library/Logs/Multica' "$HOME" ;;
    *)      printf '%s/Multica/logs' "${XDG_CACHE_HOME:-$HOME/.cache}" ;;
  esac
}

run_one_liner_install() {
  local workspace_slug="$1"
  local install_token="$2"
  local server_url="$3"
  local diagnose="$4"

  # Diagnostic bundle: stdout/stderr are still streamed live; we additionally
  # tee them to a redacted log file so users can attach it to a support
  # request without exposing a token.
  local diag_dir=""
  local diag_log=""
  if [ "$diagnose" = "true" ]; then
    diag_dir=$(diagnostics_dir)
    mkdir -p "$diag_dir"
    chmod 0700 "$diag_dir" 2>/dev/null || true
    # 8-char random hex; collision-resistant across same-second runs.
    local dx
    dx=$(openssl rand -hex 4 2>/dev/null || printf '%08x' "$$")
    diag_log="$diag_dir/install-dx_$dx.log"
    : > "$diag_log"
    chmod 0600 "$diag_log" 2>/dev/null || true
    # Redirect a copy of all subsequent stdout+stderr through scrub_token
    # into the log file. Using a coprocess via process substitution keeps the
    # original terminal output flowing live; only the file copy is scrubbed.
    exec > >(tee >(scrub_token >>"$diag_log")) 2>&1
    info "Diagnostic log: $diag_log (tokens are redacted)"
  fi

  printf "\n"
  printf "${BOLD}  Multica — Add Computer${RESET}\n"
  printf "  Workspace: ${BOLD}%s${RESET}\n" "$workspace_slug"
  printf "\n"

  detect_os
  install_cli

  if ! command_exists multica; then
    fail "CLI installed but 'multica' not on PATH. Re-open your shell and re-run."
  fi

  info "Registering this computer with the workspace..."
  local args=( "daemon" "start" "--install-token" "$install_token" )
  if [ -n "$server_url" ]; then
    args+=( "--server-url" "$server_url" )
  fi

  # We deliberately pass the token via argv to a trusted local binary the
  # user just installed — same process boundary as `multica login --token`.
  # The daemon CLI scrubs it from its own logs (see daemon resolveAuth);
  # we never print it ourselves.
  if ! multica "${args[@]}"; then
    printf "\n"
    fail "Failed to register this computer. The install token may have been used or expired.
Generate a new one from Add Computer in the workspace UI and re-run this script."
  fi

  printf "\n"
  printf "${BOLD}${GREEN}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${RESET}\n"
  printf "${BOLD}${GREEN}  ✓ This computer is connected${RESET}\n"
  printf "${BOLD}${GREEN}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${RESET}\n"
  printf "\n"
  printf "  Open the ${BOLD}Computers${RESET} page in the workspace to confirm it is online.\n"
  printf "  Manage agent runtimes with: ${CYAN}multica computer list${RESET}\n"
  printf "\n"
}

# ---------------------------------------------------------------------------
# Main: Default mode (install / upgrade CLI only)
# ---------------------------------------------------------------------------
run_default() {
  printf "\n"
  printf "${BOLD}  Multica — Installer${RESET}\n"
  printf "\n"

  detect_os
  install_cli

  printf "\n"
  printf "${BOLD}${GREEN}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${RESET}\n"
  printf "${BOLD}${GREEN}  ✓ Multica CLI is ready!${RESET}\n"
  printf "${BOLD}${GREEN}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${RESET}\n"
  printf "\n"
  printf "  ${BOLD}Next: configure your environment${RESET}\n"
  printf "\n"
  printf "     ${CYAN}multica setup${RESET}                # Connect to Multica Cloud (multica.ai)\n"
  printf "     ${CYAN}multica setup self-host${RESET}       # Connect to a self-hosted server\n"
  printf "\n"
  printf "  ${BOLD}Self-hosting?${RESET} Install the server first:\n"
  printf "     curl -fsSL https://raw.githubusercontent.com/multica-ai/multica/main/scripts/install.sh | bash -s -- --with-server\n"
  printf "\n"
}

# ---------------------------------------------------------------------------
# Main: With-server mode (provision self-host infrastructure + install CLI)
# ---------------------------------------------------------------------------
run_with_server() {
  printf "\n"
  printf "${BOLD}  Multica — Self-Host Installer${RESET}\n"
  printf "  Provisioning server infrastructure + installing CLI\n"
  printf "\n"

  detect_os
  check_docker
  setup_server
  install_cli

  printf "\n"
  printf "${BOLD}${GREEN}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${RESET}\n"
  printf "${BOLD}${GREEN}  ✓ Multica server is running and CLI is ready!${RESET}\n"
  printf "${BOLD}${GREEN}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${RESET}\n"
  printf "\n"
  printf "  ${BOLD}Frontend:${RESET}  http://localhost:3000\n"
  printf "  ${BOLD}Backend:${RESET}   http://localhost:8080\n"
  printf "  ${BOLD}Server at:${RESET} %s\n" "$INSTALL_DIR"
  printf "\n"
  printf "  ${BOLD}Next: configure your CLI to connect${RESET}\n"
  printf "\n"
  printf "     ${CYAN}multica setup self-host${RESET}   # Configure + authenticate + start daemon\n"
  printf "\n"
  printf "  ${BOLD}Login:${RESET} configure ${CYAN}RESEND_API_KEY${RESET} in .env for email codes,\n"
  printf "  or read the generated code from backend logs when Resend is unset.\n"
  printf "\n"
  printf "  ${BOLD}To stop all services:${RESET}\n"
  printf "     curl -fsSL https://raw.githubusercontent.com/multica-ai/multica/main/scripts/install.sh | bash -s -- --stop\n"
  printf "\n"
}

# ---------------------------------------------------------------------------
# Stop: shut down a self-hosted installation
# ---------------------------------------------------------------------------
run_stop() {
  printf "\n"
  info "Stopping Multica services..."

  if [ -d "$INSTALL_DIR" ]; then
    cd "$INSTALL_DIR"
    if [ -f docker-compose.selfhost.yml ]; then
      docker compose -f docker-compose.selfhost.yml down
      ok "Docker services stopped"
    else
      warn "No docker-compose.selfhost.yml found at $INSTALL_DIR"
    fi
  else
    warn "No Multica installation found at $INSTALL_DIR"
  fi

  if command_exists multica; then
    multica daemon stop 2>/dev/null && ok "Daemon stopped" || true
  fi

  printf "\n"
}

# ---------------------------------------------------------------------------
# Entry point
# ---------------------------------------------------------------------------
main() {
  local mode="default"
  local workspace_slug=""
  local install_token=""
  local cli_server_url=""
  local diagnose="false"
  local interactive_fallback="false"

  while [ $# -gt 0 ]; do
    case "$1" in
      --with-server) mode="with-server" ;;
      --local)       mode="with-server" ;;  # backwards compat alias
      --stop)        mode="stop" ;;
      --workspace)
        shift
        workspace_slug="${1:-}"
        ;;
      --workspace=*) workspace_slug="${1#*=}" ;;
      --token)
        shift
        install_token="${1:-}"
        ;;
      --token=*)     install_token="${1#*=}" ;;
      --server-url)
        shift
        cli_server_url="${1:-}"
        ;;
      --server-url=*) cli_server_url="${1#*=}" ;;
      --diagnose)    diagnose="true" ;;
      --interactive) interactive_fallback="true" ;;
      --help|-h)
        cat <<'USAGE'
Usage: install.sh [options]

Default (no options):
  Install or upgrade the Multica CLI. Run `multica setup` afterwards to
  authenticate and start the daemon manually.

One-liner Computer install (RFC v6.1):
  --workspace <slug>     Workspace slug the install token was minted for.
  --token <mit_…>        One-time install token from Add Computer in the UI.
  --server-url <url>     Override the server URL (default: Multica Cloud).
  --diagnose             Write a redacted log bundle to ~/Library/Logs/Multica
                         (macOS) or ~/.cache/Multica/logs (Linux). Token
                         values are never written to stdout/stderr or the log.

Self-host:
  --with-server          Install CLI + provision a self-hosted server (Docker).
  --stop                 Stop a self-hosted installation.

Hidden:
  --interactive          Skip the one-liner path and drop to the legacy
                         three-step interactive install. Intended for
                         operators recovering from a stale install token.
USAGE
        exit 0
        ;;
      *) warn "Unknown option: $1" ;;
    esac
    shift
  done

  # Either both --workspace and --token are present (one-liner Computer
  # install), or neither (legacy install-only). Mixing the two is a config
  # mistake — fail loudly so the user generates a fresh token.
  if [ -n "$install_token" ] && [ -z "$workspace_slug" ]; then
    fail "--token requires --workspace; pass --workspace <slug> as well, or omit both for the legacy install-only flow"
  fi
  if [ -n "$workspace_slug" ] && [ -z "$install_token" ]; then
    fail "--workspace requires --token; pass --token <mit_…> as well, or omit both for the legacy install-only flow"
  fi
  if [ -n "$install_token" ] && [ "${install_token#mit_}" = "$install_token" ]; then
    fail "--token must be a one-time install token (starts with 'mit_'). Generate one from Add Computer in the workspace UI."
  fi

  if [ -n "$install_token" ] && [ "$interactive_fallback" != "true" ]; then
    mode="one-liner"
  fi

  case "$mode" in
    default)     run_default ;;
    with-server) run_with_server ;;
    stop)        run_stop ;;
    one-liner)   run_one_liner_install "$workspace_slug" "$install_token" "$cli_server_url" "$diagnose" ;;
  esac
}

main "$@"
