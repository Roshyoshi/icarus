#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
mkdir -p .tools
curl -fsSL https://github.com/tlaplus/tlaplus/releases/download/v1.8.0/tla2tools.jar \
    -o .tools/tla2tools.jar.download
expected=0f56d6a813368c48008dfbecb23a73ab43a4a5e1bf09f026782a674aa9d4b3c9
actual=$(shasum -a 256 .tools/tla2tools.jar.download | cut -d ' ' -f 1)
if [ "$actual" != "$expected" ]; then
    echo 'TLC checksum mismatch; inspect upstream release before updating the pin.' >&2
    exit 1
fi
mv .tools/tla2tools.jar.download .tools/tla2tools.jar
