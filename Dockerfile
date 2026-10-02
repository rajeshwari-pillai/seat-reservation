FROM golang:1.25-alpine AS builder

WORKDIR /app

# Copy go mod files first for layer caching
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Build the main binary
RUN CGO_ENABLED=0 GOOS=linux go build -o /server .

# Build the burst test binary
RUN CGO_ENABLED=0 GOOS=linux go build -o /burst ./cmd/burst

# Runtime image
FROM alpine:3.19

RUN apk --no-cache add ca-certificates

WORKDIR /app

COPY --from=builder /server /app/server
COPY --from=builder /burst /app/burst
COPY init.sql /app/init.sql

EXPOSE 8080

CMD ["/app/server"]
