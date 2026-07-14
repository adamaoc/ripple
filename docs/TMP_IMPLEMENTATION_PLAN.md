# Temporary implementation plan: autonomy, agents, and project settings

> **Temporary document.** Track and implement the work below step by step. Delete this file (and any references to it) once every phase is complete and shipped.

**Repo:** Ripple (TheTaskManager)  
**Created:** 2026-07-14  
**Status:** Phase 5 complete — ready for Phase 6



 

---

## 1. Product goals

Ripple today hardcodes one delivery loop:

```text
branch → Codex implement → commit/push → open PR → Grok review
  → optional Codex fix → quality gate → merge → story done
```

We will evolve it into a **configurable delivery system** with two clear axes:

| Axis | Scope | Purpose |
|------|--------|---------|
| **Tooling** | App-level Settings | Which agents/APIs implement and review work |
| **Autonomy** | Per project | How far automation may go without a human |

Secondary goals:

- Easier local workspace setup per project
- Richer per-project settings beyond `workingDirectory`
- Keep **done = merged** always

---

## 2. Locked decisions

These are agreed product rules. Do not re-litigate during implementation unless a hard technical conflict appears.

### 2.1 Agent / provider choice — global

- Configured on an **app-level Settings** page (not per project, for now).
- Roles:
  - **Implementer** — writes code, applies fix passes
  - **Reviewer** — independent PR review
- Surfaces:
  - **CLI adapters** (start with existing Codex + Grok CLIs)
  - **API integrations** (API keys, model, optional base URL) so additional agents can be added without a local CLI
- One global binding: which provider fills Implementer vs Reviewer.

### 2.2 Autonomy — per project

- Some projects may run fully autonomous (today’s behavior).
- Others need hand-holding: agent opens PR and reviews, then **stops for the human**.
- Autonomy is resolved **per story’s project** when a multi-project queue runs.
- Agent choice is **not** per project (global only).

### 2.3 Supervised delivery loop

For projects in **supervised** mode:

1. Agent implements, commits, pushes, opens PR.
2. Agent reviewer still runs and posts review feedback.
3. Pipeline **stops** — does **not** auto-fix from agent-only feedback, does **not** merge.
4. Story waits for human action (see status below).
5. Human reviews on GitHub (and may leave comments).
6. Human opens the story and clicks **Act on review comments**:
   - Agent reads **human** review comments and the **prior agent** review.
   - Agent applies changes, commits, pushes.
   - Pipeline **stops again** (still not merged).
7. Human chooses:
   - **Act on review comments** again (another loop), or
   - **Merge pull request** → merge → story **done**.

### 2.4 Status semantics

| Status | Meaning |
|--------|---------|
| `backlog` | Not queued |
| `queued` | In execution queue (human decision) |
| `in_progress` | Agent is actively implementing or fixing |
| `in_review` | **New.** PR open; ball is with the human |
| `done` | **Always means merged** |
| `closed` | Human archived after review |

While waiting on PR feedback, the story is **`in_review`**, never `done`.

### 2.5 Queue behavior while waiting

If story A is `in_review`, the queue **continues** with later stories. Human latency must not block the entire run.

### 2.6 Quality gate

| When | Behavior |
|------|----------|
| After **Act on review comments** | **Skip** (fast iteration; no quality gate) |
| On **Merge pull request** | **Required** — same quality gate as today before merge |

### 2.7 External merge

If the human merges on GitHub outside Ripple, Ripple should detect that and allow transition to `done` so the board stays honest. Prefer an explicit **Sync PR status** control first (Phase 2e); do not silently rewrite status without a clear user path.

### 2.8 Supervised edge cases (locked)

| Topic | Decision |
|-------|----------|
| **No new comments** on “Act on review comments” | Clear UI message; **do not** start an agent run; story stays `in_review`. |
| **Agent review says “request changes”** at first stop | Still **wait for human**. No automatic fix pass in supervised mode. |
| **Autonomy mode changed** while a story is mid-pipeline | **Ignored for in-flight work.** New mode applies only when a **new** queue run starts for that project’s stories. |
| **Board column for `in_review`** | **Yes** — between In progress and Done. |
| **Bot API may set `in_review`?** | **No.** Orchestrator/human only (same class of restriction as `queued` / `closed`). |
| **Local git cleanup after supervised merge** | **Yes** — checkout default branch and delete feature branch, same as autonomous. |
| **Run workspace for supervised pause** | Show a distinct **waiting on you** state per story (not “completed/merged”). |
| **Concurrent agent work** | Keep a **single global agent lock**. Queue runs and manual “Act on review comments” do not interleave file work on shared trees. |

