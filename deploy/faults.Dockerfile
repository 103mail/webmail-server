# The fault proxy (deploy/faults): passes federation traffic to each Domain's
# API, and misbehaves only when a story arms it. Standard library only, so the
# build needs no module downloads.
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY deploy/faults/ deploy/faults/
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/faults ./deploy/faults

FROM alpine:3.20
RUN adduser -D -u 1000 faults
COPY --from=build /out/faults /usr/local/bin/faults
USER faults
EXPOSE 8080 8026
HEALTHCHECK --interval=10s --timeout=3s --retries=5 \
  CMD wget -qO- http://127.0.0.1:8026/health || exit 1
ENTRYPOINT ["faults"]
