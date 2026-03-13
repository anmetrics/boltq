.PHONY: build server cli test bench clean docker run
.PHONY: cluster cluster-stop cluster-status cluster-clean
.PHONY: cluster-up cluster-down cluster-scale cluster-ps
.PHONY: web web-build

# ───────────────────────────── Build ─────────────────────────────

build: server cli

server:
	go build -ldflags="-s -w" -o bin/boltq-server ./cmd/server

cli:
	go build -ldflags="-s -w" -o bin/boltq ./cmd/cli

# ───────────────────────────── Run ───────────────────────────────

run: server
	./bin/boltq-server

run-disk: server
	BOLTQ_STORAGE_MODE=disk ./bin/boltq-server

# ───────────────────────────── Local Cluster (Dev) ───────────────
#
# Start a 3-node cluster on localhost for development:
#   make cluster          — start 3 voter nodes
#   make cluster-stop     — kill all nodes
#   make cluster-status   — check cluster status
#   make cluster-clean    — remove all cluster data
#
# Ports:
#   node1: HTTP=9090  TCP=9091  Raft=9100 (bootstrap leader)
#   node2: HTTP=9092  TCP=9093  Raft=9101
#   node3: HTTP=9094  TCP=9095  Raft=9102

cluster: server
	@echo "==> Starting 3-node dev cluster..."
	@mkdir -p data/node1/raft data/node2/raft data/node3/raft
	@echo "==> Starting node1 (bootstrap leader)..."
	@BOLTQ_CLUSTER_ENABLED=true BOLTQ_NODE_ID=node1 BOLTQ_RAFT_ADDR=127.0.0.1:9100 \
		BOLTQ_RAFT_DIR=./data/node1/raft BOLTQ_BOOTSTRAP=true \
		BOLTQ_HTTP_PORT=9090 BOLTQ_TCP_PORT=9091 \
		./bin/boltq-server > /tmp/boltq-node1.log 2>&1 & echo $$! > /tmp/boltq-node1.pid
	@sleep 2
	@echo "==> Starting node2 (join via seeds)..."
	@BOLTQ_CLUSTER_ENABLED=true BOLTQ_NODE_ID=node2 BOLTQ_RAFT_ADDR=127.0.0.1:9101 \
		BOLTQ_RAFT_DIR=./data/node2/raft BOLTQ_SEEDS=127.0.0.1:9090 \
		BOLTQ_HTTP_PORT=9092 BOLTQ_TCP_PORT=9093 \
		./bin/boltq-server > /tmp/boltq-node2.log 2>&1 & echo $$! > /tmp/boltq-node2.pid
	@sleep 1
	@echo "==> Starting node3 (join via seeds)..."
	@BOLTQ_CLUSTER_ENABLED=true BOLTQ_NODE_ID=node3 BOLTQ_RAFT_ADDR=127.0.0.1:9102 \
		BOLTQ_RAFT_DIR=./data/node3/raft BOLTQ_SEEDS=127.0.0.1:9090,127.0.0.1:9092 \
		BOLTQ_HTTP_PORT=9094 BOLTQ_TCP_PORT=9095 \
		./bin/boltq-server > /tmp/boltq-node3.log 2>&1 & echo $$! > /tmp/boltq-node3.pid
	@sleep 1
	@echo ""
	@echo "==> Cluster started!"
	@echo "    node1: TCP=localhost:9091  HTTP=localhost:9090  (leader)"
	@echo "    node2: TCP=localhost:9093  HTTP=localhost:9092"
	@echo "    node3: TCP=localhost:9095  HTTP=localhost:9094"
	@echo ""
	@echo "    Logs:   /tmp/boltq-node{1,2,3}.log"
	@echo "    Status: make cluster-status"
	@echo "    Stop:   make cluster-stop"

cluster-status:
	@echo "==> Node1 (localhost:9090):"
	@curl -s http://localhost:9090/cluster/status | python3 -m json.tool 2>/dev/null || echo "  node1 unreachable"
	@echo ""
	@echo "==> Node2 (localhost:9092):"
	@curl -s http://localhost:9092/cluster/status | python3 -m json.tool 2>/dev/null || echo "  node2 unreachable"
	@echo ""
	@echo "==> Node3 (localhost:9094):"
	@curl -s http://localhost:9094/cluster/status | python3 -m json.tool 2>/dev/null || echo "  node3 unreachable"

cluster-stop:
	@echo "==> Stopping cluster..."
	@kill $$(cat /tmp/boltq-node1.pid 2>/dev/null) 2>/dev/null; rm -f /tmp/boltq-node1.pid
	@kill $$(cat /tmp/boltq-node2.pid 2>/dev/null) 2>/dev/null; rm -f /tmp/boltq-node2.pid
	@kill $$(cat /tmp/boltq-node3.pid 2>/dev/null) 2>/dev/null; rm -f /tmp/boltq-node3.pid
	@echo "==> Cluster stopped"

cluster-clean: cluster-stop
	@rm -rf data/node1 data/node2 data/node3
	@rm -f /tmp/boltq-node{1,2,3}.log
	@echo "==> Cluster data cleaned"

# ───────────────────────── Docker Cluster (Production) ───────────
#
# Production cluster with Docker Compose:
#   make cluster-up                  — Start 3 voter nodes
#   make cluster-scale N=5           — Add 5 read replicas (non-voter)
#   make cluster-scale N=10          — Scale replicas to 10
#   make cluster-scale N=0           — Remove all replicas
#   make cluster-ps                  — Show running containers
#   make cluster-down                — Stop everything
#
# Architecture:
#   3 voter nodes   — fixed, handle Raft consensus
#   N replica nodes — auto-scale, non-voter read replicas

N ?= 2

cluster-up: docker
	docker compose -f deploy/docker/docker-compose.yml up -d node1 node2 node3
	@echo ""
	@echo "==> 3-node quorum started"
	@echo "    Add replicas: make cluster-scale N=5"

cluster-scale:
	docker compose -f deploy/docker/docker-compose.yml up -d --scale replica=$(N)
	@echo "==> Scaled to $(N) read replicas"

cluster-ps:
	docker compose -f deploy/docker/docker-compose.yml ps

cluster-down:
	docker compose -f deploy/docker/docker-compose.yml down -v

# ───────────────────────────── Web Admin ─────────────────────────

web:
	cd web && npm run dev

web-build:
	cd web && npm run build

# ───────────────────────────── Test ──────────────────────────────

test:
	go test -v -race ./...

bench:
	go test -bench=. -benchmem -run=^$$ ./internal/queue/
	go test -bench=. -benchmem -run=^$$ ./internal/wal/
	go test -bench=. -benchmem -run=^$$ ./internal/broker/
	go test -bench=. -benchmem -run=^$$ ./internal/api/

bench-queue:
	go test -bench=. -benchmem -benchtime=5s -run=^$$ ./internal/queue/

bench-wal:
	go test -bench=. -benchmem -benchtime=5s -run=^$$ ./internal/wal/

bench-broker:
	go test -bench=. -benchmem -benchtime=5s -run=^$$ ./internal/broker/

bench-api:
	go test -bench=. -benchmem -benchtime=5s -run=^$$ ./internal/api/

# ───────────────────────────── Docker ────────────────────────────

docker:
	docker build -t boltq:latest .

# ───────────────────────────── Clean ─────────────────────────────

clean:
	rm -rf bin/ data/
