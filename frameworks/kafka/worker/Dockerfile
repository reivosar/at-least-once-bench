FROM golang:1.22-alpine AS builder

WORKDIR /app
COPY go.mod go.sum ./
COPY shared ./shared
COPY frameworks/kafka/worker ./worker

RUN go mod download
RUN CGO_ENABLED=0 GOOS=linux go build -o kafka-worker ./worker

FROM alpine:latest
RUN apk --no-cache add ca-certificates
WORKDIR /root/

COPY --from=builder /app/kafka-worker .

EXPOSE 9092
CMD ["./kafka-worker"]
