#!/bin/bash
# 发布新版本：编译 Mac + Windows 程序，上传到 GitHub 发布页，主播那边重启程序就会自动更新
# 用法：./release.sh v1.0.1 "这次改了什么"
set -euo pipefail
cd "$(dirname "$0")"

VER="${1:-}"
NOTES="${2:-}"
if [[ ! "$VER" =~ ^v[0-9]+(\.[0-9]+)*$ ]]; then
  echo "用法：./release.sh v1.0.1 \"更新说明\"（版本号要比上一次大）"
  exit 1
fi
if [[ -n "$(git status --porcelain)" ]]; then
  echo "❌ 还有没提交的改动，先 git commit 再发布"
  exit 1
fi
if git rev-parse "$VER" >/dev/null 2>&1; then
  echo "❌ 版本 $VER 已经发布过了，换一个更大的版本号"
  exit 1
fi

LDFLAGS="-X main.Version=$VER"
OUT="$(mktemp -d)"

echo "▶ 编译 Mac 版…"
go build -ldflags "$LDFLAGS" -o "$OUT/bug-doodle-server-mac" .
echo "▶ 编译 Windows 版…"
GOOS=windows GOARCH=amd64 go build -ldflags "$LDFLAGS" -o "$OUT/bug-doodle-game.exe" .

(cd "$OUT" && for f in bug-doodle-server-mac bug-doodle-game.exe; do shasum -a 256 "$f" > "$f.sha256"; done)

# 本地目录里的程序也换成新版（直接发给新主播用的那份）
cp "$OUT/bug-doodle-server-mac" bug-doodle-server
cp "$OUT/bug-doodle-game.exe" dist-win/bug-doodle-game.exe

echo "▶ 推送代码和版本标签…"
git push origin HEAD
git tag "$VER"
git push origin "$VER"

echo "▶ 创建 GitHub 发布…"
gh release create "$VER" \
  "$OUT/bug-doodle-game.exe" "$OUT/bug-doodle-game.exe.sha256" \
  "$OUT/bug-doodle-server-mac" "$OUT/bug-doodle-server-mac.sha256" \
  --title "$VER" --notes "${NOTES:-$VER}"

rm -rf "$OUT"
echo "✅ 发布完成：$VER（主播重启程序就会自动更新）"
echo "   注意：bug-doodle-server 和 dist-win 里的程序已换成新版，记得之后一起提交"
