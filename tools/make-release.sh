#!/usr/bin/env bash
# 产出可分发的单文件 dist/MiMoSwitch.exe。说明文档不随下载走：安装时由程序写进安装目录。
set -euo pipefail
cd "$(dirname "$0")/.."

export CGO_ENABLED=0 GOOS=windows GOARCH=amd64
export GOPATH="${GOPATH:-$PWD/../.gopath}"
export GOCACHE="${GOCACHE:-$PWD/../.gocache}"

rm -rf dist
mkdir -p dist

go build -trimpath -buildvcs=false -ldflags "-H=windowsgui -s -w" -o dist/MiMoSwitch.exe ./cmd/switch

ls -l dist
echo "打包完成：dist/MiMoSwitch.exe"
