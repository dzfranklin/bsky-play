# syntax=docker/dockerfile:1

FROM golang:1.22-bookworm AS build
WORKDIR /build

RUN mkdir -p /out

COPY go.* ./
RUN go mod download

COPY . ./
RUN go build -o /out/bsky-play .

FROM debian:bookworm
WORKDIR /app

COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /out/bsky-play /app/

ENV TZ=UTC
ENTRYPOINT ["/app/bsky-play", "-data", "/data"]
