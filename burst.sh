#!/usr/bin/env bash
set -euo pipefail

BASE_URL="${1:-http://localhost:8080}"

echo "Building burst test..."
go run ./cmd/burst "$BASE_URL"