### 2.9 Settings, keys, and providers (locked)

| Topic | Decision |
|-------|----------|
| **API key storage** | SQLite (provider/app config tables). Local trusted-machine model. **Never** log keys; **mask** in UI. |
| **Encrypt keys at rest** | **Not in v1.** Document that Ripple is local-first and keys are as protected as the machine/DB file. |
| **First HTTP API shape** | **OpenAI-compatible** chat/completions (covers many hosts). Keep Codex + Grok **CLI** paths. |
| **API providers as Implementer** | **Not in v1.** API integrations may be bound as **Reviewer** only. Implementer stays CLI-based until a deliberate tool-loop design exists. |
| **Project create** | API create remains. Phase 5 adds UI create + setup verify; not required for Phases 1–4. |

---

## 3. Decision log (formerly open questions)

All items below are **locked**. Implementers must follow these; do not re-open during a phase unless a hard technical conflict forces a plan amendment (update this section if that happens).

| # | Decision | Applies in |
|---|----------|------------|
| D1 | Quality gate **off** on “Act on review comments”; **required** on merge | Phase 2 |
| D2 | No new comments → message only, no agent run, stay `in_review` | Phase 2 |
| D3 | Supervised mode never auto-fixes from agent-only “request changes” | Phase 2 |
| D4 | Autonomy mode changes apply to **next** run only, not in-flight stories | Phase 1–2 |
| D5 | API keys in SQLite provider config; never log; mask in UI | Phase 3–4 |
| D6 | First API integration: OpenAI-compatible HTTP (reviewer) | Phase 4 |
| D7 | Board includes **In review** column between In progress and Done | Phase 2 |
| D8 | Bot API cannot set `in_review` (orchestrator/human only) | Phase 2 |
| D9 | Supervised merge performs same local branch cleanup as autonomous | Phase 2 |
| D10 | Project UI create/verify is Phase 5; API create stays available | Phase 5 |
| D11 | No at-rest encryption for API keys in v1; document trust model | Phase 3 |
| D12 | Run UI shows distinct “waiting on you” for supervised pauses | Phase 2 |
| D13 | Implementer = CLI only in v1; API providers are reviewer-capable only | Phase 4 |
| D14 | Single global agent lock for queue runs and manual feedback actions | Phase 2 |

---

## 4. Target architecture (high level)

```text
Global Settings
  ├── providers[] (cli | api)
  ├── implementerProviderId
  └── reviewerProviderId

Project
  ├── workingDirectory          (existing)
  ├── autonomyMode              autonomous | supervised
  └── (later) setup metadata

Queue run (per story)
  ├── resolve global Implementer + Reviewer
  ├── resolve project.autonomyMode
  └── pipeline:
        autonomous  → … → merge → done
        supervised  → … → agent review → in_review → (human loops) → merge → done
```

### 4.1 New pipeline phases

Add phases (names can be adjusted during implementation, keep stable in DB):

| Phase | Meaning |
|-------|---------|
| `awaiting_human` | Stopped after PR + agent review (or after a fix pass) |
| `addressing_feedback` | Human-triggered fix pass in progress |
| `merging` | Existing |
| `completed` | Existing (merged path finished) |

Autonomous mode continues to use existing phases through `merging` → `completed` without entering `awaiting_human`.

### 4.2 Agent runner interface (Phase 3–4)

Introduce a thin interface so Codex/Grok CLIs and HTTP APIs share one call path:

```text
type AgentRunner interface {
  // role: implement | review | fix
  Run(ctx, req AgentRunRequest) (AgentRunResult, error)
}
```

Do **not** build a plugin marketplace. Two CLI implementations + one HTTP API implementation is enough.

### 4.3 Human-triggered endpoints (Phase 2)

| Method | Path | Action |
|--------|------|--------|
| `POST` | `/stories/{id}/address-feedback` | Collect PR comments → run fixer → push → back to `in_review` |
| `POST` | `/stories/{id}/merge` | Quality gate → merge → `done` |
| `POST` | `/stories/{id}/sync-pr` (optional) | If PR already merged externally → `done` |

