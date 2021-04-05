FROM golang:1.15.11-alpine3.13 AS builder

WORKDIR /app
COPY . /app

RUN go build

FROM alpine:3.13

COPY --from=builder /app/proxy-informer /usr/bin/proxy-informer
ENTRYPOINT ["proxy-informer"]
