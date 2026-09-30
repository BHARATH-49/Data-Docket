# Data Docket

Data Docket answers a narrow question before a CSV dataset reaches model training: **can this version proceed, and if not, why?** A project defines required columns, an ID column, a target label, and two comparison thresholds. Users validate a baseline, approve it after a Pass report, then check candidate CSVs against that approved version.

## Workflow

1. Create a project in the web interface at `http://localhost:8080/projects` or with `POST /api/projects`.
2. Upload a baseline CSV. The upload returns a job ID immediately after the file is saved; a separate worker validates it.
3. Open the job's report. If it passes, approve that baseline.
4. Upload a candidate CSV. A worker validates it and compares it with the baseline approved when the upload was queued.
5. Review the Pass or Block decision, validation findings, comparison evidence, and project check history.

```mermaid
flowchart LR
  Browser[Web UI or API] --> API[Go HTTP API]
  API --> PG[(PostgreSQL: projects, jobs, reports)]
  API --> Files[(Shared CSV volume)]
  PG --> W1[Go worker 1]
  PG --> W2[Go worker 2]
  Files --> W1
  Files --> W2
  W1 --> PG
  W2 --> PG
```

PostgreSQL stores project rules, upload metadata, the approved baseline pointer, job state, and reports. The shared volume stores uploaded CSVs under generated names. Workers claim jobs with PostgreSQL row locks and `SKIP LOCKED`; a claim has a token and a 30-second lease renewed during processing. Expired jobs can be reclaimed. Reports are unique per job, and only the current claim can complete a job. Transient processing failures retry at most three times; invalid CSV input completes with a Block report instead of a failed job.

## Start with Docker Compose

Install Docker Desktop with Compose. Copy `.env.example` to `.env` and replace the sample password. On PowerShell:

```powershell
Copy-Item .env.example .env
notepad .env
docker compose up --build -d
docker compose ps
```

Open `http://localhost:8080/projects`. Compose starts PostgreSQL, the API, and two separate workers. It stores PostgreSQL data and uploaded CSVs in named volumes. The database port is bound to `127.0.0.1:5432` for local development. Stop services with `docker compose down`; this preserves the volumes.

## Run Go processes locally

Install Go 1.23 or newer and Docker Desktop. Start only PostgreSQL with `docker compose up -d db`. Set the same password as `.env`, then start the API and two workers in three separate PowerShell windows:

```powershell
$env:DATABASE_URL = 'postgres://datadocket:YOUR_PASSWORD@localhost:5432/datadocket?sslmode=disable'
$env:UPLOAD_DIR = (Join-Path (Get-Location) 'data/uploads')
go run ./cmd/datadocket api
```

```powershell
$env:DATABASE_URL = 'postgres://datadocket:YOUR_PASSWORD@localhost:5432/datadocket?sslmode=disable'
$env:UPLOAD_DIR = (Join-Path (Get-Location) 'data/uploads')
go run ./cmd/datadocket worker
```

Run the worker command again in a third window. All processes must use the same upload directory. The API applies the embedded initial SQL migration at startup; the SQL is also available in [`migrations/001_init.sql`](migrations/001_init.sql) for review.

Environment variables:

| Name | Purpose | Default |
| --- | --- | --- |
| `DATABASE_URL` | PostgreSQL connection string | Required |
| `UPLOAD_DIR` | Shared directory for CSVs | `data/uploads` |
| `HTTP_ADDR` | API listen address | `:8080` |
| `MAX_UPLOAD_BYTES` | Maximum CSV file size | `52428800` (50 MiB) |

## Project rules and decision

The [sample project](examples/project.json) demonstrates the JSON shape. Required column types are `string`, `integer`, `number`, `boolean`, `date` (`YYYY-MM-DD`), and `datetime` (RFC 3339). Empty cells in ordinary required columns are allowed; nonempty cells must match their configured type. An empty ID is never allowed. Missing target labels are counted and compared with the baseline, rather than failing validation by themselves.

