#!/bin/sh
# Build-time install for the wraptool-connect feature.
#
# Deliberately installs NO packages — the feature must lay over any base image
# without assumptions. It only persists the resolved option values and drops the
# runtime connect script, which the postCreateCommand runs later.
set -eu

SHARE=/usr/local/share/wraptool-connect
FEATURE_DIR="$(cd "$(dirname "$0")" && pwd)"

mkdir -p "$SHARE"

# Feature options arrive as upper-cased env vars at install time; postCreate runs
# in a fresh shell without them, so freeze them into an env file connect.sh reads.
cat > "$SHARE/options.env" <<EOF
WT_HARNESS='${HARNESS:-claude}'
WT_PORT='${PORT:-8717}'
WT_URL='${URL:-}'
EOF

cp "$FEATURE_DIR/connect.sh" "$SHARE/connect.sh"
chmod +x "$SHARE/connect.sh"

echo "wraptool-connect: installed (harness=${HARNESS:-claude}, port=${PORT:-8717})"
