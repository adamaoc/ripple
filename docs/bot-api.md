# TheTaskManager Bot API

TheTaskManager is a local task manager with a JSON API designed for chat bots and automation agents.

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
- Intended flow is `backlog -> in_progress -> done`.
- Bots may only set story status to `backlog`, `in_progress`, or `done`.
- Bots must not close stories. Closing is a manual human review action in the UI.
- If a user says work is complete, move the story to `done`, not `closed`.
- Closed stories are hidden from the default board and default story list.

## Human-Friendly IDs

Stories use project-prefixed IDs such as `TXG-001` or `RV-001`.

Projects have a required prefix. When creating a project through a story request, provide `projectPrefix` if the user has a clear preference. If no prefix is known, choose a short uppercase prefix from the project name.

## Create a Project

Use this when you know the project before creating stories.

```http
POST /api/projects
Content-Type: application/json

{
  "id": "txgarage",
  "name": "TXGarage",
  "prefix": "TXG"
}
```

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

Do not attempt to set `closed`; the API rejects it.

## Event History

Use this when you need to understand what happened to a story.

```http
GET /api/stories/TXG-001/events
```
