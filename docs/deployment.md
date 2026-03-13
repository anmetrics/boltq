# Deployment

## Binary Deployment

### Build

```bash
# Build both server and CLI
make build

# Output:
# bin/boltq-server
# bin/boltq
```

### Cross-compilation

```bash
# Linux AMD64
GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o bin/boltq-server-linux ./cmd/server

# Linux ARM64
GOOS=linux GOARCH=arm64 go build -ldflags="-s -w" -o bin/boltq-server-arm64 ./cmd/server

# macOS
GOOS=darwin GOARCH=arm64 go build -ldflags="-s -w" -o bin/boltq-server-darwin ./cmd/server

# Windows
GOOS=windows GOARCH=amd64 go build -ldflags="-s -w" -o bin/boltq-server.exe ./cmd/server
```

### Systemd Service

Create `/etc/systemd/system/boltq.service`:

```ini
[Unit]
Description=BoltQ Message Queue Server
After=network.target

[Service]
Type=simple
User=boltq
Group=boltq
ExecStart=/usr/local/bin/boltq-server -config /etc/boltq/config.json
Restart=always
RestartSec=5
LimitNOFILE=65535

# Environment overrides
Environment=BOLTQ_STORAGE_MODE=disk
Environment=BOLTQ_DATA_DIR=/var/lib/boltq/data

# Security hardening
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=/var/lib/boltq

[Install]
WantedBy=multi-user.target
```

```bash
# Install
sudo cp bin/boltq-server /usr/local/bin/
sudo mkdir -p /etc/boltq /var/lib/boltq/data
sudo cp configs/default.json /etc/boltq/config.json
sudo useradd -r -s /bin/false boltq
sudo chown -R boltq:boltq /var/lib/boltq

# Enable and start
sudo systemctl daemon-reload
sudo systemctl enable boltq
sudo systemctl start boltq

# Check status
sudo systemctl status boltq
sudo journalctl -u boltq -f
```

## Docker

### Build Image

```bash
docker build -t boltq:latest .
```

### Run

```bash
# Memory mode
docker run -d --name boltq \
  -p 9090:9090 \
  boltq:latest

# Disk mode with volume
docker run -d --name boltq \
  -p 9090:9090 \
  -e BOLTQ_STORAGE_MODE=disk \
  -v boltq-data:/var/lib/boltq/data \
  boltq:latest

# With API key
docker run -d --name boltq \
  -p 9090:9090 \
  -e BOLTQ_API_KEY=my-secret-key \
  boltq:latest
```

### Docker Compose

```yaml
version: '3.8'

services:
  boltq:
    build: .
    ports:
      - "9090:9090"
    environment:
      - BOLTQ_STORAGE_MODE=disk
      - BOLTQ_DATA_DIR=/data
      - BOLTQ_API_KEY=${BOLTQ_API_KEY:-}
    volumes:
      - boltq-data:/data
    restart: unless-stopped
    healthcheck:
      test: ["CMD", "wget", "-q", "--spider", "http://localhost:9090/health"]
      interval: 10s
      timeout: 5s
      retries: 3

  prometheus:
    image: prom/prometheus:latest
    ports:
      - "9091:9090"
    volumes:
      - ./prometheus.yml:/etc/prometheus/prometheus.yml
    depends_on:
      - boltq

volumes:
  boltq-data:
```

### Prometheus Config (prometheus.yml)

```yaml
global:
  scrape_interval: 15s

scrape_configs:
  - job_name: 'boltq'
    static_configs:
      - targets: ['boltq:9090']
```

## Kubernetes

### Deployment

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: boltq
  labels:
    app: boltq
