# Minimal isolated client image for a wraptool devcontainer.
#
# Deliberately has NO git and NO credentials: privileged tools (git, gcloud,
# kubectl, ...) run on the host and are reached only through wraptool's MCP
# tools. That isolation is the whole point — see examples/devcontainer/README.md.
FROM debian:bookworm-slim

RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      ca-certificates \
      curl \
      jq \
 && rm -rf /var/lib/apt/lists/*

# Node/Claude Code are installed on demand (postCreate or by the user) rather
# than baked in, to keep the image small and the trust surface minimal.
