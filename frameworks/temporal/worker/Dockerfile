FROM golang:1.22-alpine AS builder

WORKDIR /app
COPY go.mod go.sum ./
COPY shared ./shared
COPY frameworks/temporal/worker ./worker

RUN go mod download
RUN CGO_ENABLED=0 GOOS=linux go build -o temporal-worker ./worker

FROM alpine:latest
RUN apk --no-cache add ca-certificates
WORKDIR /root/

COPY --from=builder /app/temporal-worker .

EXPOSE 9091 7234
CMD ["./temporal-worker"]