Prefer form posts + redirect consistent with existing UI patterns (`data-current-redirect`).

---

## 5. Implementation phases (do one by one)

Work **in order**. Each phase should be mergeable on its own with tests. Check off items as you go.

---

### Phase 0 — Prep and scaffolding (small)

**Goal:** Make later phases easy without shipping user-facing behavior yet.

#### Steps

1. **Add this plan to the repo** (this file) — done when committed.
2. **Inventory current touchpoints** (read-only mental map; update this section if wrong):
   - Pipeline: `pipeline.go` (`runStoryPipeline` / phases / merge)
   - Agent binaries: `resolveCodexBinary`, `resolveGrokBinary`, `resolveGhBinary`
   - Project model: `Project` in `main.go`, `projects` table (`working_directory` only extra field)
   - Story statuses: constants in `main.go`; board/backlog filters
   - Settings UI: `templates/settings.html` (theme only)
   - Story panel: `templates/story_panel.html`
   - Run UI: `templates/run.html`
   - Tests: `main_test.go`, `pipeline_test.go`
3. **Decision log is locked** (Section 3 / §2.6–2.9). Implement phases against those rules; only amend the plan if a hard technical conflict appears.

#### Acceptance criteria

- [x] Plan file exists; product decisions and former open questions are locked.
- [ ] No product behavior change required in Phase 0.

#### Suggested commit

```text
docs: add temporary autonomy and agents implementation plan
```

---

### Phase 1 — Per-project autonomy mode (data + UI, behavior still autonomous)

**Goal:** Store and edit `autonomyMode` per project. Default remains **autonomous** so nothing breaks. Pipeline still fully auto until Phase 2.

#### 1.1 Database

1. Add column on `projects`:
   - `autonomy_mode TEXT NOT NULL DEFAULT 'autonomous'`
2. Use existing `ensureColumn` migration pattern (same as `working_directory`).
3. Valid values: `autonomous`, `supervised`.

#### 1.2 Model and persistence

1. Extend `Project` struct with `AutonomyMode string` (`json:"autonomyMode"`).
2. Update all project `SELECT` / `INSERT` / `UPDATE` queries.
3. Add helper: `normalizeAutonomyMode(s string) string` → default `autonomous` on empty/invalid.
4. API create/update project paths (if any) should accept optional `autonomyMode` without requiring it.

#### 1.3 UI — project settings

1. In backlog project settings (`templates/backlog.html` and board equivalents if duplicated):
   - Add control: **Autonomy**
     - Autonomous — “Implement, review, and merge without waiting.”
     - Supervised — “Open PR and review, then wait for you to act on feedback and merge.”
2. Wire form post, e.g. `POST /projects/{id}/settings` or extend existing working-directory form carefully (prefer a dedicated settings form if the current one is path-only).
3. Show current mode on dashboard row subtitle if space allows (optional polish).

#### 1.4 Pipeline hook (no behavior change yet)

1. Thread `project.AutonomyMode` into `pipelineContext`.
2. Add a clearly named function stub or early branch:

   ```text
   if project.AutonomyMode == "supervised" {
     // Phase 2 will stop after agent review
   }
   ```

   Either leave a `// TODO Phase 2` no-op or implement a feature flag comment only — **do not** stop the pipeline until Phase 2 tests exist.

#### 1.5 Tests

1. Project save/load round-trip for `autonomyMode`.
2. Invalid value normalizes to `autonomous`.
3. Default for existing DB rows is `autonomous`.
4. UI/settings form posts succeed (httptest).

#### Acceptance criteria

- [x] Existing projects behave exactly as today.
- [x] User can set supervised vs autonomous on a project and it persists.
- [x] API/JSON project payloads include `autonomyMode` when listing/getting projects.
- [x] `go test ./...` green.

#### Suggested commit

```text
Add per-project autonomy mode setting (default autonomous).
```

#### Files likely touched

- `main.go` (schema, Project, handlers)
- `templates/backlog.html` (and board if needed)
- `main_test.go`
- `docs/bot-api.md` / `docs/openapi.yaml` if project schema is documented

#### Completed

- Column `projects.autonomy_mode` + `ensureColumn` migration (default `autonomous`)
- `Project.AutonomyMode`, `normalizeAutonomyMode`, create/list/get/update paths
- UI: backlog + board project settings; dashboard subtitle shows mode
- Pipeline stub after agent review for supervised (`// TODO Phase 2`) — no behavior change
- Tests + bot-api / OpenAPI docs

