#!/bin/bash

# stop-all.sh — Stop shared infrastructure and all frameworks

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

echo "Stopping frameworks..."
for framework in temporal kafka nats rabbitmq; do
    if [ -f "$SCRIPT_DIR/frameworks/$framework/docker-compose.yml" ]; then
        echo "Stopping $framework..."
        docker compose -f "$SCRIPT_DIR/frameworks/$framework/docker-compose.yml" down
    fi
done

echo "Stopping shared infrastructure..."
docker compose -f "$SCRIPT_DIR/docker-compose.shared.yml" down

echo "All services stopped."
