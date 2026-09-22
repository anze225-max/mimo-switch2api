#!/usr/bin/env bash
# 产出可分发包：一个静默 Windows 二进制 + 安装脚本 + 使用说明，打成 zip。
set -euo pipefail
cd "$(dirname "$0")/.."

export CGO_ENABLED=0 GOOS=windows GOARCH=amd64
export GOPATH="${GOPATH:-$PWD/../.gopath}"
export GOCACHE="${GOCACHE:-$PWD/../.gocache}"

out=dist/mimo-switch-win-x64
rm -rf dist "$out"
mkdir -p "$out"

go build -trimpath -ldflags "-H=windowsgui -s -w" -o "$out/MiMoSwitch.exe" ./cmd/switch
cp tools/install.ps1 "tools/使用说明.md" "$out/"

# Windows 自带 Compress-Archive，不依赖 zip.exe。
powershell -NoProfile -ExecutionPolicy Bypass -Command \
  "Compress-Archive -Path '$out' -DestinationPath 'dist/mimo-switch-win-x64.zip' -Force"

ls -l dist
echo "打包完成：dist/mimo-switch-win-x64.zip"
