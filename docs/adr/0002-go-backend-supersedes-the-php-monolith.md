# 2. Go backend supersedes the PHP monolith

Status: accepted
Date: 2026-08-24
Source: `../webmail` (MintyPHP application), `../webmail-client`

## Context

`../webmail` is a MintyPHP application that serves both the UI and the API. Pages
under `pages/` render `.phtml` templates, and the JSON API is a thin layer over
`lib/Public/{AuthApi,EmailApi,CalendarApi,ChatApi}.php` reached through
`pages/api/v1/`. The same process therefore owns session-based page rendering and
the API, and the two share PHP request lifecycles and static configuration.

Three things the platform now needs are awkward in that shape. Real-time chat
wants long-lived connections, which the PHP request model does not provide.
Federation wants background workers with retry budgets that outlive a request.
Multi-domain deployment wants one client bundle served against many backends,
which conflicts with server-rendered pages carrying per-domain branding.

`../webmail-client` already exists as a React SPA covering mail, calendar, chat
and auth, running on mock fixtures in `src/data/`.

## Decision

Split the monolith. This repo becomes the Go backend and serves only a JSON API
plus federation endpoints. `../webmail-client` becomes the only frontend.

The PHP repo stays as the behavioural reference and the source of the production
schema. It does not receive new features.

The Go API keeps the resource shape of `lib/Public/`: mailbox, folder, thread,
subscriber, message, attachment, contact for mail; calendar, meeting, attendee,
document for calendar; chat, participant, message, attachment, presence, typing
for chat. Endpoint paths stay under `/api/v1/` so the client can be pointed at
either backend during migration.

## Consequences

- Chat can use WebSocket connections and federation can use worker goroutines
  without fighting the request model.
- Session-cookie auth backed by server-rendered login pages has to be replaced by
  bearer-token auth the SPA can carry.
- Two backends exist during migration. Keeping the resource shape stable is what
  makes running them side by side practical, so deviating from it needs a reason.
