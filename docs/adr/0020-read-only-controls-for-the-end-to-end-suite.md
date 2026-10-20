# 20. Read-only controls for the end-to-end suite

Status: proposed
Date: 2026-10-20

## Context

The stories in `../webmail-e2e` act only through the public API, and they may
not read a database to decide an outcome. Some outcomes cannot be seen through
the public API, though: that a Domain sent exactly one envelope to a peer, or
that it holds no account for someone who lives on another Domain. Those are
the outcomes that prove federation did not fall back to treating everyone as
local, so the stories need to see them.

## Decision

The API server can serve a small read-only control surface on a listener of
its own, set by `WEBMAIL_CONTROL_ADDR`:

- `GET /control/envelopes` lists the federation outbox: peer, recipients,
  status, attempts and times for each envelope, and never the message itself.
- `GET /control/accounts` lists each user with every address that reaches it.
- `GET /control/peers` lists each pinned peer descriptor: the keys it holds
  and the URL it was fetched from.
- `GET /control/files?md5=` lists how often the bytes with that MD5 are stored
  and how many attachments refer to them, never the bytes or the file names.
- `GET /control/threads?member=` lists the federated threads an address is on,
  with their federation id and members, never the subject or content.

It is off unless the variable is set, and it is never routed through the
public handler, so no Federation hostname can serve it. It has no
authentication: whoever can reach the listener is the operator. The local
stack sets it and publishes it on each Domain's own loopback address, port
8081.

## Consequences

- Stories can check what crossed a Domain boundary without reading SQL, and
  without the suite knowing the schema.
- A production deployment must not set `WEBMAIL_CONTROL_ADDR`, or must keep
  that port private. The server logs a warning when it is set.
- The surface only reads. Controls that change state, such as taking a Domain
  down or moving its clock, stay in the test stack, not in the server. The
  stack has two: a Domain's federation config can be swapped for a story
  (`FEDERATION_CONFIG_<DOMAIN>`), and federation traffic passes through a fault
  proxy that a story can arm (`deploy/faults`).
