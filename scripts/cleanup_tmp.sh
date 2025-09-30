#!/usr/bin/env bash
set -euo pipefail

# Remove temp files created by the service older than 1 hour.
# Targets files in system temp dir matching prefixes used by the app.

TMPDIR=${TMPDIR:-/tmp}
find "$TMPDIR" \
  -maxdepth 1 \
  -type f \( -name 'pdfdl-*.pdf' -o -name 's3pdf-*.pdf' \) \
  -mmin +60 -print -delete || true

echo "Temp cleanup done at $(date)"

