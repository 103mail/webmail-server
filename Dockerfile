# Build the API server. CGO is off because both database drivers, pgx for
# PostgreSQL and modernc.org/sqlite, are pure Go (ADR 19), which keeps the
# runtime image to a static binary on a distroless-sized base.
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ cmd/
COPY internal/ internal/
# The schema is embedded, so the server can create its own database.
COPY schema/ schema/
# Both binaries share the build: the worker is the same code with a different
# entry point, and shipping one image keeps them from drifting apart.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/ ./cmd/...

FROM alpine:3.20
# tzdata is not optional: times are stored in UTC with an IANA identifier
# alongside for display (ADR 8), and the calendar rejects a meeting whose zone
# it cannot load. Alpine ships no zoneinfo, so without this every zone but UTC
# is a 400.
RUN apk add --no-cache ca-certificates curl tzdata \
 && adduser -D -u 1000 webmail
COPY --from=build /out/ /usr/local/bin/
# Attachment and document bytes, and in development the SQLite database.
# Mounted as a volume in compose so they survive a rebuild, and so the PHP
# backend can share the attachment directory when both run.
RUN mkdir -p /data && chown webmail:webmail /data
USER webmail
ENV WEBMAIL_ADDR=:8080 WEBMAIL_DATA_DIR=/data
EXPOSE 8080
HEALTHCHECK --interval=10s --timeout=3s --start-period=20s --retries=5 \
  CMD curl -fsS http://127.0.0.1:8080/health || exit 1
ENTRYPOINT ["webmail-server"]
