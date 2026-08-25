# 18. The ws2api chat bridge

Status: accepted
Date: 2026-08-25
Source: `../ws2api`. Implements the decision recorded in ADR 13, which resolved
the scaling question left open by ADR 9.

## Context

ADR 9 put chat on WebSockets. ADR 13 decided the connections terminate in
`../ws2api` rather than in the API process, so the API tier stays stateless and
a deploy does not drop every open socket. Neither says what travels over the
socket, how a connection is authenticated, or how a message reaches the other
participants. None of it was implemented: `internal/api` routed nothing the
proxy calls, so the proxy would have refused every upgrade, and the client polled
every five seconds instead.

Polling is the wrong shape for chat twice over. It is slow where latency is most
visible, and it is most expensive when nothing is happening, because an idle
conversation costs exactly as many queries as a busy one.

## Decision

Implement the bridge ADR 13 describes, and delete the polling.

### What the proxy already fixes

`../ws2api` converts a connection's whole lifecycle into ordinary HTTP against
the API server, one request per event:

| WebSocket event | Becomes | The API must answer |
| --- | --- | --- |
| upgrade | `GET /wsoverhttp/{ClientId}` | exactly `ok`, or the upgrade is refused with 403 |
| client message | `POST /wsoverhttp/{ClientId}` with the frame as the body | a frame, written back to that same socket |
| close | `DELETE /wsoverhttp/{ClientId}` with the close reason as the body | anything |

A server-initiated push is the mirror image: `POST {proxy}/{ClientId}` with the
frame as the body, answered `ok`, or `404` when that client is no longer
connected. That 404 is useful rather than an error: it is how the API learns a
session is gone without the proxy having to tell it.

The API therefore holds no connections and needs no shared memory between
instances. Everything it knows about who is connected is a row.

### ClientId is a session, not an identity

ADR 13 requires an opaque per-connection identifier that is never a user id, a
mailbox id or a bearer token, because it appears in URL paths and proxy logs.

So connecting is two steps. The client posts to `/api/v1/chat/session` with its
bearer token and gets back a ClientId; then it opens
`wss://webmail.{domain}/ws/{clientId}`. The proxy's upgrade check resolves that
ClientId to the session row, and the connection inherits the identity the
session was minted with.

A session is single-use for connecting, short-lived until claimed, and belongs
to exactly one user. A ClientId leaking from a log is worth little: it is
useless once connected, and expires unclaimed.

`/wsoverhttp/` is **not** exposed publicly. It is authenticated only by knowing
a ClientId, which is exactly the property that makes it safe for the proxy on a
private network and unsafe on the internet. nginx proxies `/api/` and `/ws/`
and nothing else.

### The frame format

Text frames carrying JSON objects with a `type`. Binary frames are dropped by
the proxy, so text is the only option.

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
returns, so the client has one representation of a chat message rather than two.

One consequence of the transport is worth stating: a response to a client frame
goes back on that client's own socket, so it is a reply and not a broadcast, and
the fan-out to the other participants is a separate push. A handler with nothing
to say answers with an empty body, which the proxy turns into no frame at all.

### Fan-out is N posts, and that is the price

A message to a chat with four participants becomes one insert and then one HTTP
POST per open session of the other three. There is no broadcast primitive
because there is no shared connection state to broadcast over. ADR 13 accepted
this: it is what statelessness costs, and it makes fan-out naturally parallel
with no participant able to block another.

The sender's own originating session is skipped, because it already received
the acknowledgement as the response to its own frame.

A push that answers 404 means the session is gone. The row is marked closed
there and then, so the next fan-out does not try it again.

### Presence and typing keep their timeouts

The proxy does call `DELETE` on close, so a clean disconnect sets presence
offline immediately. It cannot be relied on alone: a proxy that is killed takes
its connections with it and no close ever arrives.

So presence stays a `last_activity_at` with a TTL, which is what
`presence_status` already holds, and typing keeps its `expires_at`. The DELETE
is an optimisation that makes the common case immediate, not the mechanism.

## Consequences

- Chat is live. `useChats` and `useChatMessages` stop polling; the socket
  updates the state the components already read.
- The API tier stays stateless and horizontally scalable, and a deploy does not
  drop connections, which was the point of ADR 13.
- There is one more table, `chat_session`, and one more thing that can be
  misconfigured: `WEBMAIL_WS_PUSH_URL`. Unset, everything still works except
  fan-out, so a message reaches the database and the sender but not the other
  participants until they reload. The server logs a warning at startup rather
  than failing, because a deployment without chat is legitimate.
- The client needs a reconnect policy. It backs off and re-mints a session,
  because a ClientId is not reusable across connections.

### What building this changed in ws2api

The proxy hardcoded its listen port and the API URL in `main`, and `localhost`
inside a container is the container, so it could not reach the API at all. It
now takes both as flags with environment fallbacks, and `deploy/` configures it
rather than patching it.

Three defects surfaced while building against it, all now fixed upstream with
tests that fail without the fix:

- **A duplicate ClientId unregistered the live connection.** A second upgrade
  claiming a connected id stored over the first; when the first closed, it
  deleted the map entry now pointing at the second, and that socket stopped
  receiving pushes with nothing in the logs. A duplicate is refused now.
- **An empty API response was written as an empty frame.** The protocol says an
  empty response sends nothing. This client was filtering empty frames out to
  compensate; it no longer has to.
- **A failed API call had its body forwarded to the client.** A 500 put
  `internal server error` on the socket looking like an ordinary chat frame.

That last one is why this bridge answers a refusal with an `error` frame rather
than an HTTP status: anything non-200 was, at the time, reaching the browser as
though the server had said it.

### Alternatives rejected

**Terminating WebSockets in the API.** What ADR 13 rejected, for the reasons it
gives: it makes the API tier stateful, a deploy drops every socket, and group
fan-out has to reach connections other nodes accepted.

**Server-Sent Events.** One-directional, so sending would still be a POST, and
the connection count problem is identical. It would avoid the proxy at the cost
of a second transport for the same feature.

**Sharing a network namespace with the proxy** (`network_mode: service:api`) so
that the proxy's hardcoded `localhost:8000` resolves to the API. It works and
needs no patching, but it couples the two containers into one network identity
and moves the API off its port to satisfy a constant in someone else's `main`.
Patching the constant is the more honest of the two.
