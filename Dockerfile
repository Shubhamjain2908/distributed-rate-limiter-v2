# Build stage
FROM golang:1.22-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# Build a statically linked binary
RUN CGO_ENABLED=0 GOOS=linux go build -o /app/rate-limiter ./cmd/server/main.go

# Final stage
FROM alpine:latest
WORKDIR /root/
COPY --from=builder /app/rate-limiter .
EXPOSE 8080
CMD ["./rate-limiter"]