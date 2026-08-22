#!/bin/sh
# Entrypoint for the smoke-remote git server (see deploy/Dockerfile.gitserver).
# Initialises a bare marketplace repo once, then serves it over the git://
# protocol with receive-pack (push) enabled.
set -e

REPO=/srv/git/marketplace.git
if [ ! -d "$REPO" ]; then
  git init --bare -b master "$REPO"
fi

# --export-all      : serve every repo under base-path without export markers
# --enable=receive-pack : allow push (default is fetch-only)
# --informative-errors  : return useful errors to the client
exec git daemon \
  --reuseaddr \
  --listen=0.0.0.0 --port=9418 \
  --base-path=/srv/git \
  --export-all \
  --enable=receive-pack \
  --verbose --informative-errors
