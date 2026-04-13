#!/bin/bash

# start-all.sh — Start shared infrastructure and all frameworks
# Usage: ./scripts/start-all.sh [--frameworks=<csv>]

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
FRAMEWORKS="${1#--frameworks=}"
FRAMEWORKS="${FRAMEWORKS:-rabbitmq,nats,kafka,temporal}"
PROJECT_NAME="at-least-once-bench"

echo "Starting shared infrastructure..."
docker compose -p "$PROJECT_NAME" -f "$SCRIPT_DIR/docker-compose.shared.yml" up -d --remove-orphans

echo "Waiting for shared infrastructure to be ready..."
for i in {1..30}; do
    if curl -s http://localhost:8080/health > /dev/null 2>&1; then
        echo "Shared infrastructure ready"
        break
    fi
    echo "Attempt $i: Waiting for downstream..."
    sleep 2
done

echo ""
echo "Port assignments:"
echo "  Shared infrastructure:"
echo "    PostgreSQL:        5432"
echo "    Downstream HTTP:   8080"
echo "    Prometheus:        9090"
echo "    Grafana:           3000"
echo ""

# Start each framework
IFS=',' read -ra FRAMEWORK_LIST <<< "$FRAMEWORKS"
for framework in "${FRAMEWORK_LIST[@]}"; do
    framework=$(echo "$framework" | xargs)

    echo "Starting $framework framework..."
    docker compose -p "$PROJECT_NAME" -f "$SCRIPT_DIR/frameworks/$framework/docker-compose.yml" up -d --remove-orphans

    case "$framework" in
        rabbitmq)
            echo "  RabbitMQ broker:   5672"
            echo "  RabbitMQ UI:       15672"
            echo "  Worker metrics:    9094"
            ;;
        nats)
            echo "  NATS server:       4222"
            echo "  NATS monitoring:   8222"
            echo "  Worker metrics:    9095"
            ;;
        kafka)
            echo "  Kafka broker:      9092"
            echo "  Worker metrics:    9096"
            ;;
        temporal)
            echo "  Temporal server:   7233"
            echo "  Temporal UI:       8083"
            echo "  Worker metrics:    9091"
            ;;
    esac

    sleep 2
done

echo ""
echo "All services started!"
echo ""
echo "Next steps:"
echo "  1. Run benchmarks:"
echo "     go run ./bench/runner/main.go --framework=rabbitmq --scenario=http-down"
echo ""
echo "  2. Or run all benchmarks:"
echo "     ./scripts/run-all.sh"
echo ""
echo "  3. View dashboards:"
echo "     Grafana:      http://localhost:3000 (admin/admin)"
echo "     Prometheus:   http://localhost:9090"
echo "     RabbitMQ:     http://localhost:15672 (bench/benchpass)"
echo "     Temporal UI:  http://localhost:8083"
echo ""
echo "  4. Stop all services:"
echo "     ./scripts/stop-all.sh"
