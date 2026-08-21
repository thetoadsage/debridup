# Dashboard Redesign and Runtime Hardening

Date: 2026-08-21

## Objective

Make the dashboard answer two questions within a few seconds:

1. Are all configured providers healthy now?
2. What changed recently enough to require attention?

The redesign keeps the current dark visual language and lightweight Go plus vanilla JavaScript architecture. It adds a consolidated dashboard API, clearer operational metrics, safer secret handling, bounded history, stronger scheduling behavior, and automated release checks.

## Scope

This change includes:

- A responsive dashboard combining an operations overview with provider-level analysis.
- A consolidated, range-aware dashboard API.
- Server-side time-series bucketing and latency percentile calculations.
- Provider pulse intervals for healthy, degraded, and outage periods.
- An accessible provider-detail drawer.
- Database migrations, connection-safe SQLite configuration, history retention, and scheduler overlap protection.
- Deployment guidance that keeps the encryption key outside writable application data.
- Continuous integration and release-safety checks.

Existing monitor-management, incident, notification, login, and reset behavior remains available. Existing API routes remain supported during this change.

## User Experience

### Information hierarchy

The default dashboard uses a full labeled sidebar with Dashboard, Providers, Incidents, and Settings destinations. The content area is ordered by urgency:

1. Overall status, providers online, active incidents, and checks completed today.
2. A provider pulse timeline showing state changes over the selected range.
3. A provider table with current status, uptime, p50 latency, p95 latency, last check, and a compact status strip.
4. A multi-provider latency comparison chart.
5. Recent incidents.

The existing near-black and navy surfaces, blue controls, mint success accents, thin borders, compact radii, and straightforward system typography remain the visual foundation. Warning and outage colors always appear with text or icons so meaning does not depend on color alone.

### Time ranges

The dashboard supports `24h`, `7d`, and `30d`. The selected range controls the pulse timeline, uptime, latency percentiles, chart series, and incident list. Changing the range performs one dashboard request and updates all affected sections together.

### Provider details

Selecting a provider row or card opens a right-side drawer without navigating away. The drawer shows:

- Current state and state duration.
- Rolling availability.
- p50 and p95 latency.
- Last successful check.
- Slowest check in the selected range.
- Latest event and recent incident history.

The drawer traps focus while open, closes with Escape, restores focus to its trigger, and uses a modal sheet on narrow viewports.

### Loading and failure states

The dashboard has explicit states for:

- Initial loading with stable skeleton geometry.
- No providers configured, with a direct setup action.
- No data in the selected range.
- A stale response, showing the age of the most recent successful refresh.
- A total request failure, with retry guidance.
- Partial provider data, identifying unavailable providers while preserving valid results.

Automatic refresh pauses while the browser tab is hidden and resumes with an immediate refresh. A refresh already in progress is not duplicated.

## Backend Design

### Consolidated endpoint

Add an authenticated endpoint:

```text
GET /api/dashboard?range=24h
```

Accepted ranges are `24h`, `7d`, and `30d`. Invalid values return `400` with a stable error code. The response contains:

```json
{
  "generated_at": "2026-08-21T14:32:18Z",
  "range": "24h",
  "summary": {},
  "providers": [],
  "series": [],
  "incidents": []
}
```

Each provider entry contains its current state, summary metrics, pulse buckets, latest check, and data-quality state. Chart series use a server-selected bucket width to keep payloads bounded:

| Range | Bucket width | Maximum points per series |
| --- | ---: | ---: |
| 24 hours | 15 minutes | 96 |
| 7 days | 2 hours | 84 |
| 30 days | 8 hours | 90 |

The old overview, monitor, check, and incident routes remain unchanged for compatibility and management views.

### Metrics

- Availability is successful checks divided by completed checks in the selected range.
- p50 and p95 are calculated from completed latency samples using a documented nearest-rank method.
- A pulse bucket is healthy when every completed check succeeds, degraded when at least one succeeds and one fails, and outage when all completed checks fail.
- A bucket with no completed checks is unknown and is not counted as downtime.
- Checks completed today use UTC day boundaries, matching stored timestamps.

### Internal boundaries

The current application file is split only where required by the feature:

- `dashboard` owns range parsing, aggregation, percentiles, bucketing, and response models.
- `store` owns migrations, dashboard queries, retention, and SQLite connection configuration.
- `scheduler` owns due-check selection and in-flight protection.
- HTTP handlers validate requests, invoke these modules, and encode responses.

Provider-specific request behavior remains separate from dashboard aggregation. This keeps presentation queries from affecting monitoring execution.

## Storage and Scheduling

### SQLite configuration

Connection-wide requirements such as foreign-key enforcement and busy timeout are applied through the connection configuration rather than a one-time statement on an arbitrary pooled connection. Migrations are versioned and execute transactionally.

### Retention

