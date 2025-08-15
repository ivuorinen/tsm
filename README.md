# tsm — ultra-light tmux session manager (Go 1.24)

Features:
- Live built-in fuzzy filter UI (no external `fzf`)
- Scans Git repos concurrently (one goroutine per `scan_paths` root)
- **Max depth 3** by default
- Session name from folder + parent: `/Code/ivuorinen/a` → `ivuorinen_a`
- Existing tmux sessions listed and selectable
- Bookmarked folders always shown
- XDG config, no macOS `Library` default

## Install

```shell
go install github.com/ivuorinen/tsm@latest
```

## Usage

- Run `tsm` → live filter list opens. Type to filter, navigate with keys below, Enter to select.
- Selecting a session switches to it. Selecting a repo/bookmark creates (or reuses) a session and switches/attaches.

### Keybindings

| Key            | Action                                  |
|----------------|-----------------------------------------|
| ↑ / Ctrl-P     | Move up                                 |
| ↓ / Ctrl-N     | Move down                               |
| **Home**       | Jump to first item                      |
| **End**        | Jump to last item                       |
| **PgUp**       | Move up 5 items                         |
| **PgDn**       | Move down 5 items                       |
| **Backspace**  | Delete one character from query         |
| **Ctrl-U**     | Clear query                             |
| **Tab**        | Toggle preview (path + planned action)  |
| **Enter**      | Select                                  |
| **Ctrl-C**     | Cancel                                  |

### tmux key binding (popup)

```text
bind-key t display-popup -E 'tsm' -w 90% -h 80%
```

## Config

XDG-only:
- `$XDG_CONFIG_HOME/tsm/config.yaml` or fallback `$HOME/.config/tsm/config.yaml`

Create a default config:

```bash
tsm -init-config
```

Example:

```yaml
scan_paths:
  - "$HOME/Code"
bookmarks:
  - "$HOME"
exclude_dirs:
  - ".git"
  - "node_modules"
  - "vendor"
  - "dist"
  - "build"
  - "target"
  - "out"
  - "bin"
  - ".cache"
  - ".next"
  - ".nuxt"
  - ".pnpm-store"
  - ".yarn"
  - ".yarn/cache"
  - ".venv"
  - ".direnv"
  - "deps"
  - "_build"
  - ".terraform"
  - ".terragrunt-cache"
  - ".m2"
  - ".gradle"
  - "Pods"
  - "Carthage"
max_depth: 3
```

## Flags

- `-config PATH` : set explicit config file path
- `-print`       : print candidate list (Kind, Name, Path) and exit
- `-init-config` : write default config to XDG path and exit

## Tests

```bash
go test ./...
```

Tests mock tmux and validate name derivation,
fuzzy ranking, scanning (depth/excludes),
and tmux command flow.
