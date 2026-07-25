---
name: steady-api
description: Use this skill when writing code, scripts, or automations that interact with the Steady API (runsteady.com, service.steady.space) — including making HTTP requests against `https://service.steady.space/api/v2`, generating clients from the Steady OpenAPI spec, authenticating with personal access tokens, or fetching/updating Steady data (teams, people, check-ins, activities, goals, echoes, comments, reactions, digest). Also use when answering questions about Steady's API endpoints, authentication, rate limits, schemas, or any `steady_pat_` token. Trigger any time the user mentions the "Steady API", references `service.steady.space`, pastes a `steady_pat_` token, or wants to build something on top of Steady data — even if they don't explicitly say "API".
---

# Steady API

This skill covers everything Claude needs to write correct, well-behaved code against Steady's v2 REST API. It assumes the user wants real code (curl, scripts, clients, integrations) — not marketing or product-usage answers.

## Source of truth: the OpenAPI spec

Steady publishes a complete, machine-readable OpenAPI 3.1 description of every endpoint, schema, and example. It is updated automatically whenever the API changes — your training data is **not** the source of truth here, the spec is.

- **Spec URL**: `https://app.steady.space/openapi.yml`
- **Human-readable reference**: `https://runsteady.com/docs/api/category/endpoints/`

**When to fetch the spec live**: any time the task is non-trivial — looking up exact field names, generating a client, writing typed models, building anything that mentions multiple endpoints, or answering a question where you're not 100% sure of the shape. Don't guess from memory; fetch and read. For one-off curl commands against a well-known endpoint (`/me`, `/teams`, `/people`), you can skip the fetch.

**For client generation**: point tooling (OpenAPI Generator, openapi-typescript, Speakeasy, etc.) directly at the URL — don't download and commit a snapshot, as it will drift.

## Base URL and authentication

- **Base URL**: `https://service.steady.space/api/v2`
- **Auth**: Bearer token in the `Authorization` header
- **Token format**: personal access tokens are prefixed with `steady_pat_`
- **Where users get tokens**: `https://app.steady.space/my/integrations/edit`
- **Scopes**: tokens are either `read` (can be set to never expire) or `read+write` (must expire within 365 days)

Every request looks like:

```
Authorization: Bearer steady_pat_xxxxxxxxxxxx
```

**Quick smoke test** for any auth setup — `GET /me` returns the authenticated person:

```bash
curl -s https://service.steady.space/api/v2/me \
  -H "Authorization: Bearer $STEADY_TOKEN"
```

If that returns a person object, auth works. If it returns `401`, the token is wrong, expired, or missing the `Bearer` prefix.

**Never** hardcode tokens in code you write for the user. Pull from an environment variable, a config file the user provides, or prompt them to set one. If the user pastes a token directly into chat, use it for the immediate task but suggest they rotate it afterward and store it in an env var.

## Rate limits — handle them, don't ignore them

Steady enforces **two** limits per user, simultaneously:

- **30 requests per 10 seconds** (burst protection)
- **500 requests per 30 minutes** (sustained usage cap)

Exceeding **either** limit returns `429 Too Many Requests` with a `Retry-After` header (in seconds).

### Response headers worth knowing

Steady returns IETF-format rate-limit headers on every response:

- `RateLimit-Policy` — the policies in effect, e.g. `"burst";q=30;w=10, "sustained";q=500;w=1800`
- `RateLimit` — current state per policy, e.g. `"burst";r=27;t=8, "sustained";r=482;t=1620`
  - `r` = remaining, `t` = seconds until reset

### What this means for code Claude writes

For one-off scripts (a few requests), you can ignore rate limits — you won't hit them.

For anything that loops, paginates a lot, or runs unattended, the code should:

1. **Honor `Retry-After` on 429** — sleep for that many seconds, then retry.
2. **Don't burst blindly** — if you're about to fire 100 requests in a tight loop, pace them. A simple `time.sleep(0.4)` between requests keeps you safely under 30/10s.
3. **Watch the headers when debugging** — if a script is mysteriously slow or failing, log `RateLimit` to see which budget is exhausted. Bursty workloads usually starve the 30/10s policy first.

A reasonable Python pattern:

```python
import time, requests

def steady_get(path, **params):
    url = f"https://service.steady.space/api/v2{path}"
    headers = {"Authorization": f"Bearer {TOKEN}"}
    for attempt in range(5):
        r = requests.get(url, headers=headers, params=params)
        if r.status_code == 429:
            time.sleep(int(r.headers.get("Retry-After", 5)))
            continue
        r.raise_for_status()
        return r.json()
    raise RuntimeError("rate-limited after 5 retries")
```

Adapt the pattern to the language/HTTP client the user is using. The shape is what matters: retry on 429, respect `Retry-After`, cap retries.

## Check-ins: PATCH-only, no POST

Check-ins don't have a POST/CREATE endpoint. Steady pre-generates check-ins automatically based on each team's check-in schedule combined with the assigned person's absences, so "filling out a check-in" means updating an existing record, not creating a new one. **This is unique to check-ins** — most other resources (goals, goal updates, comments, reactions, echo answers) follow normal REST CRUD with POST to create and PATCH to update. Don't generalize the no-POST behavior; see "Post a goal update" in the patterns section below for the standard create flow.