---

### Phase 2 — Supervised pipeline stop + human actions

**Goal:** Supervised projects stop after PR + agent review; humans drive feedback loops and merge. This is the highest product-value phase.

Implement as sub-phases if the PR gets large: **2a → 2b → 2c → 2d → 2e**.

---

#### Phase 2a — Status `in_review` + pipeline stop

1. **Add status constant** `StatusInReview = "in_review"`.
2. Update allowed transitions:
   - `in_progress` → `in_review` (orchestrator)
   - `in_review` → `in_progress` (when address-feedback starts)
   - `in_review` → `done` (only after merge)
   - Bot API: agents **cannot** set `in_review`, `queued`, or `closed` (extend existing restrictions).
3. **Board / backlog / filters**
   - Include `in_review` in status filters and board columns.
   - Labels: “In review”.
4. **Pipeline change for supervised mode** after agent review is posted:
   - Do **not** run automatic fix from agent “request changes”.
   - Do **not** run quality gate or merge.
   - Set pipeline phase `awaiting_human`.
   - Set story status `in_review`.
   - Add story event, e.g. `awaiting_human_review` with PR URL.
   - Treat the **story pipeline item** as successfully paused (queue run continues to next story). Define run-item status carefully:
     - Prefer something like `awaiting_human` vs `completed`/`failed` so the run UI does not look fully “done.”
5. **Autonomous mode** path unchanged (including one auto fix pass).

##### Tests (2a)

- Supervised project: pipeline ends in `awaiting_human`, story `in_review`, PR exists, no merge.
- Autonomous project: still merges and marks `done` (existing behavior).
- Queue with two stories: first supervised pause does not prevent second from starting.

##### Acceptance (2a)

- [x] Supervised stop works end-to-end in tests (mock or subprocess-level as existing tests do).
- [x] Board shows In review.
- [x] Autonomous regression covered.

##### Completed (2a)

- Status `in_review` (valid + UI-writable; bot API blocked)
- Board column, backlog tab, status selects, About workflow line
- Supervised path: after agent review → phase `awaiting_human`, story `in_review`, event `awaiting_human_review`, no fix/merge
- Queue item status `awaiting_human`; queue continues to later stories
- Autonomous path still marks `done` after merge

---

#### Phase 2b — “Act on review comments”

1. **Collect feedback** via `gh`:
   - PR review bodies, review comments, and issue comments (human + bot).
   - Include stored `ReviewJSON` from agent review on the pipeline row.
2. **Build fix prompt** (new builder, sibling of `buildCodexFixPrompt`):
   - Emphasize human comments as primary.
   - Include agent review as secondary context.
   - Same git constraints as today (no merge, no status changes).
3. **Handler** `POST /stories/{id}/address-feedback`:
   - Validate story is `in_review` and has pipeline row with PR.
   - Validate project working directory / branch still valid.
   - Collect comments first. If **no actionable comments** (locked D2): return a clear message, **do not** start an agent run, stay `in_review`.
   - Otherwise set story → `in_progress`, phase → `addressing_feedback`.
   - Run implementer/fixer (still Codex CLI until Phase 4 abstraction). **No quality gate** after this step (locked D1).
   - Commit if changes exist; push.
   - If the agent ran but produced no code changes, still return to `in_review` with an event explaining no changes.
   - Phase → `awaiting_human`, story → `in_review`.
4. **Concurrency (locked D14):** reuse the **single global agent lock**. Queue runs and manual “Act on review comments” share one agent activity slot so they cannot stomp the same working tree.
5. **UI** on story panel when `status == in_review`:
   - Primary button: **Act on review comments**
   - Show PR link prominently
   - Short help text describing the loop

##### Tests (2b)

- Happy path: comments present → fix run recorded → status back to `in_review`.
- No comments: no agent process (or immediate no-op) + message.
- Wrong status rejects with 400.

##### Acceptance (2b)

- [x] Human can trigger fix passes without merging.
- [x] Multiple loops allowed.
- [x] Events/transcript show address-feedback runs.

##### Completed (2b)

