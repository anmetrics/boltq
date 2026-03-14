#!/bin/bash

# Configuration
SERVER_BIN="./bin/boltq-server"
K6_DIR="./scripts/k6"
DATA_DIR="./data"
LOG_FILE="boltq_test.log"
export BOLTQ_STORAGE_MODE="disk"
export BOLTQ_DATA_DIR="$DATA_DIR"

# Colors for output
GREEN='\033[0;32m'
RED='\033[0;31m'
NC='\033[0m' # No Color

function log() {
    echo -e "${GREEN}[TEST] $1${NC}"
}

function error() {
    echo -e "${RED}[ERROR] $1${NC}"
    exit 1
}

# 1. Cleanup
log "Cleaning up old data..."
killall boltq-server 2>/dev/null
rm -rf "$DATA_DIR"
mkdir -p "$DATA_DIR"

# 2. Build server
log "Building BoltQ server..."
go build -o "$SERVER_BIN" ./cmd/server || error "Build failed"

# 3. Start BoltQ (Phase 1: Initial load)
log "Starting BoltQ server (Phase 1)..."
$SERVER_BIN > "$LOG_FILE" 2>&1 &
SERVER_PID=$!
sleep 2

# Check if running
if ! ps -p $SERVER_PID > /dev/null; then
    error "Server failed to start. Check $LOG_FILE"
fi

# 4. Run k6 Publish
log "Running k6 publish load..."
k6 run "$K6_DIR/publish.js" || error "k6 publish failed"

# Capture stats before shutdown
log "Stats before shutdown:"
curl -s http://localhost:9090/stats

# 5. Shutdown Server
log "Shutting down BoltQ server (simulating crash/maintenance)..."
kill $SERVER_PID
wait $SERVER_PID 2>/dev/null
sleep 2

# 6. Start BoltQ (Phase 2: Recovery)
log "Starting BoltQ server (Phase 2 - Recovery)..."
$SERVER_BIN >> "$LOG_FILE" 2>&1 &
SERVER_PID=$!
sleep 2

# Check if running
if ! ps -p $SERVER_PID > /dev/null; then
    error "Server failed to restart. Check $LOG_FILE"
fi

# 7. Run k6 Verify
log "Running k6 verification..."
k6 run "$K6_DIR/verify.js" || error "k6 verification failed"

# 8. Final Stats
log "Final Stats:"
curl -s http://localhost:9090/stats

# Cleanup
log "Cleaning up..."
kill $SERVER_PID
log "Test Complete."
