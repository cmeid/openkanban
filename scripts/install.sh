#!/usr/bin/env bash
# install.sh — openkanban from-source bootstrap.
#
# Run from a clone of openkanban:
#     ./scripts/install.sh
#
# It will:
#   1. Sanity-check go + git at the version this module requires.
#   2. Verify $GOBIN (or $GOPATH/bin / $HOME/go/bin) is on $PATH; if not,
#      print the exact line to add to your shell rc and exit non-zero.
#      We do NOT modify shell config files for you.
#   3. `go install` the openkanban binary with -ldflags so the resulting
#      binary knows where its source clone lives (powers `openkanban update`).
#   4. If Claude Code is detected on this machine, prompt to install the
#      session-status hooks into ~/.claude/settings.json.
#   5. Print next-steps.
#
# Idempotent: safe to re-run.

set -euo pipefail

# -- self-locate ----------------------------------------------------------
# This script lives in <repo>/scripts/install.sh — repo root is one up.
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd -P)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd -P)"
cd "$REPO_ROOT"

if [ ! -f go.mod ] || [ ! -d cmd ]; then
  echo "install.sh: expected to run from an openkanban clone (no go.mod or cmd/ at $REPO_ROOT)" >&2
  exit 1
fi

# -- linked worktree detection ------------------------------------------
# A linked worktree's --git-dir points into .git/worktrees/<name>; the
# main clone's --git-dir is just ".git". Detecting this lets us build a
# local test binary instead of clobbering the global $GOBIN install.
GIT_DIR_VAL="$(git -C "$REPO_ROOT" rev-parse --git-dir 2>/dev/null || true)"
GIT_COMMON_DIR_VAL="$(git -C "$REPO_ROOT" rev-parse --git-common-dir 2>/dev/null || true)"
IS_WORKTREE=0
if [ -n "$GIT_DIR_VAL" ] && [ -n "$GIT_COMMON_DIR_VAL" ] && [ "$GIT_DIR_VAL" != "$GIT_COMMON_DIR_VAL" ]; then
  IS_WORKTREE=1
fi

# -- pretty printing ------------------------------------------------------
have_tty=0
if [ -t 1 ]; then have_tty=1; fi

bold()  { if [ "$have_tty" = "1" ]; then printf '\033[1m%s\033[0m' "$1"; else printf '%s' "$1"; fi; }
warn()  { printf '%s\n' "$(bold 'warn:') $*" >&2; }
fail()  { printf '%s\n' "$(bold 'install.sh:') $*" >&2; exit 1; }
say()   { printf '%s\n' "$*"; }
step()  { printf '\n%s\n' "$(bold "==> $*")"; }

# -- prerequisites --------------------------------------------------------
step "Checking prerequisites"

command -v git >/dev/null 2>&1 || fail "git not found. Install git (e.g. \`xcode-select --install\` on macOS, \`apt install git\` on Debian/Ubuntu)."
command -v go  >/dev/null 2>&1 || fail "go not found. Install Go from https://go.dev/dl/."

# Defensively handle a stale GOROOT env var. If it's set to a directory
# that doesn't exist, `go version` fails with "cannot find GOROOT
# directory". This usually means the Go install moved (e.g. Homebrew
# x86_64 → arm64) and a shell rc kept exporting the old path. Try
# unsetting and re-running; if that works, drop GOROOT for this script.
if ! go version >/dev/null 2>&1; then
  if [ -n "${GOROOT:-}" ] && [ ! -d "${GOROOT}" ] && (unset GOROOT && go version >/dev/null 2>&1); then
    warn "GOROOT=$GOROOT does not exist — unsetting for this install. Consider removing the stale export from your shell rc."
    unset GOROOT
  else
    fail "\`go version\` failed and the cause isn't a stale GOROOT. Run \`go version\` manually for the real error."
  fi
fi

# Parse minimum Go version from go.mod (tracks the module, no hardcoded version).
REQUIRED_GO="$(grep -E '^go [0-9]' go.mod | awk '{print $2}' | head -n1)"
if [ -z "$REQUIRED_GO" ]; then
  fail "could not parse minimum Go version from go.mod"
fi

# Go 1.21+ prints e.g. "go version go1.26.4 darwin/arm64".
GO_VERSION_RAW="$(go version | awk '{print $3}')"
GO_VERSION="${GO_VERSION_RAW#go}"

