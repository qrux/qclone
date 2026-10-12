# PROJ_META: how Q's project repos are laid out

**This file is identical in every project repo.** Don't edit it in one repo.
Change the copy in treeflow (the canonical one), copy it to every project
in `~/proj/qrux` (the monoproj; what is there is the only list of
projects, and nothing else names them), commit each, and run:

```bash
make -C ~/proj/qrux/treeflow/src proj-meta-check
```

It compares the copy on every repo's `main` and fails if any differ. (It came
from `~/jorp/README.md`, 2026-09-27, and replaces `docs/CLAUDE-OUTPUT.md`,
retired 2026-10-02.)

The monoproj was named `~/proj/claude` until 2026-10-12, when it was renamed
`~/proj/qrux`. Anything in a repo that still names `~/proj/claude` is stale,
unless it is a dated record of what was true then (star's T10, for one).

## Two homes per project

Every project lives in two places:

| Where | What it is | Who writes there |
|---|---|---|
| `~/proj/qrux/<project>` | The git repo, with its remote (GitHub or other): a real directory, the primary clone, on `main` | Q, and Claude Code sessions |
| `~/jorp/<project>` | The project's Claude folder: everything Claude produces or reads that isn't the repo. `jorp` is `proj` spelled backwards | Claude Desktop/Cowork sessions, and the logs of commands Q runs |

The split exists to avoid **SHuMA** conflicts: Single Human, Multiple Agents
working in the same directory, and the git breakage that follows (stray
`index.lock` files above all). The folder a Desktop project connects to is
`~/jorp/<project>`, which has **no writable repo in it**: only a read-only copy.

## Inside `~/jorp/<project>`

| Directory | What goes there |
|---|---|
| `bench/` | Benchmark output, from the host, the device VM or an MCP VM |
| `chat/` | The default sink for the chat |
| `docs/` | Filed documents at the top level (JSON frontmatter, categorized by the project's `_schema.json`). `docs/raw/`: unfiled write-ups. `docs/spec/`: specs, usually drafted in Claude Desktop (heavy prose, no code) and used by Claude Code |
| `logs/` | Output of commands Q runs on the host, which Claude reads when he says `kk` |
| `repos/` | Read-only mirrors of the repo, and of any repo this project depends on (for example `repos/star` beside `repos/buds`): exactly the files `main` holds, no `.git`, every entry flagged `uchg`, and `.MIRROR-REV` naming the commit. Once `make -C ~/proj/qrux/treeflow/src mirror-install` has run, each repo's `reference-transaction` hook rebuilds its mirrors and swaps them in whenever `main` moves; `make -C ~/proj/qrux/treeflow/src mirror-check` reports on them |

Some older projects also have `filed/` and `out/`; they predate this layout.

## Inside the repo

- **`_bench _chat _docs _logs _repos`**: symlinks into `~/jorp/<project>/`. Each
  machine makes its own (`make -C ~/jorp proj-<project>`), so the links
  themselves are untracked and ignored in `.gitignore` (one `/_<name>` line
  each, or a single `/_*`), and nothing behind them is ever committed through
  them.
- **`docs/`**: tracked. It holds copies of the filed documents at the top of
  `_docs/` and everything in `_docs/spec/`. `_docs/raw/` and `bench/` stay out
  of the repo.
- **`PROJ_META.md`**: this file, identical everywhere.
- **`START_HERE.md`**, where present: a dated handoff for a session starting
  cold, written by `/fresh`.
- **`.treeflow/project-type`**: the project's recorded settings, one
  `key=value` per line. `log=runlog`, `log=cleanlog` or `log=strictlog` says
  which log tool Claude Code sessions use for commands Q runs. If it's
  missing, the session asks Q once and records the answer.

## Which kind of session works where

- **Claude Desktop/Cowork** works in `~/jorp/<project>`. It never writes to the
  repo directly: repo work happens in a per-session MCP VM, made before the
  first edit, and comes back as a branch.
- **Claude Code** (the CLI and the desktop app's Code tab) works in the repo
  itself, on `main`, with no feature branches or worktrees unless Q asks
  for one, and hands Q logged commands through
  `runlog`, `cleanlog` or `strictlog` (in `~/bin`, source in treeflow).

## A new project

```bash
make -C ~/jorp proj-<project>
```

It creates `~/jorp/<project>` with the folders above, the `_*` links in the
repo, a tracked `docs/`, and the ignore rule. It's idempotent and commits
nothing. Then copy this file into the repo, and put the repo itself in
`~/proj/qrux`.
