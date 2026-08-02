# syntax=docker/dockerfile:1

FROM golang:1.25-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG SERVICE
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/service ./cmd/${SERVICE}

# alpine provides wget for Docker healthchecks while staying small (~8 MB).
# The Go binary is statically linked (CGO_ENABLED=0) so it runs without glibc.
FROM alpine:3.20
RUN apk add --no-cache wget
COPY --from=builder /out/service /service
ENTRYPOINT ["/service"]
