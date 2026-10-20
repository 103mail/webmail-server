# The mail sink (deploy/mailsink): a stand-in for SNS that captures the
# notification email every Domain publishes. Standard library only, so the
# build needs no module downloads.
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY deploy/mailsink/ deploy/mailsink/
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/mailsink ./deploy/mailsink

FROM alpine:3.20
RUN adduser -D -u 1000 mailsink
COPY --from=build /out/mailsink /usr/local/bin/mailsink
USER mailsink
EXPOSE 4566
HEALTHCHECK --interval=10s --timeout=3s --retries=5 \
  CMD wget -qO- http://127.0.0.1:4566/health || exit 1
ENTRYPOINT ["mailsink"]
