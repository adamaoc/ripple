# TheTaskManager

TheTaskManager is a local, self-hosted task manager built around a simple kanban board and a bot-friendly JSON API.

The goal is to keep a long-running task service available on your machine so humans can review work visually while chat bots and coding agents can create, update, and move stories without needing a fresh explanation every time.

## Current Direction

This project is intentionally small and local-first:

- One Go application
- SQLite for storage
- Server-rendered HTML templates
- HTMX for lightweight UI interactions
- Plain CSS
- JSON API for bots and agents
- Bot-discoverable documentation served by the app

We chose this over a separate Go API plus SolidJS frontend because the app does not need complex client-side state, routing, or a frontend build pipeline. The priority is easy startup, easy self-hosting, and a durable local workflow.

## Product Decisions

### Stories

A story is the main work item.

Stories have:

- Human-friendly ID
- Title
- Markdown description
- Required project
- Optional epic
- Status
- Event history
- Optional close comment

Story IDs are generated per project using that project's prefix:

```txt
TXG-001
TXG-002
RV-001
```

Projects can define their own prefix. If a bot creates a story with a new `projectName` but no `projectPrefix`, the API chooses a short uppercase prefix from the project name.

### Projects

Every story must belong to a project.

Projects are created through the API, not through the UI. The UI only filters by project.

### Epics

Stories may belong to an epic, but it is not required.

Epics are created through the API, not through the UI. The UI only filters by epic.

### Statuses

Stories support five statuses:

```txt
backlog
queued
in_progress
done
closed
```

The intended flow is:

```txt
backlog -> queued -> in_progress -> done -> closed
```

This flow is documented, but not rigidly locked between the bot-writable states. Bots can move a story directly between `backlog`, `in_progress`, and `done` when needed.

Bots cannot move stories to `queued` or `closed`. Queued is currently a human-facing planning status, and closing is a manual human review action.

In practice:

- Bots should create stories in `backlog` unless there is a reason to do otherwise.
- Bots should move active work to `in_progress`.
- Bots should move completed work to `done`.
- Humans close stories from the UI after review.

### Closed Stories

Closed stories are treated as archived.

They are hidden by default on the board and in the default API story list. They can be shown with a UI toggle or API query.

The UI supports:

- Closing a single story
- Adding a close comment
- Closing all stories currently in the Done column

Closing all done stories respects the current project and epic filters.

### Deletion

There is no story deletion in v1.

Closed stories handle the archive case and preserve history.

### Change History

Story events are logged for important changes:

- Story creation
- Status changes
- Detail updates
- Epic changes
- Close comments

This is especially useful because bots and agents may make changes over time.

## Running The App

From the project directory:

```bash
go run .
```

Then open:

```txt
http://localhost:8080
```

By default, the app creates and uses:

```txt
taskmanager.db
```

You can choose a different address or database path:

```bash
go run . -addr :8090 -db ~/taskmanager/taskmanager.db
```

You can also build a binary:

```bash
go build -o taskmanager
./taskmanager
```

## Environment Variables

The app also reads these optional environment variables:

```txt
TASKMANAGER_ADDR
TASKMANAGER_DB
```

Example:

```bash
TASKMANAGER_ADDR=:8090 TASKMANAGER_DB=~/taskmanager/taskmanager.db go run .
```

The app reads environment variables as defaults. Explicit command-line flags take precedence.

## UI

The main UI is served at:

```txt
GET /
```

The board shows:

- Backlog
- In Progress
- Done
- Closed, only when enabled

From the UI you can:

- Filter by project
- Filter by epic
- Show or hide closed stories
- Move stories between bot-writable statuses
- Open a card detail panel
- Edit a story description
- Close one story manually
- Close all done stories

Project and epic management intentionally happen through the API only.

## Bot API Discovery

Bots should start with:

```http
GET /api
```

That response links to:

```http
GET /api/docs
GET /api/openapi.yaml
```

The Markdown bot guide is also stored in:

```txt
docs/bot-api.md
```

The OpenAPI schema is stored in:

```txt
docs/openapi.yaml
```

The point is that a future chat bot can be pointed at the running service and discover how to create and manage tasks without you re-explaining the rules.

## Common API Calls

### Create A Project

```bash
curl -X POST http://localhost:8080/api/projects \
  -H 'Content-Type: application/json' \
  -d '{
    "id": "txgarage",
    "name": "TXGarage",
    "prefix": "TXG"
  }'
```

### Create A Story

With an existing project:

```bash
curl -X POST http://localhost:8080/api/stories \
  -H 'Content-Type: application/json' \
  -d '{
    "projectId": "txgarage",
    "title": "Add saved vehicle filter",
    "description": "Add a **saved vehicles** filter.",
    "status": "backlog"
  }'
```

Letting the API create or reuse project and epic:

```bash
curl -X POST http://localhost:8080/api/stories \
  -H 'Content-Type: application/json' \
  -d '{
    "projectName": "Real View",
    "projectPrefix": "RV",
    "epicName": "Listing workflow",
    "title": "Show listing preview before publish",
    "description": "Render a **preview** before the listing goes live."
  }'
```

### List Stories

```bash
curl http://localhost:8080/api/stories
```

Filter by project:

```bash
curl 'http://localhost:8080/api/stories?projectId=txgarage'
```

Filter by epic:

```bash
curl 'http://localhost:8080/api/stories?epicId=txgarage-mobile-polish'
```

Filter by status:

```bash
curl 'http://localhost:8080/api/stories?status=in_progress'
```

Include closed stories:

```bash
curl 'http://localhost:8080/api/stories?showClosed=1'
```

### Move A Story

```bash
curl -X PATCH http://localhost:8080/api/stories/TXG-001/status \
  -H 'Content-Type: application/json' \
  -d '{
    "status": "done"
  }'
```

Allowed bot statuses:

```txt
backlog
in_progress
done
```

The API rejects:

```txt
closed
```

### Update A Story

```bash
curl -X PATCH http://localhost:8080/api/stories/TXG-001 \
  -H 'Content-Type: application/json' \
  -d '{
    "description": "Updated Markdown description."
  }'
```

### View Event History

```bash
curl http://localhost:8080/api/stories/TXG-001/events
```

## Source Layout

```txt
main.go                 Go server, handlers, services, migrations
templates/layout.html   Page shell
templates/board.html    Kanban board partial
templates/story_panel.html
static/styles.css       UI styles
docs/bot-api.md         Human/agent-readable bot guide
docs/openapi.yaml       Machine-readable API schema
```

This is deliberately compact for now. If the app grows, the likely next refactor would be splitting `main.go` into focused packages for storage, services, API handlers, and UI handlers.

## Verification Commands

Build:

```bash
go build ./...
```

Format:

```bash
gofmt -w main.go
```

Basic API smoke test:

```bash
go run . -addr :8090 -db /tmp/thetaskmanager-smoke.db
```

Then in another shell:

```bash
curl http://localhost:8090/api
```

## Current Non-Goals

- No login or authentication
- No external database
- No cloud hosting assumptions
- No separate frontend build system
- No project or epic editing in the UI
- No story deletion
- No strict status-transition lock between bot-writable states

## Likely Next Improvements

- Add a small install/run script or launch agent for always-on local use
- Add tests around status rules and ID generation
- Add API endpoint examples to OpenAPI responses
- Add search by text/title
- Add due dates or priority only if they become useful
- Add drag-and-drop once the board behavior is otherwise settled
