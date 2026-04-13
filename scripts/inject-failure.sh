#!/bin/bash

# inject-failure.sh — Helper script for injecting failures via Docker API
# Usage: ./scripts/inject-failure.sh <action> <target>
#   action: disconnect|connect|kill|restart
#   target: downstream|postgres|<framework>-worker

set -e

ACTION=$1
TARGET=$2

if [ -z "$ACTION" ] || [ -z "$TARGET" ]; then
    echo "Usage: $0 <action> <target>"
    echo "  action: disconnect|connect|kill|restart"
    echo "  target: downstream|postgres|<framework>-worker"
    exit 1
fi

case "$ACTION" in
    disconnect)
        echo "Disconnecting $TARGET from bench-net"
        docker network disconnect bench-net "$TARGET" || true
        ;;
    connect)
        echo "Connecting $TARGET to bench-net"
        docker network connect bench-net "$TARGET" || true
        ;;
    kill)
        echo "Killing container $TARGET"
        docker kill --signal=SIGKILL "$TARGET" || true
        ;;
    restart)
        echo "Restarting container $TARGET"
        docker restart "$TARGET"
        # Wait for container to be healthy
        for i in {1..30}; do
            if docker inspect -f '{{.State.Running}}' "$TARGET" | grep -q "true"; then
                echo "Container $TARGET restarted"
                exit 0
            fi
            sleep 1
        done
        echo "Error: $TARGET did not restart within timeout"
        exit 1
        ;;
    *)
        echo "Unknown action: $ACTION"
        exit 1
        ;;
esac

echo "Done"
