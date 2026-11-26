# aeolian

an open-source orchestrator: name a workflow and a goal, and Claude Code agents carry it out in isolated worktrees while a live canvas draws every run as it happens.

## Workflows

A workflow is a YAML file of `steps`, each with a prompt template, and the helper `roles` its steps may spawn; see `examples/` for complete ones. An optional top-level `description:` says in a line what the workflow is for:

```yaml
description: Build a change, then check it.
steps:
  - name: build
    prompt: "Build {{.Goal}}."
  - name: check
    prompt: "Check {{.Goal}}. {{.Handoff}}"
```

Keep the workflows you use in `$AEOLIAN_HOME/workflows/` (`~/.local/share/aeolian/workflows/` when `AEOLIAN_HOME` is unset), one `.yaml` file each. `GET /api/workflows` lists every `.yaml` directly in that folder, sorted by name:

```json
[{"name": "two-step", "description": "Build a change, then check it.", "steps": ["build", "check"], "contents": "…", "error": ""}]
```

`name` is the file name without `.yaml` and `contents` is the file as written. A file that cannot be read or parsed is still listed, with the reason in `error` and empty `description` and `steps`. A missing folder lists as `[]`.

Starting a run still posts the workflow's full source to `POST /api/runs`.

A step or role can set `dir:` (an absolute path or `~/…`) to start its agent in that existing folder instead of a scratch folder. A step whose `dir` is missing fails; a spawn into a missing role `dir` is refused. Worktrees handed to a helper win over its role's `dir`. aeolian never removes or untrusts a `dir`.

A run can take a `dir` too: `dir` on `POST /api/runs`, or `aeolian run --dir D`, which reads a relative `D` against the shell's folder and `~/…` as the home folder. The host takes an existing absolute folder only, else the run is refused with 400. It is stored on the run, and every step agent and helper of the run starts there unless a handed worktree or a step or role `dir:` says otherwise: worktrees, then the yaml `dir:`, then the run's `dir`, then a scratch folder.

## Ends, the cap and the watchdog

`max_agents` caps the agents *working*: busy and not blocked in `spawn` or `await`. An agent idle at its prompt holds no slot, so when a person types into one, or a handoff is typed in, the count can go over the cap for a while. The same goes for an agent waiting on its own background task (a Bash run with `run_in_background`): Claude Code stops its turn, so the agent reads as idle, holds no slot, and the stall watchdog does not watch it, until the task's `<task-notification>` wakes it. That prompt is logged as Claude Code's (`by: claude`), not as a person's message. This is a known gap: the cap and the watchdog do not yet see background work.

A run ends `done` with a `reason` beside the state: `delivered` when its last step called done, `ended` when it was ended on purpose, `closed` when its interactive lead's terminal was closed while idle. An agent whose session the kernel's out-of-memory killer ended at its `memory:` limit (default `1G`) ends with reason `oom`, busy or idle, and its step and run fail with it; the host learns this by asking systemd how the agent's scope ended. `aeolian end [--force] <run-id>` (`POST /api/runs/{run}/end`, answers 202) ends a busy run: its helpers are stopped, an interactive lead is asked to call done within `end_grace` (default `90s`), and otherwise the host writes the lead's handoff from its last message. A lead can end its own run with the `end_run` tool.

A headless lead fans out with `spawn` and `wait: false`, then collects the handoffs with `await` (`mode: all|any`, `timeout_s`). The sweep's watchdog ends a busy helper that shows no sign of life (no hook, no transcript write, and no CPU used by its systemd scope between two sweeps) for `stall_after` (default `20m`); its parent wakes with a handoff saying so. It also closes an agent that already handed back, was resumed, and then sat idle at its prompt for `idle_close_after` (default `30m`); an agent that never handed back is never idle-closed. `await` with no ids also returns the handoffs of helpers that finished before a host restart and never reached their parent. Set them in `config.json` as durations, or `"off"`.

The watchdog only catches an agent that shows no sign of life; one that keeps calling tools in a loop looks alive forever. For that a run takes a context ceiling: `aeolian run --context-cap N [--cap-kills K]`, or `context_cap` and `cap_kills` on `POST /api/runs`. After each tool call, a headless agent whose `context` (see Events) is over `context_cap` tokens is stopped `failed` with reason `budget`. A helper stopped this way hands back to its parent a note saying it reached its cap and to spawn a fresh helper with that handoff; the lead decides whether to. A step agent stopped this way fails its step and the run, `failed`/`budget`. Once `cap_kills` helpers of the run (default `3`; witnesses do not count) have ended `budget`, the run ends `failed`/`budget` and every agent of it is stopped, as `aeolian kill` would. A witness stopped at the cap leaves its claims `unwitnessed`. `context_cap` `0` means no ceiling; `cap_kills` without `context_cap`, or either negative, is refused with 400. Both are stored on the run and in `run.created`. Interactive agents are never stopped, since a person is watching the bar. Pick a cap below Claude Code's autocompact threshold: the ceiling means "replace the agent instead of compacting it", and aeolian does not touch compaction.

