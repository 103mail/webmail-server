# 23. Federating chat

Status: accepted
Date: 2026-10-25

## Context

Chat stayed on one Server: a chat with someone on another Domain gave them
nothing at all. RFC §14 now defines how a chat crosses Domains, on protocol
version `0.3`, and the M7 stories in `../webmail-e2e` prove it. Within a
Domain the rules are still those of ADR 9.

## Decision

- **Chat goes through the outbox, on a loop of its own.** A chat message, or a
  read receipt, is written to `federation_outbox` in the transaction that
  stores it, as mail is, with kind `chat` or `chat_read`. The worker drains
  those rows every second in a goroutine apart from the mail loop, so mail
  waiting on a slow peer never holds a chat up. Retries start after a second,
  back off to at most a minute, and stop after an hour
  (`WEBMAIL_CHAT_RETRY_BUDGET` shortens it on a test stack). After that the
  message is owed and backfilled exactly like mail (RFC §14.4).
- **Two versions.** A Server with the chat domain attached advertises
  `chat: true` and versions `0.2` and `0.3`. Mail is still sent as `0.2`; a
  chat envelope is sent as `0.3`, signed with that version, and only to a peer
  that lists it. A peer pinned before it took up chat is re-fetched once before
  its old descriptor is believed. An envelope of a kind we do not implement is
  answered `400 unsupported_kind`.
- **The federation package owns the wire, the chat package the rows.** As for
  mail (ADR 17), `internal/federation` declares a `ChatDeliverer` that
  `*chat.Service` satisfies, and `internal/chat` declares the `Federator` it
  needs, so neither imports the other's service.
- **Identifiers.** `g_chat` and `g_chat_message` get a `federation_id`. A
  message's is minted with its send time; a chat's when it is first needed. A
  direct chat that arrives under another id is matched by its two members and
  settles on the lower id. A chat's messages are listed in the order of RFC
  §5.5: send time in whole seconds, then federation id.
- **Membership travels as messages.** Adding someone to a chat with members
  elsewhere writes "X added Y", and leaving writes "Y left the chat" from the
  leaver, with them in `chat.left`. A chat that stays on this Server says
  nothing, as before. Only this Server's own people can be removed here. Every
  Domain that learns of an addition sends the newcomer's Domain its own users'
  earlier messages, marked `history`, which arrive read and do not bring back
  a deleted chat.
- **Receipts.** Members elsewhere have no `chat_message` row here, so their
  ticks are `message_status` rows against their address: `sent` when queued,
  `delivered` on the peer's 202, `read` when its `chat_read` arrives. Reading
  a chat sends a receipt to each Domain whose users wrote what was just read,
  up to the newest of their messages.
- **Bounces stay in the chat.** A message a peer refused, or that timed out,
  gets a local notice from `mailer-daemon` in the sender's own copy of the
  chat. Recipients that do not exist, or whose Domain does not do chat, are
  then no longer members.
- **No edits across Domains.** A message in a chat with members elsewhere
  cannot be edited or deleted (RFC §14.6).
- **Push.** A message from another Domain is pushed to the open WebSocket
  connections of its recipients, as one written here is (ADR 13).

## Consequences

- Existing databases need the two `federation_id` columns and their unique
  indexes. Messages from before have no id until they are sent as history.
- An address on a Domain without chat is only found out when its first
  envelope is refused, so the sender learns of it in the chat a second later
  rather than when adding them (RFC §14.1 asks for "at once").
- Receipts are not throttled: every read that reaches a new message from
  another Domain queues one (RFC §14.7 says at most one every few seconds).
- Chat envelopes count against the same inbound rate limit as mail, where RFC
  §14.8 suggests limiting them apart.
