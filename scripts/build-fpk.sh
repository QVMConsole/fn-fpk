#!/bin/bash
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
PACKAGE="$ROOT/package/qvmconsole-manager"
DIST="$ROOT/dist"
VERSION="$(awk -F= '$1 == "version" { print $2; exit }' "$PACKAGE/manifest" | tr -d '[:space:]')"
OUTPUT="qvmconsole-manager-${VERSION}-x86_64.fpk"
BINARY="$PACKAGE/app/server/qvmconsole-manager"

if [ -z "$VERSION" ]; then
  echo "manifest 缺少版本号。" >&2
  exit 1
fi

if ! command -v fnpack >/dev/null 2>&1; then
  echo "缺少 fnpack，请先按 FN-Docs 安装 fnpack 1.2.3。" >&2
  exit 1
fi

if [ ! -f "$BINARY" ]; then
  echo "缺少 Linux/amd64 管理器二进制，请先运行 scripts/build.ps1。" >&2
  exit 1
fi

# 防止 manifest 已升级但 FPK 仍携带上一次编译的旧管理器。
chmod 0755 "$BINARY"
BINARY_VERSION="$("$BINARY" version 2>/dev/null || true)"
if [ "$BINARY_VERSION" != "$VERSION" ]; then
  echo "管理器二进制版本不匹配：manifest=$VERSION，binary=${BINARY_VERSION:-无法读取}。请先在本地运行 scripts/build.ps1 并等待同步完成。" >&2
  exit 1
fi

if find "$PACKAGE" -type f \( -name 'install.sh' -o -name 'kvm-console-linux-*.tar.gz' -o -name 'kvm-console' \) | grep -q .; then
  echo "FPK 目录包含被禁止的 QVMConsole 安装文件。" >&2
  exit 1
fi

# Windows 同步到飞牛后不会保留 POSIX 模式，发布前统一修复。
find "$PACKAGE" -type d -exec chmod 0755 {} +
find "$PACKAGE" -type f -exec chmod 0644 {} +
chmod 0755 "$PACKAGE"/cmd/* "$BINARY"

cd "$ROOT"
fnpack build --directory "$PACKAGE"
mkdir -p "$DIST"
cp -f "$ROOT/qvmconsole-manager.fpk" "$DIST/$OUTPUT"
(
  cd "$DIST"
  sha256sum "$OUTPUT" > "$OUTPUT.sha256"
)

echo "构建完成: $DIST/$OUTPUT"
cat "$DIST/$OUTPUT.sha256"
