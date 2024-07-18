#!/usr/bin/env bash
set -euox pipefail
docker build . --file datasette.Dockerfile --tag ghcr.io/dzfranklin/bsky-play-datasette \
  --build-arg VERSION=0.64.8