- `POST /stories/{id}/address-feedback` with global agent lock
- Collect PR reviews/comments/issue comments via `gh` + stored agent `ReviewJSON`
- No-comments / no-new-comments (fingerprint) → clear 400, stay `in_review`, no agent
- Fix pass: `in_progress` → `addressing_feedback` → codex → commit/push if dirty → `awaiting_human` / `in_review`
- No quality gate on this path
- Story panel: Act on review comments + PR link when `in_review`

---

#### Phase 2c — “Merge pull request”

1. **Handler** `POST /stories/{id}/merge`:
   - Validate `in_review` + PR number.
   - Run quality gate (required).
   - `gh pr merge` (same flags as today unless project later customizes).
   - Local branch cleanup (checkout default, delete feature branch) as today.
   - Story → `done` with event `agent_completed` / `merged_by_human` as appropriate.
   - Pipeline phase → `completed`.
2. **UI:** secondary/danger-appropriate **Merge pull request** button next to Act on feedback.
3. Confirm copy: “Merges the PR and marks the story done.”

##### Tests (2c)

- Merge success → `done`.
- Quality gate failure → stay `in_review`, error visible.
- Autonomous path still auto-merges without this button.

##### Acceptance (2c)

- [x] Supervised stories only reach `done` via merge (button or Phase 2e external sync).

##### Completed (2c)

- `POST /stories/{id}/merge` under global agent lock (sync so quality-gate errors return to UI)
- Quality gate required; on failure stay `in_review` with `quality_gate_failed` event
- `gh pr merge` + local branch cleanup; story `done`, phase `completed`, event `merged_by_human`
- Story panel **Merge pull request** button next to Act on review comments

---

#### Phase 2d — Run workspace + events UX

1. Run completion summary distinguishes:
   - Merged PRs
   - PRs awaiting human
2. Live/agent activity copy does not say “merged” for supervised pauses.
3. Story events human-readable for: awaiting review, addressing feedback, merged by human.
4. About page / workflow line updated: include **In review** between In progress and Done.

##### Acceptance (2d)

- [x] Run page is not misleading for supervised outcomes.
- [x] About/docs mention supervised flow at a high level.

##### Completed (2d)

- Run completion summary splits **Merged** vs **Awaiting you** (queue-item + PR outcomes)
- Sidebar shows human labels (`waiting on you`, `merged`) instead of raw statuses
- Story history uses `eventTitle` for supervised events
- About page workflow + autonomous vs supervised steps; bot-api supervised wording updated

---

#### Phase 2e — External merge sync (small but important)

1. Explicit **Sync PR status** button on the story panel (locked with §2.7):
   - If PR is merged on GitHub and story is `in_review` → transition to `done` (with event).
2. Do not silently auto-rewrite status on every page load without a clear user control.

##### Acceptance (2e)

- [x] Board does not strand stories that were merged outside Ripple.

##### Completed (2e)

- `POST /stories/{id}/sync-pr` — checks PR merged via `gh pr view`; no silent page-load rewrite
- On success: story `done`, phase `completed`, event `merged_externally`, queue item completed, best-effort local branch cleanup
- Story panel **Sync PR status** button when `in_review`

---

#### Phase 2 suggested commits

```text
Add in_review status and supervised pipeline stop after PR review.
Add address-feedback action for supervised stories.
Add human merge action to finish supervised stories.
Polish run UI and docs for supervised delivery.
Sync externally merged PRs to done.
```

#### Files likely touched

- `pipeline.go`, `main.go`
- `templates/story_panel.html`, `board.html`, `backlog.html`, `run.html`, `about.html`
- `static/styles.css`
- `main_test.go`, `pipeline_test.go`
- `docs/bot-api.md`, `docs/openapi.yaml`, `README.md` (brief)

---

### Phase 3 — App Settings: provider registry + role binding (CLIs first)

**Goal:** Move tool configuration into Settings UI; persist globally; wire pipeline to resolved binaries/roles. Still only Codex + Grok CLIs under the hood, but structure supports API later.

#### 3.1 Storage

1. New table `app_settings` (key/value) **or** structured tables:

   Use structured tables (not a single opaque blob):  
   - `agent_providers` (id, kind, name, config_json, created_at, updated_at)  
   - `app_config` (implementer_provider_id, reviewer_provider_id, …)

2. Seed defaults on first boot:
   - Provider `codex_cli` (implementer)
   - Provider `grok_cli` (reviewer)
   - Bind roles accordingly.

