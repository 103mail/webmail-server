# The WebSocket proxy from ../ws2api (ADR 13). It terminates chat connections
# and turns them into plain HTTP against the API server, which is what keeps the
# API stateless.
#
# The proxy hardcodes both its listen port and the API URL it calls in main:
#
#   http.ListenAndServe(":7001", getWsHandler("http://localhost:8000/wsoverhttp/"))
#
# `localhost` inside this container is this container, so as written it cannot
# reach the API at all. Both literals are patched here from build arguments.
# This is a deployment hack standing in for two flags the proxy should take, and
# it is the first thing to delete if that repo grows them. See ADR 18.
FROM golang:1.22-alpine AS build
ARG WS_LISTEN=":7001"
ARG WS_API_URL="http://api:8080/wsoverhttp/"
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN sed -i \
      -e "s|\"http://localhost:8000/wsoverhttp/\"|\"${WS_API_URL}\"|" \
      -e "s|ListenAndServe(\":7001\"|ListenAndServe(\"${WS_LISTEN}\"|" \
      wsproxy/wsproxy.go \
 && grep -q "${WS_API_URL}" wsproxy/wsproxy.go \
 && CGO_ENABLED=0 go build -trimpath -o /out/wsproxy ./wsproxy

FROM alpine:3.20
RUN adduser -D -u 1000 ws2api
COPY --from=build /out/wsproxy /usr/local/bin/wsproxy
USER ws2api
EXPOSE 7001
ENTRYPOINT ["wsproxy"]
