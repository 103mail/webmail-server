# Architecture decisions

One decision per file, numbered. A decision that changes is edited in place
rather than disputed by a later ADR. See ADR 1 for the format.

Decisions marked with a source were inherited from `../webmail` or
`../webmail-federation` and are recorded here so the reasoning survives the
rewrite.

| ADR | Title | Source |
| --- | --- | --- |
| [1](0001-record-architecture-decisions.md) | Record architecture decisions | |
| [2](0002-go-backend-supersedes-the-php-monolith.md) | Go backend supersedes the PHP monolith | webmail |
| [3](0003-canonical-objects-with-per-user-projections.md) | Canonical objects with per-user projections | 001-internal-messaging |
| [4](0004-notifications-only-and-no-forwarding.md) | Notifications only, and no forwarding | 001-internal-messaging |
| [5](0005-immutable-message-history.md) | Immutable message history | 001-internal-messaging |
| [6](0006-external-recipient-onboarding-via-tokens.md) | External recipient onboarding via single-use tokens | 001, 002, 003 |
| [7](0007-coalesced-notifications-with-quiet-hours.md) | Coalesced notifications with quiet hours | 001, 002, 003 |
| [8](0008-calendar-times-stored-in-utc.md) | Calendar times stored in UTC with a separate IANA zone | 002-calendaring |
| [9](0009-realtime-chat-over-websocket.md) | Real-time chat over WebSocket | 003-real-time-chat |
| [10](0010-append-only-audit-log.md) | Append-only audit log with YAML diffs | 004-audit-logging |
| [11](0011-federation-over-https.md) | Federation over HTTPS, not SMTP | webmail-federation |
| [12](0012-schema-in-dbml.md) | Schema kept in DBML | |
| [13](0013-websocket-fan-out-via-ws2api.md) | WebSocket fan-out via ws2api | ws2api |
| [14](0014-schema-baseline-and-calendar-reimplementation.md) | Schema baseline, and calendar reimplemented from the spec | webmail |
| [15](0015-email-transport-over-amazon-ses.md) | Email transport: SNS out, SES in | webmail |
| [16](0016-wiring-email-in-and-out.md) | Wiring email in and out | |
| [17](0017-implementing-webmail-federation.md) | Implementing Webmail-Federation | webmail-federation |
| [18](0018-the-ws2api-chat-bridge.md) | The ws2api chat bridge | ws2api |
| [19](0019-postgresql-in-production-sqlite-under-test.md) | PostgreSQL in production, SQLite under test | |
| [20](0020-read-only-controls-for-the-end-to-end-suite.md) | Read-only controls for the end-to-end suite | webmail-e2e |
