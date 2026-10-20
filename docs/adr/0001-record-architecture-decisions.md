# 1. Record architecture decisions

Status: accepted
Date: 2026-08-24

## Context

The webmail platform is being split from a PHP monolith into a Go backend, a
TypeScript SPA and a federation protocol. Most of the design has already been
settled in `../webmail`, where it lives in feature specifications and a project
constitution written for Spec Kit. That format records what the system must do,
but not why a given shape was chosen, and it is tied to tooling this repo does
not use.

Rewriting those documents as specifications again would duplicate them. Dropping
them would lose the constraints that make the system coherent, in particular the
no-forwarding rule and the canonical/projection split.

## Decision

Record architecture decisions as numbered ADRs in `docs/adr/`, one decision per
file, using the format of this document: context, decision, consequences.

ADRs derived from the PHP repo carry a `Source:` line naming the document they
came from, so the provenance of an inherited constraint stays visible.

ADRs describe the architecture as it stands. When a decision changes, the ADR
that holds it is edited and the superseded text removed, so no ADR disputes an
earlier one. Git history keeps what was there before.

## Consequences

- The reasoning behind inherited constraints survives the rewrite, and a reader
  can tell which rules are inherited and which are new to this repo.
- `CLAUDE.md` stays short. It states the rules; the ADRs explain them.
- Specifications for new features still need somewhere to live. ADRs record
  decisions, not acceptance criteria.
