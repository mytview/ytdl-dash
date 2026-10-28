#!/bin/sh
# Build both images. Local: ./build.sh (loads ytdl-dash:dev and ytdl-dash-agent:dev). Publish: REGISTRY=ghcr.io/mytview VERSION=0.1.0 ./build.sh push
set -eu
VERSION=${VERSION:-dev}; REGISTRY=${REGISTRY:-}
for cmd in agent dash; do
  case $cmd in agent) name=ytdl-dash-agent;; dash) name=ytdl-dash;; esac
  if [ "${1:-}" = push ]; then
    [ -n "$REGISTRY" ] || { echo "set REGISTRY to push" >&2; exit 2; }
    docker buildx build --platform linux/amd64,linux/arm64 --build-arg CMD=$cmd --build-arg VERSION=$VERSION -t $REGISTRY/$name:$VERSION -t $REGISTRY/$name:latest --push .
  else
    docker buildx build --build-arg CMD=$cmd --build-arg VERSION=$VERSION -t $name:$VERSION --load .
  fi
done