3. Config fields for CLI providers:
   - `binaryPath` optional override (else env + auto-detect existing logic)
   - display name

#### 3.2 Settings UI

Expand `templates/settings.html` beyond Appearance:

1. **Nav sections:** Appearance | Agents | (later) Advanced  
2. **Agents section:**
   - Implementer select
   - Reviewer select
   - Per-CLI path overrides
   - Status probes: “Codex found”, “Grok found”, “gh found” (run version checks server-side)
3. Save via POST forms; success flash or redirect.

#### 3.3 Resolution layer

1. Replace direct `resolveCodexBinary()` call sites for **role** resolution with:

   ```text
   resolveImplementer(ctx) → runner config
   resolveReviewer(ctx) → runner config
   ```

2. Keep env vars `RIPPLE_CODEX_BIN` / `RIPPLE_GROK_BIN` as overrides that win over settings (document precedence):

   ```text
   env override > settings path > auto-detect
   ```

3. Start-run preflight uses role resolution (clearer errors: “Implementer not configured”).

#### 3.4 Tests

- Default seed works on empty DB.
- Settings update changes resolution.
- Env override still wins.

#### Acceptance criteria

- [x] User can view/edit agent roles and CLI paths in Settings.
- [x] Runs still work with zero user config (defaults = today’s behavior).
- [x] No API keys required yet.

#### Suggested commit

```text
Add global agent settings with CLI provider roles.
```

#### Completed

- Tables `agent_providers` + `app_config`; seed `codex_cli` / `grok_cli` with role bindings
- Settings → Agents UI: implementer/reviewer selects, CLI path overrides, tool status probes (Codex/Grok/gh + version)
- `POST /settings/agents` save with redirect flash; explicit path validated as executable
- Resolution: `resolveImplementer` / `resolveReviewer` with precedence env > settings path > auto-detect
- Queue preflight, Codex runs, Grok review, and address-feedback use role resolution
- Tests: seed, settings update, env override wins, reject bad path; settings page markers

---

### Phase 4 — AgentRunner abstraction + first API integration

**Goal:** Support “2 CLIs + custom API integrations” as specified.

#### 4.1 Interface

1. Define `AgentRunner` + request/result types in a dedicated file e.g. `agents.go` (or `runner.go`).
2. Implement:
   - `CodexCLIRunner` (existing exec JSON event path)
   - `GrokCLIRunner` (existing headless review path)
   - `HTTPAPIRunner` (generic chat/completions-style; start OpenAI-compatible)

3. Map roles → runner instances from Settings.

4. Generalize run_kind values if needed:
   - Prefer role-based labels in UI (“Implementer”, “Reviewer”) while keeping DB kinds stable or versioned.

#### 4.2 API provider settings

1. UI form: name, base URL, API key, model, optional headers.
2. Mask API key in UI (`••••` + “replace key” field).
3. Never write keys to story events, transcripts, or logs.
4. Test connection button (optional but valuable): minimal models list or tiny completion.

#### 4.3 Review contract for API reviewers

1. Reuse structured JSON review schema (`approved`, `summary`, `comments[]`) already used for Grok.
2. Prompt must require that JSON shape so `parseGrokReview` can be generalized to `parseAgentReview`.

#### 4.4 Implementer contract for API implementers

1. Harder than CLI: API agents cannot natively edit files.
2. **v1 constraint (locked D13):**  
   - **Implementer:** CLI only (Codex CLI; optionally other CLIs later).  
   - **Reviewer:** CLI **or** OpenAI-compatible HTTP API.  
   - **API providers may not be bound as Implementer** until a deliberate tool-loop design exists.

   Document this limit in Settings help text so users are not surprised. **Do not half-implement implementer-via-API.**

#### 4.5 Tests

- Reviewer HTTP mock server returns JSON → pipeline accepts review.
- Invalid API key surfaces clean error.
- CLI path still default green.

#### Acceptance criteria

- [x] User can add an API key provider and select it as Reviewer.
- [x] Implementer remains reliable via CLI.
- [x] Keys never appear in run transcripts.

#### Suggested commits

```text
Introduce AgentRunner interface for implement and review roles.
Add OpenAI-compatible API provider for reviews.
```

#### Completed

