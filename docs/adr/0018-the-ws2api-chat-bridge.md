# 18. The ws2api chat bridge

Status: accepted
Date: 2026-08-25
Source: `../ws2api`. Implements the decision recorded in ADR 13.

## Context

ADR 13 terminates chat's WebSockets in `../ws2api` and fixes the HTTP calls the
proxy makes. It does not say what travels over the socket, how a connection is
authenticated, or how a message reaches the other participants.

## Decision

Implement the bridge over ws2api. The client does not poll.

### ClientId is a session, not an identity

Connecting is two steps. The client posts to `/api/v1/chat/session` with its
bearer token and gets back a ClientId; then it opens
`wss://webmail.{domain}/ws/{clientId}`. The proxy's upgrade check resolves that
ClientId to the session row, and the connection inherits the identity the
session was minted with.

A session is single-use for connecting, short-lived until claimed, and belongs
to exactly one user. A ClientId leaking from a log is worth little: it is
useless once connected, and expires unclaimed.

`/wsoverhttp/` is **not** exposed publicly. It is authenticated only by knowing
a ClientId, which is safe for the proxy on a private network and unsafe on the
internet. nginx proxies `/api/` and `/ws/` and nothing else.

### The frame format

Text frames carrying JSON objects with a `type`.

Client to server, each optionally carrying an `id` the acknowledgement echoes:

```
{"type":"hello"}                                   -> ready
{"type":"message","chatId":"6","body":"hi"}        -> ack, then a push to the others
{"type":"typing","chatId":"6","on":true}           -> push to the others
{"type":"read","chatId":"6"}                       -> ack
{"type":"presence","status":"away"}                -> push to everyone who shares a chat
{"type":"ping"}                                    -> pong
```

Server to client:

```
{"type":"ready","userId":1,"email":"..."}
{"type":"message","chatId":"6","message":{...}}
{"type":"typing","chatId":"6","userId":2,"on":true}
{"type":"presence","userId":2,"status":"online"}
{"type":"ack","id":"c1","messageId":"12"}
{"type":"error","id":"c1","error":"..."}
```

The message payload is the same shape `GET /api/v1/chat/message/{chatId}`
returns, so the client has one representation of a chat message. A refusal is
an `error` frame, not an HTTP status.

The response to a client frame goes back on that client's own socket, so it is
a reply, and the fan-out to the other participants is a separate push.

### Fan-out

A message becomes one insert and then one push per open session of the other
participants. The sender's originating session is skipped, because it already
received the acknowledgement. A push that answers 404 marks the session closed,
so the next fan-out does not try it again.

### Presence and typing keep their timeouts

The proxy's `DELETE` on close sets presence offline immediately, but a proxy
that is killed takes its connections with it and no close arrives. So presence
stays a `last_activity_at` with a TTL in `presence_status`, and typing keeps its
`expires_at`. The DELETE makes the common case immediate; the TTL is the
mechanism.

## Consequences

- There is one more table, `chat_session`, and one more setting:
  `WEBMAIL_WS_PUSH_URL`. Unset, a message reaches the database and the sender
  but not the other participants until they reload. The server logs a warning at
  startup rather than failing, because a deployment without chat is legitimate.
- The client needs a reconnect policy. It backs off and re-mints a session,
  because a ClientId is not reusable across connections.
- ws2api takes its listen port and API URL as flags with environment fallbacks,
  and `deploy/` configures it rather than patching it.
- Server-Sent Events were rejected: one-directional, so sending would still be a
  POST, with the same connection count problem and a second transport for one
  feature.
