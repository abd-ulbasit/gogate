#!/bin/bash
# GoGate Demo Script
# This script starts echo backends and optionally runs load tests.
#
# Usage:
#   ./scripts/demo.sh backends     # Start 3 TCP echo backends on ports 9001-9003
#   ./scripts/demo.sh gateway      # Start GoGate
#   ./scripts/demo.sh load-tcp     # Run TCP load test against GoGate
#   ./scripts/demo.sh load-http    # Run HTTP load test against GoGate
#   ./scripts/demo.sh all          # Start backends + gateway (in background), then wait
#   ./scripts/demo.sh stop         # Kill all demo processes

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_DIR="$(dirname "$SCRIPT_DIR")"
cd "$PROJECT_DIR"

PIDS_FILE="/tmp/gogate-demo-pids"

start_backends() {
    echo "Starting TCP echo backends on ports 9001, 9002, 9003..."
    
    # Backend 1: weight 1
    go run ./scripts/echo-tcp -listen :9001 -no-registry &
    echo $! >> "$PIDS_FILE"
    
    # Backend 2: weight 1
    go run ./scripts/echo-tcp -listen :9002 -no-registry &
    echo $! >> "$PIDS_FILE"
    
    # Backend 3: weight 2 (matches config.yaml)
    go run ./scripts/echo-tcp -listen :9003 -no-registry &
    echo $! >> "$PIDS_FILE"
    
    echo "Backends started. PIDs saved to $PIDS_FILE"
    sleep 1
}

start_gateway() {
    echo "Starting GoGate..."
    go run ./cmd/gogate/main.go &
    echo $! >> "$PIDS_FILE"
    echo "GoGate started. PID saved to $PIDS_FILE"
    sleep 2
}

run_load_tcp() {
    echo "Running TCP load test against GoGate (30s, 200 req/s, 50 workers)..."
    go run ./scripts/load-tcp -addr localhost:8080 -concurrency 50 -rate 200 -duration 30s
}

run_load_http() {
    echo "Running HTTP load test against GoGate (30s, 200 req/s, 20 workers)..."
    go run ./scripts/load-http -target http://localhost:8080/ -concurrency 20 -rate 200 -duration 30s
}

stop_all() {
    if [ -f "$PIDS_FILE" ]; then
        echo "Stopping demo processes..."
        while read pid; do
            if kill -0 "$pid" 2>/dev/null; then
                kill "$pid" 2>/dev/null || true
                echo "Killed PID $pid"
            fi
        done < "$PIDS_FILE"
        rm -f "$PIDS_FILE"
        echo "All demo processes stopped."
    else
        echo "No PIDs file found. Nothing to stop."
    fi
}

case "${1:-help}" in
    backends)
        start_backends
        echo "Press Ctrl+C to stop backends."
        wait
        ;;
    gateway)
        start_gateway
        echo "Press Ctrl+C to stop gateway."
        wait
        ;;
    load-tcp)
        run_load_tcp
        ;;
    load-http)
        run_load_http
        ;;
    all)
        start_backends
        start_gateway
        echo ""
        echo "=== GoGate Demo Running ==="
        echo "Backends: localhost:9001, localhost:9002, localhost:9003"
        echo "Gateway:  localhost:8080"
        echo "Admin:    localhost:9090"
        echo "Metrics:  http://localhost:9090/metrics"
        echo ""
        echo "Test with:"
        echo "  curl http://localhost:8080/"
        echo "  echo 'hello' | nc localhost 8080"
        echo "  ./scripts/demo.sh load-tcp"
        echo ""
        echo "Press Ctrl+C to stop all."
        wait
        ;;
    stop)
        stop_all
        ;;
    *)
        echo "GoGate Demo Script"
        echo ""
        echo "Usage: $0 <command>"
        echo ""
        echo "Commands:"
        echo "  backends    Start 3 TCP echo backends on ports 9001-9003"
        echo "  gateway     Start GoGate gateway"
        echo "  load-tcp    Run TCP load test (30s)"
        echo "  load-http   Run HTTP load test (30s)"
        echo "  all         Start backends + gateway, then wait"
        echo "  stop        Kill all demo processes"
        echo ""
        echo "Quick start:"
        echo "  Terminal 1: $0 all"
        echo "  Terminal 2: $0 load-tcp"
        ;;
esac
