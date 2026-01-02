# ---------- Build stage ----------
# Pin exact Go version instead of "latest"
FROM golang:1.25.5-alpine3.23 AS builder

# Build static Linux binary
ENV CGO_ENABLED=0 GOOS=linux

WORKDIR /app

# Copy module files first (better Docker cache)
COPY go.mod go.sum ./
RUN go mod download

# Copy the rest of the source
COPY . .

# Build dprl binary
RUN go build -o /app/dprl ./cmd/dprl

# ---------- Runtime stage ----------
FROM alpine:3.23

RUN apk add --no-cache ca-certificates netcat-openbsd

RUN adduser -D -g '' appuser
USER appuser

WORKDIR /app
COPY --from=builder /app/dprl /app/

CMD ["/app/dprl"]