# Compare X.Y as: parse major.minor only.
version_lt() {
  # version_lt A B → "is A < B"  using sort -V
  [ "$1" = "$2" ] && return 1
  local lower
  lower="$(printf '%s\n%s\n' "$1" "$2" | sort -V | head -n1)"
  [ "$lower" = "$1" ]
}

if version_lt "$GO_VERSION" "$REQUIRED_GO"; then
  fail "Go $REQUIRED_GO+ required (go.mod), found $GO_VERSION. Upgrade Go and re-run."
fi

say "  git:  $(git --version | awk '{print $3}')"
say "  go:   $GO_VERSION (>= $REQUIRED_GO required)"

# -- GOBIN on PATH check --------------------------------------------------
# Always resolve GOBIN_DIR — needed in the closing banner for both modes.
GOBIN_DIR="${GOBIN:-}"
if [ -z "$GOBIN_DIR" ]; then
  GOPATH_DIR="${GOPATH:-$(go env GOPATH)}"
  GOBIN_DIR="$GOPATH_DIR/bin"
fi

if [ "$IS_WORKTREE" = "0" ]; then
  step "Checking install destination"
  case ":$PATH:" in
    *":$GOBIN_DIR:"*)
      say "  install dir: $GOBIN_DIR (on \$PATH)"
      ;;
    *)
      warn "install dir $GOBIN_DIR is NOT on \$PATH"
      say  ""
      say  "  Add this to your shell rc (~/.zshrc or ~/.bashrc) and reopen your shell:"
      say  ""
      say  "      export PATH=\"$GOBIN_DIR:\$PATH\""
      say  ""
      fail "PATH setup required. Re-run install.sh after adding the line above."
      ;;
  esac
fi

# -- build + install ------------------------------------------------------
LDFLAGS="-X github.com/techdufus/openkanban/cmd.SourcePath=$REPO_ROOT"
# Mark this binary as built via the canonical install path. The root
# command's PersistentPreRunE refuses to run anything except `version`
# on binaries missing this marker — that's what makes bare
# `go install .` produce a stub.
LDFLAGS="$LDFLAGS -X github.com/techdufus/openkanban/cmd.BuildMarker=official"
# Pass current commit/version when available so `openkanban version` is informative.
if git -C "$REPO_ROOT" rev-parse --short HEAD >/dev/null 2>&1; then
  COMMIT="$(git -C "$REPO_ROOT" rev-parse --short HEAD)"
  LDFLAGS="$LDFLAGS -X github.com/techdufus/openkanban/cmd.Commit=$COMMIT"
fi

if [ "$IS_WORKTREE" = "1" ]; then
  step "Building test binary (worktree — local only)"
  if ! go build -ldflags "$LDFLAGS" -o "$REPO_ROOT/openkanban" .; then
    fail "go build failed. See output above."
  fi
  INSTALLED_BIN="$REPO_ROOT/openkanban"
else
  step "Building and installing openkanban"
  GO_INSTALL_TARGET="."
  if ! go install -ldflags "$LDFLAGS" "$GO_INSTALL_TARGET"; then
    fail "go install failed. See output above."
  fi
  INSTALLED_BIN="$GOBIN_DIR/openkanban"
fi

if [ ! -x "$INSTALLED_BIN" ]; then
  fail "build reported success but $INSTALLED_BIN does not exist or is not executable"
fi

INSTALLED_SHA="$("$INSTALLED_BIN" version 2>/dev/null | awk '/^  source:/ {print $2}' | head -n1 || true)"
say "  installed: $INSTALLED_BIN"
say "  source:    $REPO_ROOT"

