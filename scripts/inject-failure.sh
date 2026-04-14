#!/bin/bash

# inject-failure.sh — Helper script for injecting failures via Docker API
# Usage: ./scripts/inject-failure.sh <action> <framework> <service>
#   action: stop|start|kill|restart
#   framework: rabbitmq|nats|kafka|temporal
#   service: downstream|postgres (for stop/start)
# Example:
#   ./scripts/inject-failure.sh stop rabbitmq downstream
#   ./scripts/inject-failure.sh start rabbitmq downstream
#   ./scripts/inject-failure.sh kill bench-rabbitmq-worker
#   ./scripts/inject-failure.sh restart bench-rabbitmq-worker

set -e

ACTION=$1
FRAMEWORK=$2
SERVICE=$3

if [ -z "$ACTION" ] || [ -z "$FRAMEWORK" ]; then
    echo "Usage: $0 <action> <framework> [service]"
    echo "  action: stop|start|kill|restart"
    echo "  framework: rabbitmq|nats|kafka|temporal"
    echo "  service: downstream|postgres (required for stop/start)"
    echo ""
    echo "Examples:"
    echo "  $0 stop rabbitmq downstream"
    echo "  $0 start rabbitmq downstream"
    echo "  $0 kill bench-rabbitmq-worker"
    echo "  $0 restart bench-rabbitmq-worker"
    exit 1
fi

case "$ACTION" in
    stop)
        if [ -z "$SERVICE" ]; then
            echo "Error: service required for stop action"
            exit 1
        fi
        NGINX_CONTAINER="bench-${FRAMEWORK}-nginx-${SERVICE}"
        echo "Stopping $NGINX_CONTAINER"
        docker stop "$NGINX_CONTAINER"
        ;;
    start)
        if [ -z "$SERVICE" ]; then
            echo "Error: service required for start action"
            exit 1
        fi
        NGINX_CONTAINER="bench-${FRAMEWORK}-nginx-${SERVICE}"
        echo "Starting $NGINX_CONTAINER"
        docker start "$NGINX_CONTAINER"
        ;;
    kill)
        CONTAINER="$FRAMEWORK"
        echo "Killing container $CONTAINER"
        docker kill --signal=SIGKILL "$CONTAINER" || true
        ;;
    restart)
        CONTAINER="$FRAMEWORK"
        echo "Restarting container $CONTAINER"
        docker restart "$CONTAINER"
        # Wait for container to be healthy
        for i in {1..30}; do
            if docker inspect -f '{{.State.Running}}' "$CONTAINER" | grep -q "true"; then
                echo "Container $CONTAINER restarted"
                exit 0
            fi
            sleep 1
        done
        echo "Error: $CONTAINER did not restart within timeout"
        exit 1
        ;;
    *)
        echo "Unknown action: $ACTION"
        exit 1
        ;;
esac

echo "Done"
