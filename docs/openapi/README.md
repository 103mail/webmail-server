# OpenAPI

The OpenAPI definition of everything `api.Server.Handler` serves. It started
as a copy of `../webmail-legacy/web/openapi/v1/` and is now the spec of record;
the PHP copy is no longer kept in step.

| File | Covers |
|---|---|
| `v1/auth.yaml` | `/api/v1/auth/` bearer tokens |
| `v1/email.yaml` | `/api/v1/email/` mailboxes, folders, threads, messages, contacts |
| `v1/calendar.yaml` | `/api/v1/calendar/` calendars, meetings, documents |
| `v1/chat.yaml` | `/api/v1/chat/` chats, messages, presence and the WebSocket session |
| `v1/mail.yaml` | `/api/v1/mail/inbound`, the SES webhook (ADR 15) |
| `v1/federation.yaml` | The federation RFC endpoints (ADR 17) |
| `v1/internal.yaml` | The ws2api chat bridge (ADR 18) and `/health` |

The router dispatches on `/api/v1/{group}/{entity}/{id}`, and what the id means
depends on the method: `GET /mailbox/{id}` takes a customer, `PUT /mailbox/{id}`
a mailbox. Such paths are written once with `{id}`, and each operation says
which object it expects.

`internal/api/openapi_test.go` reads the routes from the handler source and
fails when a served method is missing here or a specified one is not served.
A new endpoint therefore lands together with its spec. The end-to-end controls
(`ControlHandler`, ADR 20) are test-only and deliberately left out.

To lint: `npx @redocly/cli lint docs/openapi/v1/*.yaml`.
