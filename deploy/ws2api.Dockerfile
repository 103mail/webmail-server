# The WebSocket proxy from ../ws2api (ADR 13). It terminates chat connections
# and turns them into plain HTTP against the API server, which is what keeps the
# API stateless.
#
# This is behind the `chat-ws` compose profile and off by default, because the
# API server does not implement the bridge yet: ADR 13 requires GET, POST and
# DELETE on /<ClientId>, and internal/api routes none of them. Until it does,
# the proxy would answer every upgrade with a 403. The client polls instead.
FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/wsproxy ./wsproxy

FROM alpine:3.20
RUN adduser -D -u 1000 ws2api
COPY --from=build /out/wsproxy /usr/local/bin/wsproxy
USER ws2api
EXPOSE 8000
ENTRYPOINT ["wsproxy"]
