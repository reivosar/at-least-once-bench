FROM golang:1.22-alpine AS builder

WORKDIR /app
COPY go.mod go.sum ./
COPY shared ./shared
COPY frameworks/nats/worker ./worker

RUN go mod download
RUN CGO_ENABLED=0 GOOS=linux go build -o nats-worker ./worker

FROM alpine:latest
RUN apk --no-cache add ca-certificates
WORKDIR /root/

COPY --from=builder /app/nats-worker .

EXPOSE 9095
CMD ["./nats-worker"]
