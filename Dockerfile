# syntax=docker/dockerfile:1

FROM golang:1.25-alpine AS builder
WORKDIR /app
COPY . .
RUN go build -o http-router main.go

FROM scratch
COPY --from=builder /app/http-router /http-router
EXPOSE 8080
ENTRYPOINT ["/http-router"]