- `AgentRunner` interface + `CodexCLIRunner`, `GrokCLIRunner`, `HTTPAPIRunner` (`agents.go`)
- Shared review contract: `AgentReview` / `parseAgentReview` (Grok aliases retained)
- API providers: create/update/delete/test; key masked in Settings; stored in `agent_providers.config_json`
- Reviewer role may bind Grok CLI **or** API; implementer stays Codex CLI only (D13)
- Pipeline/review path uses `newReviewerRunner` / `runReviewerForStory`; implement path uses `newImplementerRunner`
- UI labels: Implementer / Reviewer; Settings → API providers section + trust-model note
- Tests: mock HTTP review, invalid key, form create, cannot bind API as implementer, key not in settings/transcripts

---

### Phase 5 — Easier local workspace setup

**Goal:** Reduce friction configuring a project’s code workspace and verifying readiness.

#### 5.1 Project setup checklist (server-side)

Per project, compute:

| Check | Pass condition |
|-------|----------------|
| Path set | `workingDirectory` non-empty |
| Path exists | directory exists |
| Git repo | `.git` or `git rev-parse` |
| GitHub remote | `gh` can see repo / remote URL parseable |
| Clean tree | optional warning if dirty (do not hard-fail setup; preflight already cares at run) |
| Implementer available | global resolve succeeds |
| Reviewer available | global resolve succeeds |
| `gh` auth | `gh auth status` |

Expose as:

- `GET /projects/{id}/setup-status` (HTML fragment or JSON)
- Rendered in project settings panel

#### 5.2 UI improvements

1. **Verify setup** button on project settings.
2. Red/yellow/green checklist with copy-paste fix hints.
3. **Detect git root** if user picks a subdirectory (offer “use repo root instead”).
4. Optional: **Clone from GitHub URL**
   - Input URL + parent directory (default under home `RippleWorkspaces/` or similar)
   - `git clone` then set `workingDirectory`
   - Requires careful path validation (no path traversal)

#### 5.3 Project create in UI (optional but aligned)

1. Simple modal/page: name, prefix, working directory, autonomy mode.
2. Still keep API create for agents.

#### 5.4 Tests

- Checklist endpoints for missing path / non-git path.
- Clone rejected for invalid URL (if implemented).

#### Acceptance criteria

- [x] New users can see *why* a project is not runnable without reading About.
- [x] Folder picker + verify flow feel complete for local repos.

#### Suggested commit

```text
Add project workspace setup checks and clearer project onboarding.
```

#### Completed

- `projectSetupStatus`: path, exists, git, GitHub remote, clean tree (warn), implementer, reviewer, `gh auth`
- `GET /projects/{id}/setup-status` HTML fragment + JSON (`Accept: application/json`)
- Project settings: **Verify setup** checklist, clone from GitHub, use-git-root when path is a subfolder
- Folder picker offers **Use repo root instead** when browsing a subdirectory
- Dashboard **Create project** form (name, prefix, path, autonomy); API create unchanged
- Tests: missing path, non-git path, invalid clone URL, UI create, backlog markers

---

### Phase 6 — Per-project settings expansion (beyond autonomy)

**Goal:** Grow project policy without clutter. Only add fields with a clear pipeline consumer.

#### Candidates (implement as needed, not all required)

| Setting | Use |
|---------|-----|
| `default_branch_override` | When auto-detect is wrong |
| `pr_base_branch` | Same / explicit |
| `skip_review` | Autonomous internal tools (optional; careful) |
| `quality_gate_mode` | `strict` / `warn` |
| `delete_branch_on_merge` | bool |
| `branch_name_template` | advanced |

#### Steps

1. Add columns only when Phase 2–5 consumers exist or are in the same PR.
2. Project settings UI sections: Workspace | Delivery | Advanced.
3. Document each in bot-api/OpenAPI if API-visible.

#### Acceptance criteria

- [ ] No dead settings (every control changes runtime behavior).
- [ ] Defaults preserve current behavior.

#### Suggested commit

```text
Expand project delivery settings used by the pipeline.
```

---

### Phase 7 — Docs, polish, and remove this plan

**Goal:** Ship coherent documentation and delete temporary planning artifacts.

#### Steps

1. Update `README.md`:
   - Autonomy modes
   - Global agent settings
   - Status workflow including `in_review`
   - Constraints (API reviewer vs CLI implementer, local trust model)