### Two ways to address a check-in

The `PATCH /check-ins/{key}` endpoint accepts either a UUID or a date in the path:

- **`PATCH /check-ins/{date}`** — updates the authenticated user's check-in for that date (e.g. `PATCH /check-ins/2026-05-22`). This is the simple path for "submit my check-in" — no need to look up an ID first.
- **`PATCH /check-ins/{uuid}`** — updates a specific check-in record by ID. Use this when you already have the UUID from a list response, or when you're updating someone else's check-in (with appropriate token scope).

The request body is the same for both: `previous`, `intentions`, `blockers`, `mood`, `previous_completed`, and an optional `team_ids` array (omit to default to all the person's teams).

### When no check-in exists for the date

`PATCH /check-ins/{date}` returns **404** if the authenticated person doesn't have a pre-generated check-in for that date — typically because their team isn't scheduled to check in that day, or they're marked absent. Surface that to the user; check-ins can't be created on demand to work around it.

### Other things to know

- **`mood`** is a fixed enum (`focused`, `happy`, `tired`, `confident`, `stressed`, etc.). Check the spec for the full list before passing arbitrary strings.
- **`team_ids`** in the request body is optional and only affects which teams the check-in is reported to — it doesn't create new check-ins for other teams.

## Endpoint categories

The full, current list of endpoint categories lives at `https://runsteady.com/docs/api/category/endpoints/`. Fetch that page when you need to see what's available — don't try to enumerate categories from memory, since new ones get added over time. Each category has its own reference page underneath it with the request/response shapes.

For exact field names, parameters, and schemas, the OpenAPI spec (`https://app.steady.space/openapi.yml`) is the authoritative source.

## Conventions across the API

- **IDs**: all resource IDs are UUIDs (string).
- **Dates**: `date` fields are ISO date (`YYYY-MM-DD`); `*_at` fields are ISO 8601 datetimes (`2026-05-04T18:18:22Z`).
- **Markdown fields**: long-form text fields like `previous`, `intentions`, `blockers`, `body` (on comments) are Markdown.
- **Pagination**: list endpoints accept `page` (1-indexed) and `per_page`.
- **Array query parameters**: use the `name[]=value` form (e.g. `people_ids[]=abc&people_ids[]=def`), not comma-separated.
- **Filtering by date range**: `since` and `until` parameters on most list endpoints, both inclusive.
- **Mentions in Markdown bodies**: `@username` syntax — Steady will link these to people.

## Patterns for common tasks

When the user asks for one of these, here's the rough shape. Always confirm details against the spec.

### Fill out my check-in

One call: `PATCH /check-ins/2026-05-22` (substitute the relevant date) with the answer fields — `previous`, `intentions`, `blockers`, `mood`, `previous_completed`, optional `team_ids`. The date in the path is resolved to the authenticated user's check-in for that day. Returns 404 if no check-in exists for the date (off-schedule day or marked absent).

### Post a goal update

A standard CREATE — the API's usual CRUD shape, in contrast to check-ins which only support PATCH. One call: `POST /goals/{goal_id}/goal-updates` with `title`, `body`, `progress`, and `confidence` in the JSON body.

Two quirks worth knowing:

- **Nested on create/list, flat on access.** Create and list via `POST/GET /goals/{goal_id}/goal-updates`; access an individual update at the flat `GET/PATCH/DELETE /goal-updates/{id}`.
- **`confidence` snaps to 30, 60, or 90** on save (off track / at risk / on track). Both `progress` and `confidence` default to the previous update's values, so omit them when you just want to update the prose.

### Pull all check-ins from last week

`GET /check-ins?since=YYYY-MM-DD&until=YYYY-MM-DD`, paginate with `page` + `per_page`.

### Find who's blocked this week

`GET /check-ins?since=...&until=...&blocked=true`.

### Pull activity for my team

`GET /activities?team_ids[]={team_id}&kinds[]=github_pr&kinds[]=jira`. Look up the team's UUID via `GET /teams` or from `/me`'s `teams[]` array.

### Get my person ID

`GET /me` returns the authenticated person; grab `.id` when you need to filter other endpoints by your own UUID (e.g. `people_ids[]={my_id}` on `/check-ins` or `/activities`). The API doesn't accept a literal `"me"` or `"self"` shorthand in filters. Cache the UUID for the duration of a script — it doesn't change.

## Where to point users for docs

When the user asks "where's the documentation for X," prefer these direct links:

- Getting Started: `https://runsteady.com/docs/api/category/getting-started/`
- Authentication: `https://runsteady.com/docs/api/authentication/`
- Rate limits: `https://runsteady.com/docs/api/rate-limits/`
- OpenAPI page: `https://runsteady.com/docs/api/openapi-specification/`
- Raw spec: `https://app.steady.space/openapi.yml`
- All endpoint references: `https://runsteady.com/docs/api/category/endpoints/`

## A note on the MCP server

Steady also publishes an MCP server (described at `https://runsteady.com/mcp`) that exposes the same data through tool calls. If the user wants Claude itself to read/write Steady data — as opposed to writing a script that does — the MCP server is usually the better path. This skill is for the "write code that calls the API" use case.
