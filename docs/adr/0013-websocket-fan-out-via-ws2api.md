# 13. WebSocket fan-out via ws2api

Status: accepted
Date: 2026-08-24
Source: `../ws2api` (github.com/mevdschee/ws2api)

Resolves the open question left in ADR 9.

## Context

ADR 9 put chat on WebSockets and left horizontal scaling undecided. Holding
connections in the API process is what makes that hard: a node owns the
connections it accepted, a deploy drops all of them, and a group message has to
reach participants whose connections were accepted by other nodes. That would
make the API tier the one stateful part of an otherwise interchangeable set of
workers, and it would do so for the feature with the tightest latency budget.

`../ws2api` already solves this and is benchmarked for it: 250k concurrent
connections holding 25k messages per second in 32GB RAM on one proxy, described
in https://tqdev.com/2024-scaling-to-1-million-websockets.

## Decision

Terminate WebSockets in ws2api rather than in the API server. The API server
stays stateless and speaks plain HTTP in both directions.

The proxy converts the connection lifecycle into HTTP against the API server:

- Upgrade becomes `GET /<ClientId>`. The API server must answer with exactly
  `ok`; anything else refuses the connection with 403. This is where the
  connection is authenticated.
- A client message becomes `POST /<ClientId>` with the message as the body, and
  the response body is written back to that same socket.
- A close becomes `DELETE /<ClientId>` with the close reason as the body. This is
  what drives `presence_status` to `offline`.

A server-initiated push is `POST /<ClientId>` to the ws2api tier, which answers
`ok`, or 404 when that client is no longer connected. Replies to a push arrive as
ordinary client-to-server messages, so they are not correlated to the push by the
transport and must carry their own correlation in the payload.

ClientId is an opaque per-connection session identifier, never a user id, mailbox
id or bearer token. It appears in URL paths and in proxy logs.

The proxy tier scales behind HAProxy with `balance uri` and `depth 1`, or nginx
with `hash $request_uri consistent`, so a given ClientId always lands on the same
proxy node. The API server pushes through that same load balancer and therefore
never needs to know which node holds a connection.

Messages are text frames only. Binary frames are dropped by the proxy, so the
wire format for chat is text.

## Consequences

- The API tier is stateless and horizontally scalable again, and deploying it
  does not drop WebSocket connections.
- Group fan-out becomes N HTTP posts, one per participant ClientId, each routed
  independently. That is more requests than an in-process broadcast and it is the
  price of statelessness. It also makes fan-out naturally parallel, with no peer
  able to block another, which is the same rule federation follows in ADR 11.
- Message ordering from ADR 9 still holds. The API server assigns the monotonic
  ids and remains the single serialisation point; the proxy only forwards.
- A 404 on push is how the API server learns a client vanished without a clean
  close. It is a normal outcome, not an error, and it must reconcile presence
  rather than retry.
- Typing indicators and presence travel the same HTTP path as chat messages, so
  their 1 second and 5 second targets set the request rate against the API
  server. They stay out of the audit log (ADR 10).
- ws2api has no server-initiated close: its HTTP surface handles a push and an
  upgrade, and nothing else. The API server therefore cannot force-disconnect a
  client. Since connection auth happens once at the opening `GET`, a long-lived
  connection outlives the token that opened it. Revocation has to be enforced per
  message on the API side, or ws2api needs a close endpoint added upstream.
- `GET /` on the proxy serves plaintext counters (connections opened and closed,
  requests started, failed and succeeded) with no authentication. Useful for
  monitoring, and it must not be publicly reachable.
- Deployment gains a component and a load balancer requirement. HAProxy is needed
  regardless to terminate WSS to WS with the Origin header and Keep-Alive
  disabled. The tuning from the ws2api README (`nf_conntrack_max`, `fs.file-max`,
  `MaxConnsPerHost`) is required or throughput will cap far below the benchmark.