Raw checks are retained for 90 days by default. A configurable positive duration can override this value. Pruning runs at most once per UTC day in a short transaction and records errors without stopping monitoring.

Server-side bucketing operates on retained raw checks in this change. Persistent rollup tables are deferred until measured database size or query latency justifies them.

### Scheduler protection

Only one check per monitor may be active at a time. If a monitor becomes due while its prior check is still running, the scheduler records a skipped-overlap event in process metrics and waits for the next interval. A bounded global worker limit prevents a large provider list from creating unbounded concurrent requests.

## Secret Handling and Deployment

The encryption key remains file-based. A literal key is never embedded in Compose, an image, application configuration checked into source control, or a command example.

Deployment documentation must keep the key in a secret-only host directory that is not nested under the writable data mount. The container receives that individual file read-only at `/run/secrets/encryption_key`; writable application data remains mounted at `/data`.

The container hardening baseline includes:

- A non-root application process.
- Read-only secret mount.
- All Linux capabilities dropped.
- `no-new-privileges` enabled.
- A health check that distinguishes process health from database readiness.

## Frontend Implementation

The frontend stays dependency-free and uses the existing embedded static assets. The JavaScript is divided into small modules for API access, dashboard state, chart rendering, provider drawer behavior, and formatting. CSS custom properties centralize the existing color, spacing, radius, and type tokens.

The chart implementation must provide visible axes, units, legends, and non-canvas summaries for assistive technology. It must not display more points than returned by the server. Provider and incident lists use semantic tables or lists rather than clickable generic containers.

No live production data or credentials are used in fixtures, screenshots, tests, documentation, or design assets. Test fixtures use generic provider names and deterministic timestamps.

## Error Handling and Observability

API errors use a consistent JSON shape with a stable code and safe user-facing message. Internal errors are logged with request context but never include authorization headers, provider tokens, encryption material, or decrypted notification configuration.

Dashboard responses include `generated_at`. The client displays stale-data status when refreshes fail after a prior successful response. Monitoring continues independently of dashboard request failures.

The readiness check verifies that the database can be queried. The existing lightweight health check remains suitable for liveness.

## Release Safety

Continuous integration runs:

- Go formatting verification.
- Unit and integration tests.
- Static analysis.
- Race-enabled tests where supported.
- Dependency vulnerability analysis.
- Container build verification.
- Release-safety scanning.

Release-safety scanning covers tracked files, staged changes, commit metadata, proposed change text, and committed image metadata. It rejects likely credentials, private network addresses, user-home paths, ordinary personal email addresses, and prohibited tool or vendor attribution. Additional private patterns are supplied through local configuration or encrypted repository configuration; they are never added to tracked files.

The final pre-publication check repeats the scan against the complete branch diff, commit messages, proposed title and description, and generated assets. A failure blocks publication.

## Testing Strategy

Backend tests cover:

- Range validation and bucket selection.
- Availability and nearest-rank percentile calculations.
- Healthy, degraded, outage, and unknown pulse classification.
- UTC day boundaries.
- Consolidated response authorization and shape.
- Migration upgrades from the current schema.
- Retention pruning.
- Per-monitor overlap prevention and global concurrency bounds.
- Readiness behavior when the database is unavailable.

Frontend verification covers:

- Initial, loading, empty, healthy, degraded, outage, stale, partial, and failed states.
- Range changes and automatic refresh deduplication.
- Provider drawer mouse, keyboard, focus, and narrow-screen behavior.
- Accessible names, focus visibility, contrast, table structure, chart summaries, and reduced-motion behavior.
- Layout checks at desktop, tablet, and mobile widths.

Existing authentication, monitor management, notifications, and incident behavior receive regression coverage proportional to the touched code.

## Delivery Sequence

1. Add tests for aggregation, range parsing, scheduling, and storage behavior.
2. Add migrations, connection configuration, retention, and scheduler protection.
3. Add the consolidated endpoint while preserving existing APIs.
4. Build the redesigned dashboard against deterministic fixtures.
5. Connect the dashboard to the endpoint and add failure states.
6. Harden deployment guidance and container defaults.
7. Add continuous integration and release-safety checks.
8. Run functional, accessibility, responsive, security, and privacy verification.

## Acceptance Criteria

- The default dashboard communicates current health and recent change without navigation.
- One authenticated request supplies all range-dependent dashboard data.
- The dashboard supports `24h`, `7d`, and `30d` with bounded payload sizes.
- p50, p95, uptime, pulse intervals, and incident information are labeled and consistent.
- Provider details are accessible without leaving the dashboard.
- Slow provider requests do not overlap for the same monitor or create unbounded concurrency.
- Raw history is pruned according to configuration.
- The encryption key is outside writable application data and mounted read-only.
- Existing management and authentication behavior remains functional.
- Automated tests and release checks pass.
- The branch, commit metadata, proposed change description, and assets contain no private personal information, credentials, private addresses, local paths, or prohibited attribution.
