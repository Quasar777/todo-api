FROM golang:1.24-alpine AS builder
WORKDIR /app

RUN apk add --no-cache git

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ENV CGO_ENABLED=0 GOOS=linux GOARCH=amd64
RUN go build -trimpath -ldflags="-s -w" -o todo-api ./


FROM alpine:3.20
WORKDIR /app

RUN apk add --no-cache ca-certificates && adduser -D -g '' appuser
COPY --from=builder /app/todo-api /app/todo-api

USER appuser

EXPOSE 8080

CMD ["/app/todo-api"]