FROM golang:1.15.5-alpine3.12 AS builder

WORKDIR /app
COPY . /app

RUN go build

FROM alpine:3.12

COPY --from=builder /app/proxy-informer /usr/bin/proxy-informer
ENTRYPOINT ["proxy-informer"]