spec:
  replicas: 1
  selector:
    matchLabels:
      app: boltq
  template:
    metadata:
      labels:
        app: boltq
      annotations:
        prometheus.io/scrape: "true"
        prometheus.io/port: "9090"
        prometheus.io/path: "/metrics"
    spec:
      containers:
        - name: boltq
          image: boltq:latest
          ports:
            - containerPort: 9090
              name: http
          env:
            - name: BOLTQ_STORAGE_MODE
              value: "disk"
            - name: BOLTQ_DATA_DIR
              value: "/data"
            - name: BOLTQ_API_KEY
              valueFrom:
                secretKeyRef:
                  name: boltq-secret
                  key: api-key
                  optional: true
          volumeMounts:
            - name: data
              mountPath: /data
          livenessProbe:
            httpGet:
              path: /health
              port: 9090
            initialDelaySeconds: 5
            periodSeconds: 10
          readinessProbe:
            httpGet:
              path: /health
              port: 9090
            initialDelaySeconds: 3
            periodSeconds: 5
          resources:
            requests:
              memory: "64Mi"
              cpu: "100m"
            limits:
              memory: "512Mi"
              cpu: "1000m"
      volumes:
        - name: data
          persistentVolumeClaim:
            claimName: boltq-data
---
apiVersion: v1
kind: Service
metadata:
  name: boltq
spec:
  selector:
    app: boltq
  ports:
    - port: 9090
      targetPort: 9090
      name: http
  type: ClusterIP
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: boltq-data
spec:
  accessModes:
    - ReadWriteOnce
  resources:
    requests:
      storage: 10Gi
---
apiVersion: v1
kind: Secret
metadata:
  name: boltq-secret
type: Opaque
stringData:
  api-key: "your-secret-api-key"
```

## Production Checklist

### Before Deploy

- [ ] Choose storage mode (`memory` for speed, `disk` for durability)
- [ ] Set API key for authentication
- [ ] Configure queue capacity for expected workload
- [ ] Set appropriate ACK timeout for consumer processing time
- [ ] Configure max retries based on failure tolerance

### Infrastructure

- [ ] Set up Prometheus scraping `/metrics`
- [ ] Configure health check for load balancer / orchestrator
- [ ] Set up alerting rules (dead letter rate, consumer lag)
- [ ] Mount persistent volume for disk mode
- [ ] Set `LimitNOFILE` / `ulimit` high enough for concurrent connections

### Monitoring

- [ ] Grafana dashboard with key metrics
- [ ] Alert on dead letter rate > threshold
- [ ] Alert on consumer lag growing
- [ ] Alert on server down
- [ ] Log aggregation (stdout → your log system)

### Security

- [ ] Enable API key authentication
- [ ] Use TLS termination at load balancer / reverse proxy
- [ ] Network policies to restrict access
- [ ] Run as non-root user

## Reverse Proxy (Nginx)

```nginx
upstream boltq {
    server 127.0.0.1:9090;
}

server {
    listen 443 ssl;
    server_name queue.example.com;

    ssl_certificate     /etc/ssl/certs/queue.crt;
    ssl_certificate_key /etc/ssl/private/queue.key;

    location / {
        proxy_pass http://boltq;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
    }

    # SSE support for /subscribe
    location /subscribe {
        proxy_pass http://boltq;
        proxy_set_header Connection '';
        proxy_http_version 1.1;
        chunked_transfer_encoding off;
        proxy_buffering off;
        proxy_cache off;
    }
}
```

## Performance Tuning

### OS Level

```bash
# Increase file descriptor limit
echo "boltq soft nofile 65535" >> /etc/security/limits.conf
echo "boltq hard nofile 65535" >> /etc/security/limits.conf

# TCP tuning
sysctl -w net.core.somaxconn=65535
sysctl -w net.ipv4.tcp_max_syn_backlog=65535
```

### Application Level

| Setting | Low Latency | High Throughput | Durability |
|---------|------------|-----------------|------------|
| `storage.mode` | memory | memory | disk |
| `queue.capacity` | 65536 | 4194304 | 1048576 |
| `queue.ack_timeout` | 5s | 30s | 60s |
| `queue.max_retry` | 3 | 5 | 10 |
