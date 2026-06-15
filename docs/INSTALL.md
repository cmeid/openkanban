# Installing openkanban from source

`scripts/install.sh` is the supported way to install openkanban from a
clone. This page explains what it does, what prerequisites it expects,
and how to undo it.

## Prerequisites

- **git** — any modern version. macOS users get it from `xcode-select --install`.
- **Go** — the version pinned in `go.mod` (parse with `grep ^go go.mod`).
  Install from <https://go.dev/dl/>.
- **`$GOBIN` (or `$GOPATH/bin`) on `$PATH`.** The script does NOT modify
  your shell rc; it prints the exact `export PATH=…` line you need and
  exits non-zero if the directory is missing from `$PATH`.

Optional:

- **Claude Code** — if `~/.claude/` exists and `claude` is on `$PATH`,
  the installer offers to wire session-status hooks (see below).

## What `scripts/install.sh` does

1. **Self-locates** the repo root from its own path. You can invoke it as
   `./scripts/install.sh` or `make install`.
2. **Checks prerequisites**: `git`, `go`, and the Go version required by
   `go.mod`.
3. **Verifies `$GOBIN` on `$PATH`**. If not, prints the export line and
   exits — does not modify shell rc files.
4. **Builds + installs**: `go install -ldflags "-X …/cmd.SourcePath=<repo>"`
   so the resulting binary knows where its source clone lives. This
   powers `openkanban update`.
5. **Detects Claude Code** and (if present, on a TTY) prompts:

   > openkanban can install four hooks into `~/.claude/settings.json`
   > so it sees your session status live (working / idle / waiting).
   > Hooks are idempotent and only touch sessions where openkanban set
   > `$OPENKANBAN_SESSION` — they no-op everywhere else.

   Accept → runs `openkanban hooks install`. Decline → no change. The
   hooks command is itself idempotent; rerun any time.
6. **Prints next steps** and a hint to remove a legacy
   `~/.local/bin/update-openkanban` if you had one.

The installer is **idempotent**: re-running it rebuilds (Go's cache makes
this fast), re-checks PATH, and re-prompts the Claude Code question.

## What gets installed where

| Path | Contents |
|---|---|
| `$GOBIN/openkanban` | the binary |
| `~/.claude/settings.json` | four hook entries (only if you opted in) |
| `~/.config/openkanban/` | created lazily by the binary on first run |
| `~/.cache/openkanban-status/` | created lazily by status hooks |

The installer itself touches only the binary and (with consent) the
Claude Code settings file. It does not write to your shell config, your
filesystem outside `$GOBIN`, or any system-level paths.

## Updating

Three options:

- **Launch-time prompt** — on every `openkanban` invocation, the binary
  checks `origin/main` and shows a one-line prompt if you're behind.
  **Enter** to apply + relaunch, **Esc** to skip, **Q** to quit.
- **On demand** — `openkanban update --check` to print status,
  `openkanban update` to pull + rebuild + reinstall.
- **Disable** — pass `--no-update-check`, or set
  `behavior.check_for_updates_on_launch: false` in
  `~/.config/openkanban/config.json`.

The update flow **refuses** when the source clone would silently build
the wrong tree: source on a non-`main` named branch, detached HEAD, not
a git repo (unreadable / wiped), or a linked git worktree. Each refusal
exits cleanly with an actionable message that names the offending state
and the manual `git checkout main && git pull --ff-only origin main`
recipe to fix it. The non-`main` named-branch case additionally offers a
one-keystroke "switch to main & update" prompt (**Enter** to apply,
**Esc** to skip, **Q** to quit). The linked-worktree case has no offer
— `git checkout main` would refuse because `main` is already checked
out in the original clone; the refusal points you at that clone
instead.

The launch-time check also surfaces every non-empty status on stderr
("up to date", "ahead", "diverged", refusal messages) so it's never
silent about why auto-update is or isn't doing anything on a given run.

For binaries installed via Homebrew or `go install …@latest`, the update
flow prints upgrade instructions instead of attempting an in-place pull
(`SourcePath` is empty in release builds). On TTY launches, the binary
also prints a one-line stderr notice that auto-update is disabled and
how to re-enable it (`./scripts/install.sh` from a clone). Strictly
advisory — startup proceeds normally.

## Troubleshooting

### `$GOBIN` not on `$PATH`

The installer prints the exact line:

```bash
export PATH="$GOBIN_DIR:$PATH"
```

Add it to `~/.zshrc` (zsh) or `~/.bashrc` (bash), reopen your shell, and
re-run `./scripts/install.sh`.

### `go install` fails with a Go version error

`go.mod` pins a minimum Go version. The installer parses it and refuses
to continue with an older Go. Upgrade Go from <https://go.dev/dl/>.

### Launch-time update check is slow

The check has a hard 1500 ms timeout and falls through silently on
failure. If your `origin` is unreachable, the TUI still starts. Disable
the check with `--no-update-check` or the config flag if you want zero
network on launch.

### "Update available" prompt appears even after I update

Make sure the freshly-installed binary is the one on `$PATH`. Run
`openkanban version` — the `source:` line shows which clone it was built
from. If you have multiple clones, install from the one you want
`update` to track.

## Removal

```bash
rm "$(command -v openkanban)"            # binary
rm -rf ~/.config/openkanban              # projects + config
rm -rf ~/.cache/openkanban-status        # status markers
```

To remove the Claude Code hooks: open `~/.claude/settings.json` in your
editor and delete the four `hooks` entries whose `command` field starts
with `openkanban status set`. (There is no `openkanban hooks uninstall`
subcommand yet.)
