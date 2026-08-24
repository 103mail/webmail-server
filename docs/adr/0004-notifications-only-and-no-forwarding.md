# 4. Notifications only, and no forwarding

Status: accepted
Date: 2026-08-24
Source: `../webmail/specs/001-internal-messaging/spec.md` (FR-002, FR-006,
FR-006a), constitution principle I

## Context

The product is mail-like but is not email. Its value is that a message stays
inside the system: the sender knows who can read it, and that set only grows by
explicit subscription. Two mechanisms would break that guarantee. Emailing the
message body puts content on infrastructure the system does not control, and
forwarding lets any recipient hand the whole history to someone the sender never
chose.

Removing forwarding without a replacement is not workable in practice. People
legitimately need to pass on part of a conversation.

## Decision

Message content is never sent by email. Outbound email carries metadata and a
secure link only. `sent_email` rows record what was sent and must never contain a
`g_message` body; templates in `email_template` are constrained to metadata.

There is no forward action, in the UI or in the API. Visibility grows only by
adding a subscription, which is an explicit, audited act.

Quoting is the replacement. Quoting copies the selected text into a new
`g_thread` with its own subscription list. There is no schema link back to the
source thread, no hyperlink to it, and attachments are not copied. Recipients of
a quote cannot reach the original.

## Consequences

- The subscriber list of a thread is the complete answer to "who can read this",
  at any time, without inspecting anything else.
- Search indexing, digests and audit records all have to be written on the
  assumption that they may leak content. Each one is restricted to metadata.
- A quoted conversation is genuinely disconnected. Replies to the quote do not
  reach the original participants, and there is no way to reunite the threads
  later. This is intended.
- Any new export, integration or webhook must be checked against this rule before
  it ships. The constraint holds only as long as no feature quietly emits content.
