# syntax=docker/dockerfile:1

FROM golang:1.23-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG SERVICE
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/service ./cmd/${SERVICE}

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=builder /out/service /service
ENTRYPOINT ["/service"]
