# 13. WebSocket fan-out via ws2api

Status: accepted
Date: 2026-08-24
Source: `../ws2api` (github.com/mevdschee/ws2api)

## Context

Holding chat's WebSocket connections (ADR 9) in the API process makes horizontal
scaling hard: a node owns the connections it accepted, a deploy drops all of
them, and a group message has to reach participants whose connections were
accepted by other nodes. That would make the API tier the one stateful part of
an otherwise interchangeable set of workers, for the feature with the tightest
latency budget.

`../ws2api` already solves this and is benchmarked for it: 250k concurrent
connections holding 25k messages per second in 32GB RAM on one proxy, described
in https://tqdev.com/2024-scaling-to-1-million-websockets.

## Decision

Terminate WebSockets in ws2api rather than in the API server. The API server
stays stateless and speaks plain HTTP in both directions.

The proxy converts the connection lifecycle into HTTP against the API server:

- Upgrade becomes `GET /wsoverhttp/<ClientId>`. The API server must answer with
  exactly `ok`; anything else refuses the connection with 403. This is where the
  connection is authenticated.
- A client message becomes `POST /wsoverhttp/<ClientId>` with the message as the
  body, and the response body is written back to that same socket. An empty
  response sends nothing.
- A close becomes `DELETE /wsoverhttp/<ClientId>` with the close reason as the
  body. This is what drives `presence_status` to `offline`.

A server-initiated push is `POST /<ClientId>` to the ws2api tier, which answers
`ok`, or 404 when that client is no longer connected. Replies to a push arrive as
ordinary client-to-server messages, so they must carry their own correlation in
the payload.

ClientId is an opaque per-connection session identifier, never a user id, mailbox
id or bearer token. It appears in URL paths and in proxy logs.

The proxy tier scales behind HAProxy with `balance uri` and `depth 1`, or nginx
with `hash $request_uri consistent`, so a given ClientId always lands on the same
proxy node. The API server pushes through that same load balancer and therefore
never needs to know which node holds a connection.

Messages are text frames only. Binary frames are dropped by the proxy.

## Consequences

- The API tier is stateless and horizontally scalable, and deploying it does not
  drop WebSocket connections.
- Group fan-out becomes N HTTP posts, one per participant ClientId. That is the
  price of statelessness, and it makes fan-out naturally parallel, with no peer
  able to block another, as federation does in ADR 11.
- The API server still assigns the monotonic message ids of ADR 9; the proxy
  only forwards.
- A 404 on push is how the API server learns a client vanished without a clean
  close. It reconciles presence rather than being retried.
- Typing indicators and presence travel the same HTTP path as chat messages, so
  their 1 second and 5 second targets set the request rate against the API
  server.
- ws2api has no server-initiated close, so the API server cannot force-disconnect
  a client. Since connection auth happens once at the opening `GET`, a long-lived
  connection outlives the token that opened it. Revocation has to be enforced per
  message on the API side, or ws2api needs a close endpoint added upstream.
- `GET /` on the proxy serves plaintext counters with no authentication. It must
  not be publicly reachable.
- Deployment gains a component and a load balancer requirement. HAProxy
  terminates WSS to WS with the Origin header and Keep-Alive disabled. The tuning
  from the ws2api README (`nf_conntrack_max`, `fs.file-max`, `MaxConnsPerHost`)
  is required or throughput will cap far below the benchmark.