# -- macOS: assemble + place the OpenKanban.app bundle -------------------
# The daemon needs to run from inside an .app bundle so macOS attributes
# user-facing notifications to the OpenKanban identity (CFBundleIdentifier
# dev.cmeid.openkanban) instead of the parent terminal app. The bundle is a
# thin wrapper around the same openkanban binary we just installed —
# Contents/MacOS/openkanbankd is literally a copy of $GOBIN_DIR/openkanban.
# At fork time, internal/daemonclient prefers the bundle path over
# os.Executable() so the daemon process picks up the bundle identity.
#
# build-bundle.sh is idempotent (removes any existing OpenKanban.app at the
# target) and calls lsregister so Launch Services notices immediately.
if [ "$IS_WORKTREE" = "0" ]; then
  if [[ "$(uname -s)" == "Darwin" ]]; then
    step "Installing OpenKanban.app bundle (macOS notifications identity)"
    BUNDLE_SCRIPT="$REPO_ROOT/dist/macos/build-bundle.sh"
    if [[ -x "$BUNDLE_SCRIPT" ]]; then
      if "$BUNDLE_SCRIPT" "$INSTALLED_BIN" "$HOME/Applications"; then
        say "  OpenKanban.app installed to $HOME/Applications/OpenKanban.app"
        # Re-bootstrap the launchd service when it was previously installed.
        # build-bundle.sh runs `codesign --force --deep`, which invalidates
        # launchd's loaded job registration. Without this step, launchd fails
        # to spawn the updated binary with EX_CONFIG (78) on the next launch
        # and enters a ThrottleInterval loop — making `daemon start` /
        # `daemon restart` hang with "context deadline exceeded" until the user
        # manually runs `openkanban daemon install-service`. Re-bootstrapping
        # here prevents that wedge. Guarded so it only fires when a plist was
        # previously installed (i.e. the user already opted into launchd).
        LAUNCHD_PLIST="$HOME/Library/LaunchAgents/dev.openkanban.daemon.plist"
        if [[ -f "$LAUNCHD_PLIST" ]]; then
          if "$INSTALLED_BIN" daemon install-service >/dev/null 2>&1; then
            say "  launchd service re-bootstrapped with updated bundle binary"
          else
            warn "Could not re-bootstrap launchd service. Run: openkanban daemon install-service"
          fi
        fi
      fi
    else
      warn "dist/macos/build-bundle.sh not found or not executable; skipping bundle install."
      say  "  Notifications from the daemon will be attributed to the parent terminal app."
    fi
  fi
fi

# -- optional: Claude Code hooks ------------------------------------------
if [ "$IS_WORKTREE" = "0" ]; then
  step "Claude Code integration"

  CLAUDE_DIR="$HOME/.claude"
  if [ -d "$CLAUDE_DIR" ] && command -v claude >/dev/null 2>&1; then
    if [ "$have_tty" = "1" ] && [ -t 0 ]; then
      say "  Claude Code detected at $CLAUDE_DIR."
      say "  openkanban can install four hooks into ~/.claude/settings.json so it"
      say "  sees your session status live (working / idle / waiting). Hooks are"
      say "  idempotent and only touch sessions where openkanban set"
      say "  \$OPENKANBAN_SESSION — they no-op everywhere else."
      say  ""
      printf '  Install Claude Code hooks now? [Y/n] '
      # Read a full line so the user can confirm with Enter.
      read -r reply
      case "${reply:-y}" in
        y|Y|yes|YES)
          if "$INSTALLED_BIN" hooks install; then
            say "  hooks installed."
          else
            warn "openkanban hooks install failed. You can re-run it later with:"
            say  "      openkanban hooks install"
          fi
          ;;
        *)
          say "  skipped. You can install hooks later with:"
          say  "      openkanban hooks install"
          ;;
      esac
    else
      say "  Claude Code detected but stdin is not a TTY — skipping hooks prompt."
      say "  Run \`openkanban hooks install\` interactively to enable live status hooks."
    fi
  else
    say "  Claude Code not detected — skipping hooks step."
    say "  (Run \`openkanban hooks install\` later if you set up Claude Code.)"
  fi
fi

# -- optional: launchd background service (macOS) -------------------------
if [ "$IS_WORKTREE" = "0" ]; then
step "Background service (launchd)"

