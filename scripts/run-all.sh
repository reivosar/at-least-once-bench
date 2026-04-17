#!/bin/bash

# run-all.sh — Orchestrate benchmarks across all frameworks and scenarios
# Usage: ./scripts/run-all.sh [options]
#   --duration=<seconds>   Test duration (default: 60)
#   --rate=<msg/s>        Message submission rate (default: 100)
#   --warmup=<seconds>    Warmup duration (default: 10)
#   --cooldown=<seconds>  Cooldown duration (default: 30)
#   --frameworks=<csv>    Comma-separated framework list (default: rabbitmq,nats,kafka,temporal)
#   --scenarios=<csv>     Comma-separated scenario list (default: http-down,db-down,worker-crash)
#   --dry-run             Print commands without executing

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
RUNNER="${SCRIPT_DIR}/bench/runner"

# Defaults
DURATION=60
RATE=100
WARMUP=10
COOLDOWN=30
FRAMEWORKS="rabbitmq,nats,kafka,temporal"
SCENARIOS="http-down,db-down,worker-crash"
DRY_RUN=false

# Parse flags
for arg in "$@"; do
    case $arg in
        --duration=*)
            DURATION="${arg#*=}"
            ;;
        --rate=*)
            RATE="${arg#*=}"
            ;;
        --frameworks=*)
            FRAMEWORKS="${arg#*=}"
            ;;
        --scenarios=*)
            SCENARIOS="${arg#*=}"
            ;;
        --warmup=*)
            WARMUP="${arg#*=}"
            ;;
        --cooldown=*)
            COOLDOWN="${arg#*=}"
            ;;
        --dry-run)
            DRY_RUN=true
            ;;
        *)
            echo "Unknown option: $arg"
            exit 1
            ;;
    esac
done

echo "at-least-once-bench: Running all framework × scenario combinations"
echo "Duration: ${DURATION}s, Rate: ${RATE} msg/s, Warmup: ${WARMUP}s, Cooldown: ${COOLDOWN}s"

# Check shared infrastructure is running
echo "Checking shared infrastructure..."
if ! docker ps | grep -q postgres; then
    echo "Error: postgres container not running"
    echo "Start with: docker compose -f docker-compose.shared.yml up -d"
    exit 1
fi
if ! docker ps | grep -q downstream; then
    echo "Error: downstream container not running"
    echo "Start with: docker compose -f docker-compose.shared.yml up -d"
    exit 1
fi

TOTAL_TESTS=0
PASSED_TESTS=0
FAILED_TESTS=0

# Iterate over frameworks and scenarios
IFS=',' read -ra FRAMEWORK_LIST <<< "$FRAMEWORKS"
IFS=',' read -ra SCENARIO_LIST <<< "$SCENARIOS"

for framework in "${FRAMEWORK_LIST[@]}"; do
    framework=$(echo "$framework" | xargs)  # trim whitespace

    # Check framework is running
    if ! docker ps | grep -q "${framework}-worker"; then
        echo "⚠️  Skipping $framework: worker not running (start with: docker compose -f frameworks/$framework/docker-compose.yml up -d)"
        continue
    fi

    for scenario in "${SCENARIO_LIST[@]}"; do
        scenario=$(echo "$scenario" | xargs)  # trim whitespace

        TEST_NAME="${framework}-${scenario}"
        REPORT_FILE="results/${TEST_NAME}.json"

        echo ""
        echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
        echo "Running: $TEST_NAME"
        echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"

        CMD="CGO_ENABLED=0 go run $RUNNER \
            --framework=$framework \
            --scenario=$scenario \
            --duration=${DURATION}s \
            --rate=$RATE \
            --warmup=${WARMUP}s \
            --cooldown=${COOLDOWN}s \
            --report=$REPORT_FILE"

        TOTAL_TESTS=$((TOTAL_TESTS + 1))

        if [ "$DRY_RUN" = true ]; then
            echo "[DRY RUN] $CMD"
        else
            if eval "$CMD"; then
                echo "✅ PASSED: $TEST_NAME"
                PASSED_TESTS=$((PASSED_TESTS + 1))

                # Display summary
                if [ -f "$REPORT_FILE" ]; then
                    echo "📊 Report: $REPORT_FILE"
                    if command -v jq &> /dev/null; then
                        jq -r '.jobs_processed, .jobs_lost, .throughput' "$REPORT_FILE" 2>/dev/null | \
                            paste -d' ' - - - | \
                            awk '{printf "   Processed: %d, Lost: %d, Throughput: %.2f msg/s\n", $1, $2, $3}' || true
                    fi
                fi
            else
                echo "❌ FAILED: $TEST_NAME"
                FAILED_TESTS=$((FAILED_TESTS + 1))
            fi
        fi
    done
done

# Summary
echo ""
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "Summary"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "Total tests: $TOTAL_TESTS"
echo "Passed: ✅ $PASSED_TESTS"
echo "Failed: ❌ $FAILED_TESTS"

if [ $FAILED_TESTS -gt 0 ]; then
    exit 1
fi
