FROM golang:1.26-alpine AS builder

WORKDIR /app
COPY go.mod ./
RUN go mod download
COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /boltq-server ./cmd/server
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /boltq-cli ./cmd/cli

FROM alpine:3.19

RUN apk --no-cache add ca-certificates
COPY --from=builder /boltq-server /usr/local/bin/boltq-server
COPY --from=builder /boltq-cli /usr/local/bin/boltq
COPY configs/default.json /etc/boltq/config.json

RUN mkdir -p /var/lib/boltq/data

EXPOSE 9090 9091

ENTRYPOINT ["boltq-server"]
CMD ["-config", "/etc/boltq/config.json"]