# Linux / other Unixes: no systemd backend yet, so we silently skip.
# Windows isn't reached by this script in the first place.
if [ "$(uname -s)" = "Darwin" ]; then
  if [ "$have_tty" = "1" ] && [ -t 0 ]; then
    say "  openkanbankd can run as a launchd LaunchAgent under your user account,"
    say "  independent of any TUI. Benefit: you can quit and restart the TUI as much"
    say "  as you want (e.g. while iterating on openkanban itself) and your in-flight"
    say "  agent sessions keep running. The service starts at login and exits only"
    say "  when you run \`openkanban daemon stop\`."
    say  ""
    printf '  Install openkanbankd as a launchd service now? [y/N] '
    read -r reply
    case "${reply:-n}" in
      y|Y|yes|YES)
        # `daemon install-service` refuses if any daemon is currently
        # bound to the socket — the new service would race it for the
        # pidlock. Auto-stopping a sessionless daemon is a courtesy;
        # auto-stopping one with live agent sessions would silently kill
        # in-flight work, which is what this branch must NOT do — the
        # user asked to install a service, not to destroy their work.
        proceed=1
        list_output=""
        if list_output="$("$INSTALLED_BIN" daemon list 2>/dev/null)"; then
          # Daemon reachable. Count live sessions; each session line carries
          # `running=true|false` (see `openkanban daemon list` in cmd/daemon.go).
          live_count=0
          if printf '%s\n' "$list_output" | grep -q 'running=true'; then
            live_count="$(printf '%s\n' "$list_output" | grep -c 'running=true')"
          fi
          if [ "$live_count" -gt 0 ]; then
            say "  openkanbankd is already running with ${live_count} live agent session(s)."
            say "  Skipping launchd-service install so we don't terminate them."
            say "  The freshly-installed binary will be picked up on the daemon's next"
            say "  restart (see README §\"Updates and the running daemon\")."
            say "  After those sessions finish, re-run:"
            say "      openkanban daemon install-service"
            proceed=0
          else
            say "  openkanbankd is already running with no live sessions. Stopping it so the launchd service can take over..."
            if ! "$INSTALLED_BIN" daemon stop; then
              warn "openkanban daemon stop failed or was declined. Skipping launchd install."
              say  "      Stop the daemon yourself, then run: openkanban daemon install-service"
              proceed=0
            fi
          fi
        fi
        if [ "$proceed" = "1" ]; then
          if "$INSTALLED_BIN" daemon install-service; then
            say  ""
            say  "  To prevent the TUI from also forking its own daemon, add the following"
            say  "  to ~/.config/openkanban/config.json (top-level key):"
            say  ""
            say  '      "daemon": { "autostart": false }'
            say  ""
            say  "  Or pass --no-launch-daemon when invoking openkanban."
          else
            warn "openkanban daemon install-service failed. You can re-run it later with:"
            say  "      openkanban daemon install-service"
          fi
        fi
        ;;
      *)
        say "  skipped. You can install later with:"
        say  "      openkanban daemon install-service"
        ;;
    esac
  else
    say "  macOS detected but stdin is not a TTY — skipping launchd prompt."
    say "  Run \`openkanban daemon install-service\` interactively to set it up."
  fi
else
  say "  launchd-based service install is macOS-only. On Linux, run \`openkanban daemon --persistent\`"
  say "  under your own process supervisor (systemd user unit, tmux, etc.)."
fi
fi # IS_WORKTREE=0

# -- closing banner -------------------------------------------------------
if [ "$IS_WORKTREE" = "1" ]; then
  step "Done (worktree build)"
  say ""
  say "  Built a test binary inside this worktree:"
  say "    $INSTALLED_BIN"
  say ""
  say "  Run it with:  ./openkanban"
  say "  Your global install ($GOBIN_DIR/openkanban) was NOT modified."
  say "  To update the global install, run install.sh from the main clone on main."
  say ""
  exit 0
fi

step "Done"

cat <<EOF

openkanban is installed.

Next steps:
  cd <a-git-repo>
  openkanban new "Project Name"   # register the repo as a project
  openkanban                       # launch the TUI

Day-to-day:
  openkanban                       # checks for updates on launch (Enter to update, Esc to skip)
  openkanban update --check        # just print update status
  openkanban update                # pull + rebuild + reinstall from $REPO_ROOT

Standardized ticket close-out:
  The 'finishing-an-openkanban-ticket' skill installs itself into
  ~/.claude/skills/ on first launch (and refreshes on every 'openkanban
  update'). It self-evaluates with code-review / validation subagents;
  install the oh-my-claude plugin to enable them (the skill degrades to
  self-review without them, and openkanban warns at launch if missing).

EOF

# Surface the legacy-shim cleanup hint only when the shim actually exists —
# otherwise it reads as a confusing instruction to remove a missing file.
if [ -e "$HOME/.local/bin/update-openkanban" ]; then
  cat <<EOF
Already have ~/.local/bin/update-openkanban from before? You can remove it:
    rm ~/.local/bin/update-openkanban
\`openkanban update\` replaces it.

EOF
fi