2. Update `docs/bot-api.md` and `docs/openapi.yaml`:
   - New statuses, project fields, forbidden agent transitions
3. Update About page workflow diagram.
4. Screenshots if UI changed substantially (README product tour).
5. Full test pass: `go test ./...`, `go vet ./...`.
6. **Delete this file:** `docs/TMP_IMPLEMENTATION_PLAN.md`
7. Remove links to it if any were added.

#### Acceptance criteria

- [ ] New contributor understands autonomous vs supervised without reading chat history.
- [ ] No temporary plan file left in the repo.

#### Suggested commit

```text
Document autonomy and agent settings; remove temporary plan.
```

---

## 6. Cross-cutting engineering rules

1. **Local-first, low-dependency** — no new frontend build step; keep HTMX + Go templates.
2. **One active agent mutation at a time** — protect working trees (existing agent lock).
3. **Done means merged** — never mark `done` on PR open alone.
4. **Tests for every status/pipeline branch** — supervised vs autonomous matrix.
5. **Secrets** — never log API keys; never embed in prompts printed to UI beyond redaction.
6. **Migrations** — additive columns with defaults; no destructive resets of `taskmanager.db`.
7. **Feature sequencing** — do not start Phase 4 API implementers without an explicit scope expansion.
8. **Commits** — prefer one logical commit per phase (or per 2a/2b/… sub-phase).

---

## 7. Dependency graph

```text
Phase 0 Plan
    ↓
Phase 1 Project autonomyMode (data/UI)
    ↓
Phase 2 Supervised pipeline + human actions + in_review
    ↓
Phase 3 Global agent Settings (CLI roles)
    ↓
Phase 4 AgentRunner + API reviewer
    ↓
Phase 5 Workspace setup UX
    ↓
Phase 6 Extra project settings (as needed)
    ↓
Phase 7 Docs + delete this plan
```

Phases 5 and 6 can partially overlap with 3–4 if staffing allows, but **2 depends on 1**, and **4 depends on 3**.

---

## 8. Definition of done (whole initiative)

- [x] Projects choose **autonomous** vs **supervised**. *(setting only; supervised stop ships in Phase 2)*
- [x] Supervised: PR + agent review → human feedback loops → human merge → `done`. *(includes external GitHub merge sync)*
- [x] `in_review` visible on board; `done` only after merge. *(includes external sync)*
- [x] Global Settings configure implementer/reviewer (CLI + API key providers for review).
- [x] Workspace setup checklist makes local project onboarding obvious.
- [ ] Docs/About/README match behavior.
- [ ] `go test ./...` green.
- [ ] This temporary plan file is removed.

---

## 9. Progress log

| Phase | Status | Notes / PR / commit |
|-------|--------|---------------------|
| 0 Prep | done | Plan + decision log locked |
| 1 Autonomy mode setting | done | Data + UI + API; pipeline still fully autonomous until Phase 2 |
| 2a `in_review` + stop | done | Supervised pause after review; board/backlog In review |
| 2b Address feedback | done | Act on review comments; loops back to in_review |
| 2c Human merge | done | Quality gate + merge + done; panel button |
| 2d Run UX / About | done | Completion summary, event titles, About supervised flow |
| 2e External merge sync | done | Sync PR status → done when merged on GitHub |
| 3 Global agent settings | done | CLI provider registry + role binding + Settings UI |
| 4 Runner + API reviewer | done | AgentRunner + OpenAI-compatible API reviewer |
| 5 Workspace setup | done | Checklist, verify, clone, git-root, UI project create |
| 6 More project settings | not started | |
| 7 Docs + delete plan | not started | |

Update this table as phases complete.

---

## 10. Quick reference — UI copy (draft)

**Project autonomy**

- **Autonomous** — Ripple implements, reviews, and merges on its own.
- **Supervised** — Ripple implements and opens a PR, runs an agent review, then waits. You act on feedback and merge when ready.

**Story panel (in review)**

- **Act on review comments** — Have the agent read the latest PR feedback and push fixes. The story will wait for you again.
- **Merge pull request** — Run checks, merge the PR, and mark the story done.
- **Sync PR status** — If you merged on GitHub already, update the story to done.

**Settings → Agents**

- **Implementer** — Tool that writes code for stories.
- **Reviewer** — Tool that reviews pull requests before you (or autonomous mode) continue.

---

*End of temporary plan. Delete after Phase 7.*
