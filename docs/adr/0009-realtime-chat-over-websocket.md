# 9. Real-time chat over WebSocket

Status: accepted
Date: 2026-08-24
Source: `../webmail/specs/003-real-time-chat/spec.md`

## Context

Chat has latency requirements that mail does not: message delivery within 2
seconds, typing indicators within 1 second, presence within 5 seconds, and a
target of 10,000 concurrent connections. Polling cannot meet the typing indicator
target at any acceptable request rate, and the PHP request model has no place to
hold a connection open. This is one of the main reasons for the move to Go
(ADR 2).

Chat also needs ordering guarantees mail does not. Group participants must see
messages in the same order, and a client that reconnects must receive what it
missed without duplicates.

## Decision

Chat runs over WebSocket connections (terminated as in ADR 13). Ordering is
established by server-assigned monotonic message ids, not client timestamps.

Delivery state is tracked per recipient in `message_status` with forward-only
transitions `sent` to `delivered` to `read`. Group senders see the aggregate.

Presence lives in `presence_status` (`online`, `away` after 10 minutes idle,
`offline` after disconnect or 15 minutes idle). Typing indicators live in
`typing_indicator` with a 3 second TTL extended per keystroke and cleared on send.

Messages for offline participants are queued server-side and delivered on
reconnect. Outbound retries use exponential backoff with deduplication, so a
retried send never produces a second message.

Membership is `g_chat_participant` with `joined_at` and `left_at`. A new
participant sees all prior history. Someone who left keeps read-only access to
messages sent before `left_at` and receives nothing after it.

## Consequences

- Typing indicators and presence are high-frequency and low-value. They must not
  be written to the audit log or they will dominate it.
- The 2 second delivery target applies to the WebSocket path only. Federated
  chat, if it happens, inherits the RFC's asynchronous model instead and cannot
  meet it.
- Server-side sanitisation of message HTML is required, since chat renders
  formatted content from other users.
