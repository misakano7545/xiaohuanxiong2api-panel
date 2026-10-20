# syntax=docker/dockerfile:1
FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY . .
ARG VERSION=0.1.0
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/xhx2api ./cmd/server

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata && adduser -D -u 10001 app
WORKDIR /app
COPY --from=build /out/xhx2api /app/xhx2api
RUN mkdir -p /app/auths && chown -R app:app /app
USER app
ENV CONFIG=/app/config.json
EXPOSE 7867
ENTRYPOINT ["/app/xhx2api"]