Validation blocks an empty CSV, malformed header or row, empty or duplicate header name, missing required column, empty or duplicate ID, or nonempty value with the wrong type. Reports identify columns and show up to five example rows per finding. Row numbers refer to CSV source lines.

A valid candidate receives three comparison checks:

| Check | Block condition |
| --- | --- |
| Row count drop | `(baseline rows - candidate rows) / baseline rows × 100` exceeds `max_row_drop_percent` (growth is not a drop). |
| Missing target increase | Candidate missing-target rate minus baseline rate exceeds `max_missing_target_increase_percent` **percentage points**. |
| New target labels | Candidate contains any nonempty, trimmed target label absent from the baseline. |

Threshold equality passes. A candidate passes only if validation and every comparison check passes. Invalid candidates block and skip comparison. Baseline approval requires a completed Pass report.

## API

All responses are JSON. Uploads use multipart form field `file`; report routes take a **job ID**. Job states are `queued`, `running`, `completed`, and `failed`; report decisions are `pass` and `block`.

| Method | Path | Purpose |
| --- | --- | --- |
| `POST` | `/api/projects` | Create project from JSON rules and `name`. |
| `GET` | `/api/projects` | List projects. |
| `GET` | `/api/projects/{id}` | Get project and current approved baseline ID. |
| `POST` | `/api/projects/{id}/baseline` | Upload baseline; returns `202` and `job_id`. |
| `POST` | `/api/projects/{id}/candidates` | Upload candidate; requires an approved baseline. |
| `POST` | `/api/projects/{id}/baselines/{fileID}/approve` | Approve a passing baseline. |
| `GET` | `/api/projects/{id}/jobs` | Check history, newest first. |
| `GET` | `/api/jobs/{id}` | Job status, attempts, and processing error if any. |
| `GET` | `/api/reports/{jobID}` | Validation, comparison, and decision. |
| `GET` | `/healthz` | Database health. |

For a short API walkthrough in PowerShell:

```powershell
$project = Invoke-RestMethod -Method Post -Uri http://localhost:8080/api/projects -ContentType application/json -InFile examples/project.json
$baseline = curl.exe -sS -F "file=@examples/baseline.csv" "http://localhost:8080/api/projects/$($project.id)/baseline" | ConvertFrom-Json
Invoke-RestMethod "http://localhost:8080/api/jobs/$($baseline.job_id)"
Invoke-RestMethod "http://localhost:8080/api/reports/$($baseline.job_id)"
```

Wait until the baseline job is `completed`, then approve it using the `file_id` in the job response. Upload [candidate-pass.csv](examples/candidate-pass.csv) and [candidate-block.csv](examples/candidate-block.csv) to `/api/projects/{id}/candidates`. The passing candidate matches the approved label set and thresholds. The blocking candidate has a large row-count drop, more missing labels, and a new `maybe` label.

## Tests and limits

Run `go test ./...` for CSV rules and comparisons. Set `TEST_DATABASE_URL` to a disposable PostgreSQL database URL to also run concurrent claiming, recovery, approval, and HTTP upload-to-report tests. Tests that require PostgreSQL explicitly skip when this variable is absent.

Verification completed for this version: the Go application built with `docker compose up -d --build`; `go test -count=1 -p 1 ./...` passed against a separate PostgreSQL test database; and the running Compose stack produced Pass for the sample baseline and passing candidate, and Block for the blocking candidate. The Block report identified the row drop, missing-target increase, and new `maybe` label. The project and Block report pages were also checked in a browser.

This small version has no user authentication, project editing, scheduled file cleanup, or distributed object storage. It checks only the defined CSV rules and three baseline changes; it does not assess model quality or broader data drift. CSV records are streamed, while ID and target-label sets are retained in memory to detect duplicates and new labels. The shared file volume assumes workers run on one Docker host.
