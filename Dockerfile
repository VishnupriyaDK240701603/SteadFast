FROM golang:1.24-alpine AS builder

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG TARGET=api
RUN CGO_ENABLED=0 GOOS=linux go build -o /app/bin/service ./cmd/\

FROM alpine:3.21
WORKDIR /app
RUN apk --no-cache add ca-certificates curl
COPY --from=builder /app/bin/service /app/service
ENTRYPOINT [" /app/service\]
