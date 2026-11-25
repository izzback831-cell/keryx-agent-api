#!/bin/sh
set -eu
cd "$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"
mkdir -p bin/linux-amd64
(cd relay && go build -mod=readonly -trimpath -ldflags='-s -w' -o ../bin/linux-amd64/keryx-agent-api .)
(cd agent && cargo build --locked --release)
cp agent/target/release/keryx-agent bin/linux-amd64/keryx-agent
chmod 755 bin/linux-amd64/keryx-agent-api bin/linux-amd64/keryx-agent