For a spend ceiling a run takes a token cap: `aeolian run --token-cap N`, or `token_cap` on `POST /api/runs`. It sums the `input`, `output` and `cache_write` of every `usage` event of the run (lead, helpers and witnesses, across compactions); `cache_read` does not count, since every turn re-reads the cached context and that would charge a long agent its whole context per turn. Once the sum is over `token_cap`, the run ends `failed`/`budget` and every agent of it, interactive ones too, is stopped, as `aeolian kill` would. `token_cap` `0` means no cap, a negative one is refused with 400, and it is stored on the run and in `run.created`. `GET /api/runs` and `GET /api/inbox` carry `tokens`, the run's sum so far, and `token_cap`. There is no warning threshold.

## Run lanes

A run started with a `source` (an adapter's, or `"source"` on `POST /api/runs`) waits `queued` while the sourced runs busy reach `max_runs` (default 1), and starts oldest first as they end. A source named in `max_runs_by_source` gets a lane of its own: it is capped by its own number and does not count toward `max_runs`, so a slow lane never starves the others and the others never fill it:

```json
{"max_runs": 2, "max_runs_by_source": {"ekky-qa": 1}}
```

## ekky QA

This adapter is for ekky, a separate task runner that is not part of this repository. Skip this section and `examples/ekky-rfc.yaml` if you don't use ekky.

`adapters/ekky` builds `aeolian-ekky`, which watches the ekky task tree (`$EKKY_ROOT`, default `~/ekky`) and starts a run of `$AEOLIAN_HOME/workflows/qa-ekky.yaml` for each task that reaches `done` in a repo listed under `repos` in `config.json`, once per head of its `ekky/` branch: source `ekky-qa`, ref `<rfc>/<slug>@<head>`. A rework moves the head, so the reworked task gets a fresh run; a task already promoted gets none. The goal names the task record, the branch, the head and the diff from its upstream task or its base. Give `ekky-qa` a lane of one, since its runs share one local environment. `aeolian-ekky due` prints what the next poll would start. `$AEOLIAN_HOME/ekky-adapter.json` may set `ekky_root`, `workflow`, `bundle_root` (default `$AEOLIAN_HOME/qa-runs`) and `interval` (default `30s`).

## Events

Every change the host makes is appended to an event log in the same SQLite transaction as the change itself, so the tables can always be rebuilt from it. `GET /api/events` returns events in id order:

```json
[{"id": 41, "at": "2026-11-09T23:16:46Z", "run_id": "…", "agent_id": "…", "kind": "tool.used", "data": {"v": 1, "tool_name": "Bash", "input": "{\"command\":\"go test ./...\"}", "ok": true}}]
```

Every parameter is optional: `after` (an event id; read on from the last one you saw), `run`, `agent`, `kind` (repeat it or separate kinds with commas) and `limit` (1 to 1000, the default). A page shorter than its limit is the end of the log.

The kinds and their payloads are listed in `internal/store/kinds.go`. Payloads carry a version `v` and only ever gain fields. Hook input is kept in `hook` events cut to 8 KB; `PostToolUse` becomes `tool.used` with the tool's input cut to 1 KB and no response; a prompt a person typed also becomes `human.message`; `usage` events count the tokens each agent's transcript gained, read after every tool call and when a turn or session ends, and carry the agent's `context`: the input plus cache read plus cache write of its latest request, the number Claude Code's status bar shows, which drops after a compaction. `GET /api/runs` shows each agent's current `context`, kept in memory and seeded from its last `usage` event after a restart.

`events_retention_days` in `config.json` (default 30; 0 keeps everything) is how long a finished run keeps its `hook`, `tool.used` and `human.message` payloads. After that they are reduced to their size; the events themselves stay.

`aeolian debug fold <aeolian.db>` checks that a database's event log folds back into its tables. It works on a copy, so it is safe to point at the live database.

## Witness

A step with `witness: true` must hand back a `claims` part: at most 12 claims (`witness.max_claims`), each with evidence (`path:line`, a URL or `cmd: …`). `done` refuses claims without evidence, or with a `path:line` past the end of its file, and the agent fixes them and calls `done` again.

Once the step's `done` is stored, the host starts one headless **witness** in the producer's first worktree, or its own scratch folder. The witness sees only the claims, excerpts of their evidence, and the fresh precedents in scope. It never sees the producer's prompt or transcript. It calls `verdict` on each claim (`holds`, `contradicted` or `unverifiable`, with a note), then `done`. The run does not move on, and `on_finish` does not run, until it finishes. The next step then sees the verdicts as a `verdicts` part. If no verdict comes within `witness.timeout` (default 20m), or the witness ends without one, or the run is killed, the claims are marked `unwitnessed` and the witness is killed. Precedents past their `review_by` are listed by id as stale and never applied: the witness can't cite them.

`witness` is a reserved role: a workflow may not define it. A witness has no `spawn` or `await` tool and no grants, and it can give verdicts only on its parent's claims. Its model defaults to `sonnet`.

Optional `config.json` section:

```json
{"witness": {"all": false, "model": "sonnet", "timeout": "20m", "max_claims": 12, "evidence_optional": false, "precedents": ""}}
```

`all` witnesses every step agent that hands back claims, without requiring them. `precedents` defaults to the host's folder (`precedents_dir`, else `$AEOLIAN_HOME/precedents`), and `"none"` turns them off.

```sh
aeolian claims <run-id>                  # verdicts, contradicted first
aeolian witness-replay <cases-dir>       # replay past corrections; fails below 8/10 or if w02/w04/w09 fail
```

`witness-replay` runs on a host of its own, with a temporary `AEOLIAN_HOME` and a private tmux socket. It never touches the live host. Each case's `excerpt` is fed in as its evidence's content. By default the case's `precedent_ids` are in scope (`--match` uses the matcher instead, and `--precedents none` runs without precedents). The fixtures are in `internal/host/testdata/witness-replay`. The replay's witnesses boot lean; `--boot full` runs them with your own Claude Code setup to compare.

## Digest and precedents

`aeolian digest` is the page to read when you come back: only what needs you from the runs that finished since `--since` (default `12h`; also `2d`, a day or an RFC 3339 time). Items are ranked by what they cost you if ignored (a run blocked on a question, an arena waiting on your pick between its top two (until its `pick.md` has a label), a run that died, a claim a witness contradicted, helpers that died, a stale precedent, an unverifiable claim, unchecked claims) and capped at 7 (`--max`, `0` for all), with the rest counted. When nothing needs you it is one line. `--md` renders it for a daily note, a checkbox per item; `--run <id>` digests one run. `GET /api/digest?since=&run=&max=&all=` returns the same view as JSON.

An item stays until its cause resolves, it ages out, or you clear it. Every item has a key (`dead:<run>`, `contradicted:<run>:<claim>`, `stale_precedent:<id>`, …). `aeolian digest dismiss <key>...` (`POST /api/digest/dismiss {"keys": [...]}`) hides it, and acking a run (`aeolian ack`, the inbox, the map) hides that run's items. Either way only what is already there is hidden: a later occurrence of the same thing (a new verdict on the claim, the precedent matching again) shows again. A run with a source is left to dismissals, since its adapter acks it on report. `--all` shows cleared items too, marked, so nothing is lost. `examples/digest-to-daily-note.sh` is an `on_finish` script that appends each run's digest to the note you point it at; aeolian never writes to your notes itself.

A precedent is a correction you already made once, kept as a markdown file in `$AEOLIAN_HOME/precedents/` (or `precedents_dir` in `config.json`, e.g. a synced vault folder):

```markdown
---
id: p01-dev-ledger-project-branch
kind: correction
scope: [dev-ledger, branch]
review_by: 2027-02-01
source: "run 3f2a, claim 3f2a-lead-4"
---
dev-ledger is a PROJECT branch, not drift.
```

A precedent can also say who it is for, beside or instead of words:

```markdown
---
id: p07-leads-ask-before-merging
kind: safety
scope: ["*"]
roles: [lead]
repos: [aeolian]
review_by: 2027-02-01
source: "said in the 2026-11-20 freeform run"
---
Ask the person before merging anything into main.
```

`scope: ["*"]` means no word has to appear; quote it, since a bare `*` is YAML for an alias, and it must be the only term. An empty or missing scope is still an error, so a slip never makes a precedent global. `roles` lists roles, matched exactly and case-insensitively against the agent's role, or its step name for a step agent, so `roles: [lead]` reaches the freeform lead. `repos` lists repos, matched against the checkout the agent works in: its main checkout's folder name (a worktree counts as its repo) or its key in `config.json`'s `repos`. An agent in a scratch folder works in no repo. Any one listed role or repo is enough; words, roles and repos must all hold. With `["*"]` and neither `roles` nor `repos`, the precedent is **global**: it reaches every agent, whatever the workflow.

Every agent the host starts, step or helper (fanned out with `wait: false`, in a worktree, or an arena proposer or evolver), gets the precedents that reach it, up to 8. The targeted ones come first, most specific first (one point per scope word, one for `roles`, one for `repos`; ties go safety, correction, convention, then id), then the global ones, safety first. Up to 3 of the 8 slots are kept for global precedents, so a pile of specific matches cannot crowd out a rule meant for everyone, and slots either side leaves empty go to the other. A witness gets its own claim-scoped list instead, matched on the words of each claim only, so global, role and repo precedents never reach it; an arena judge gets none, so blind judging stays blind. One past `review_by` is left out and flagged in the digest instead. `aeolian precedent add --scope a,b "text"` writes one (`--role lead,builder` and `--repo aeolian` narrow it, alone or with `--scope`; `--global` writes one for every agent), `aeolian precedent list [--stale]` lists them with whom each reaches, and `aeolian precedent from-claim <claim-id> "what is true"` turns a wrong claim into one (`POST /api/precedents`, which takes `global`, `roles` and `repos` too).

## Boot profiles

Each agent boots with one of two profiles:

- **full**: your own Claude Code setup, as when you run `claude` yourself: your `~/.claude/settings.json`, `CLAUDE.md`, output style, skills, plugins and custom agents.
- **lean**: `claude --setting-sources project,local --disable-slash-commands --exclude-dynamic-system-prompt-sections`. Your user settings, `CLAUDE.md`, skills, plugins and custom agents stay behind. The agent's own `settings.json` sets the default output style and `autoMemoryEnabled: false`, and carries over only `apiKeyHelper`, `env`, `forceLoginMethod`, `forceLoginOrgUUID`, `model`, `modelSettings` and `effortLevel` from yours. It also sets `skipDangerousModePermissionPrompt: true`, because your consent to bypass mode lives in the user settings it skips. Without it an interactive agent would stop at that dialog. Auth, the host's MCP server and its hooks are the same in both profiles. The project's `CLAUDE.md` and `.claude/settings.json` in the agent's worktree still load.

Witness, `judge`, `proposer` and `evolver` helpers boot lean. Every other agent boots full. A workflow step or role can set `boot: lean` or `boot: full`. That wins over the optional `config.json` section:

```json
{"boot": {"default": "full", "roles": {"witness": "lean", "judge": "lean", "researcher": "lean"}, "confine": ["witness", "judge"]}}
```

`roles` is merged over the defaults above. `default` applies to every agent not covered by a role, step agents included. Measured with `AEOLIAN_BOOT_COST=1 go test ./internal/agent -run TestBootCost -v`, which makes two real calls: on one setup the first call's context drops from 24.7k tokens (full) to 17.3k (lean).

**Confined reads.** Helpers in a `confine` role (default: `witness` and `judge`; `[]` turns it off) get `permissions.blockReadsOutsideWorkingDirectories`. They can read only their working directory, their own scratch folder and the worktrees they were handed. They are also denied `agents/**`, `aeolian.db*`, top-level `*.json`, `workflows/**` and `vault/**` under `$AEOLIAN_HOME`. Claude Code applies the block in bypass mode too, to Bash commands as well as to the file tools. A confined helper that is resumed later gets its working directory and scratch back, but not extra worktrees from its spawn.

## claude.ai connectors

A step or role granted `claudeai:<connector>` (e.g. `claudeai:slack`) runs without `--strict-mcp-config` and can use that claude.ai connector; every other server outside its grants is denied. The connector must show as Connected in `claude mcp list`, or the agent does not start.

`default_grants` in `config.json` adds connectors to every agent except witnesses. It defaults to `["claudeai:claude_docs"]`; `[]` turns it off. A default connector that is missing or not Connected is left out with a line in the host log, and the agent starts without it.

```json
{"default_grants": ["claudeai:claude_docs"]}
```

## Accounts

A run can use one of several Claude accounts. Log in to each once in its own config folder, then name them in `config.json`:

```sh
CLAUDE_CONFIG_DIR=~/.claude-work claude
```

```json
{"accounts": {"main": "", "work": "~/.claude-work"}, "default_account": "main"}
```

An empty folder means the host's own config, the one plain `claude` uses. Do not write `~/.claude` for it: with `CLAUDE_CONFIG_DIR` set, Claude Code reads `.claude.json` from inside that folder instead of `~/.claude.json`, and so starts logged out.

Pick the account when the run starts: `aeolian run --account work wf.yaml "goal"`, `aeolian arena --account work "question"`, `"account": "work"` on `POST /api/runs` or `POST /api/arenas`, or the account menu in the map's start form. A run that names none gets `default_account`. Without `accounts` in `config.json`, runs use the host's own Claude Code config, as before. An unknown name is refused with 400. `aeolian accounts` (`GET /api/accounts`) lists the names.

The run keeps its account, and shows it as `account`. Every agent of the run starts with `CLAUDE_CONFIG_DIR` set to that folder: the lead, helpers, witnesses, arena seats, resumes, and agents started after a host restart. Trust entries go to that folder's `.claude.json`, a lean boot carries settings from its `settings.json`, and the host reads transcripts from its `projects/`. A full boot loads that folder's `CLAUDE.md`, skills, plugins and output style, so link them in if both accounts should share them. A run whose account was removed from `config.json` cannot start more agents.

## Run env

A run can put env vars and git config into every one of its agents:

```sh
aeolian run --env AYA_ITEM=x --git-config remote.origin.pushurl=/nonexistent wf.yaml "goal"
```

```json
{"workflow": "...", "goal": "...", "env": {"AYA_ITEM": "x"}, "git_config": [["remote.origin.pushurl", "/nonexistent"]]}
```

Both flags repeat, and each splits on its first `=`, so a value may hold `=` of its own. `env` is a map; `git_config` is a list of `[key, value]` pairs, kept in order, so a multi-valued key can be given more than once.

Every agent of the run gets them, headless or interactive: the step agents, helpers, witnesses, resumes, and agents started after a host restart. Helpers cannot override them; the `spawn` tool, workflow yaml, arenas and `config.json` take no env. On a lean boot `env` wins over the `env` carried from your `settings.json`. `git_config` reaches git through `GIT_CONFIG_COUNT`/`GIT_CONFIG_KEY_<n>`/`GIT_CONFIG_VALUE_<n>`, after the host's own entries (the credential helpers of a headless or `github`-granted agent), so a headless agent's credentials still work.

Keys aeolian sets itself are refused with 400, and by `aeolian run` before it posts: `GH_TOKEN`, `GITHUB_TOKEN`, `GH_CONFIG_DIR`, `SSH_AUTH_SOCK`, `GIT_SSH_COMMAND`, `GIT_TERMINAL_PROMPT`, `GIT_ASKPASS`, `MCP_TOOL_TIMEOUT`, the `GIT_AUTHOR_*`/`GIT_COMMITTER_*` identity and `CLAUDE_CONFIG_DIR` (`env: GH_TOKEN is reserved by aeolian`), and `GIT_CONFIG_COUNT`, `GIT_CONFIG_PARAMETERS` and any `GIT_CONFIG_KEY_*`/`GIT_CONFIG_VALUE_*` (`...; pass git settings as git_config`). A `git_config` key git would not take, one that is not `section[.subsection].name`, is refused the same way (`git_config: pushurl is not a git config key`). A `git_config` key the host writes itself is refused too (`git_config: credential.helper is set by aeolian`): the list is whatever the headless and `github`-granted git setup writes, today `credential.helper` and `credential.https://github.com.helper`, matched the way git matches keys, so section and name ignore case and the subsection does not.

Neither is secret: both are stored in plain text on the run, shown on it as `env` and `git_config`, and written to the event log in `run.created`. Do not pass tokens this way.

## GitHub

Agents granted `github` or `github-read` get a GitHub token in `GH_TOKEN`, and headless agents commit as the identity of the `github` section of `config.json`. The section takes one of two forms.

**Your own token.** The host runs the command at each grant and hands its output to the agent:

```json
{"github": {"token_command": ["gh", "auth", "token"]}}
```

The command runs without a shell, on the host's `PATH` (for a systemd unit, make sure `gh` is on it), and must print only the token. Headless agents commit as `git config --global user.name` and `user.email`; the host refuses to start when either is unset. The token is **not scoped**: an agent holding it has everything your `gh` login has, on every repo, and `github-read` gets the same token as `github`. The workflow's prompts are the only guard against writes.

**A GitHub App.** The host mints installation tokens scoped to the repos under `repos` with a `github` owner/name (at least one is needed), with `contents: read` for `github-read` and `contents`, `pull_requests: write` for `github`. Headless agents commit as the App's bot.

```json
{"github": {"app_id": 123456, "private_key_file": "github-app.pem"}}
```

`token_command` and `app_id`/`private_key_file` cannot be mixed.
