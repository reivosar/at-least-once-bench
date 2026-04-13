FROM golang:1.22-alpine AS builder

WORKDIR /app
COPY go.mod go.sum ./
COPY shared/downstream ./
COPY shared/proto ../proto

RUN go mod download
RUN CGO_ENABLED=0 GOOS=linux go build -o downstream .

FROM alpine:latest
RUN apk --no-cache add ca-certificates
WORKDIR /root/

COPY --from=builder /app/downstream .

EXPOSE 8080 8081
CMD ["./downstream"]
