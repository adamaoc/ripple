# Ripple Bot API

Ripple is a local project backlog and agent runner with a JSON API designed for coding agents and automation.

## Discovery

Start with:

```http
GET /api
```

The response links to this guide and the OpenAPI document:

```http
GET /api/docs
GET /api/openapi.yaml
```

## Core Rules

- Every story belongs to a project.
- An epic is optional.
- Story descriptions are Markdown.
- Intended flow is `backlog -> queued -> in_progress -> in_review -> done` (autonomous runs may skip visible `in_review` and go straight to `done` after merge).
- Bots may only set story status to `backlog`, `in_progress`, or `done`.
- `queued` and `in_review` are human/orchestrator-only. Bots must not set them.
- Bots must not close stories. Closing is a manual human review action in the UI.
- If a user says work is complete, move the story to `done`, not `closed`.
- Closed stories are hidden from the default board and default story list.

## Human-Friendly IDs

Stories use project-prefixed IDs such as `TXG-001` or `RV-001`.

Projects have a required prefix. When creating a project through a story request, provide `projectPrefix` if the user has a clear preference. If no prefix is known, choose a short uppercase prefix from the project name.

Projects may also have a `workingDirectory`. Use the Git repository root, or the folder where Codex should start work for that project. If a project already exists without a working directory, providing one in a later project or story request fills it in. Existing working directories are not overwritten silently.

Projects may set `autonomyMode` to `autonomous` (default) or `supervised`. Autonomous runs implement, review, and merge without waiting. Supervised runs implement, open a PR, post an agent review, then stop with the story in `in_review` until a human acts: address review comments, merge the PR (with quality gate), or sync if the PR was already merged on GitHub. Invalid values are stored as `autonomous`. Agents cannot set status to `in_review`, `queued`, or `closed`.

Optional delivery fields (all have safe defaults that preserve current behavior):

| Field | Default | Effect |
|-------|---------|--------|
| `defaultBranchOverride` | empty (auto-detect) | Checkout branch before runs when auto-detect is wrong |
| `prBaseBranch` | empty (use default branch) | `gh pr create --base` |
| `qualityGateMode` | `strict` | `strict` fails the run/merge on check errors; `warn` logs and continues |
| `deleteBranchOnMerge` | `true` | Delete the feature branch on GitHub and locally after merge |
| `branchNameTemplate` | `ripple/{id}-{slug}` | Feature branch name; placeholders `{id}`, `{slug}`, `{prefix}` (must include `{id}`) |

## Create a Project

Use this when you know the project before creating stories.

```http
POST /api/projects
Content-Type: application/json

{
  "id": "txgarage",
  "name": "TXGarage",
  "prefix": "TXG",
  "workingDirectory": "/Users/adamm/Documents/WEBPROJECTS/Sites and Apps/TXGarage",
  "autonomyMode": "autonomous",
  "defaultBranchOverride": "",
  "prBaseBranch": "",
  "qualityGateMode": "strict",
  "deleteBranchOnMerge": true,
  "branchNameTemplate": "ripple/{id}-{slug}"
}
```

`autonomyMode` is optional. Omit it (or pass an empty/invalid value) to default to `autonomous`. Delivery fields above are optional on create.

## Create an Epic

Use this when grouping related stories.

```http
POST /api/epics
Content-Type: application/json

{
  "projectId": "txgarage",
  "name": "Mobile polish",
  "description": "Cleanup work for the mobile experience."
}
```

## Create a Story

Use this when the user asks to add, create, track, remember, or file a task, bug, feature, or work item.

You can reference an existing project:

```http
POST /api/stories
Content-Type: application/json

{
  "projectId": "txgarage",
  "title": "Add saved vehicle filter",
  "description": "Add a filter so users can view saved vehicles only.",
  "status": "backlog"
}
```

Or create/find a project and epic while creating the story:

```http
POST /api/stories
Content-Type: application/json

{
  "projectName": "Real View",
  "projectPrefix": "RV",
  "workingDirectory": "/Users/adamm/Documents/WEBPROJECTS/Sites and Apps/RealView",
  "epicName": "Listing workflow",
  "title": "Show listing preview before publish",
  "description": "Render a Markdown-friendly preview of the listing before it goes live."
}
```

If `status` is omitted, the server uses `backlog`.

## List Stories

Default listing excludes closed stories:

```http
GET /api/stories
```

Filter by project, epic, or status:

```http
GET /api/stories?projectId=txgarage
GET /api/stories?epicId=txgarage-mobile-polish
GET /api/stories?status=in_progress
GET /api/stories?projectId=txgarage&status=backlog
```

Include closed stories only when the user specifically asks for archived or closed work:

```http
GET /api/stories?showClosed=1
```

## Update a Story

Use this to change title, description, or epic.

```http
PATCH /api/stories/TXG-001
Content-Type: application/json

{
  "description": "Updated Markdown description."
}
```

## Move a Story

Use this to move work through the bot-writable workflow.

```http
PATCH /api/stories/TXG-001/status
Content-Type: application/json

{
  "status": "in_progress"
}
```

Allowed bot statuses:

- `backlog`
- `in_progress`
- `done`

Do not attempt to set `queued`, `in_review`, or `closed`; the API rejects them.

## Event History

Use this when you need to understand what happened to a story.

```http
GET /api/stories/TXG-001/events
```
