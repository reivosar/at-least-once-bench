FROM golang:1.22-alpine AS builder

WORKDIR /app
COPY go.mod go.sum ./
COPY shared ./shared
COPY frameworks/rabbitmq/worker ./worker

RUN go mod download
RUN CGO_ENABLED=0 GOOS=linux go build -o rabbitmq-worker ./worker

FROM alpine:latest
RUN apk --no-cache add ca-certificates
WORKDIR /root/

COPY --from=builder /app/rabbitmq-worker .

EXPOSE 9094
CMD ["./rabbitmq-worker"]
