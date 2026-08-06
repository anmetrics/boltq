FROM golang:1.26-alpine AS builder

WORKDIR /app

# go.sum must travel with go.mod. Without it `go mod download` resolves modules
# with no checksum to verify them against, and the dependency layer is
# invalidated again the moment the source is copied in — losing both the
# supply-chain check and the build cache it was meant to create.
COPY go.mod go.sum ./
RUN go mod download && go mod verify

COPY . .

# Trimpath keeps local filesystem paths out of the binary; the static build is
# what allows the distroless base below.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /boltq-server ./cmd/server
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /boltq-cli ./cmd/cli

# Distroless rather than alpine: no shell, no package manager, and nothing for a
# process that escapes the application to pivot with. The image contains the two
# binaries and a CA bundle.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /boltq-server /usr/local/bin/boltq-server
COPY --from=builder /boltq-cli /usr/local/bin/boltq
COPY configs/default.json /etc/boltq/config.json

# 65532 is distroless's `nonroot` user, and the same UID the Kubernetes
# manifests set as fsGroup so the mounted volume is writable.
USER 65532:65532

#   9090 admin HTTP · 9091 queue TCP · 9100 raft · 9200 replication · 9300 gateway
EXPOSE 9090 9091 9100 9200 9300

ENTRYPOINT ["boltq-server"]
CMD ["-config", "/etc/boltq/config.json"]
