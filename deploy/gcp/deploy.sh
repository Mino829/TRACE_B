#!/usr/bin/env bash
# Run from repository root after reviewing docs/google-cloud.md.
set -euo pipefail
exec python3 "$(dirname "$0")/manual_staging.py" "$@"
