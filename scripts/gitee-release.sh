#!/usr/bin/env bash
# 在 Gitee Go(国内 runner)上构建 deepx 多平台二进制并发布到 Gitee 发行版。
#
# 为什么单独搞这个:GitHub Actions 的境外 runner 往 Gitee 传发行版附件会被限速/卡死
# (大文件上行被掐流),所以把"构建 + 上传"整体挪到 Gitee Go(国内)跑。
# 项目 CGO_ENABLED=0 纯 Go,一台 runner 出 5 平台;产物命名严格对齐 .goreleaser.yaml,
# 保证 install.sh(SOURCE=gitee)能按 deepx_<ver>_<os>_<arch>.(tar.gz|zip) + checksums.txt 下到。
#
# 必需环境变量:
#   GITEE_TOKEN   Gitee 私人令牌(在 Gitee Go 流水线里配成密钥/环境变量)
#   TAG           发布 tag,如 v0.2.53;不传则用 git describe 推断
# 可选:
#   GITEE_OWNER=itmisx   GITEE_REPO=deepx-code
#   OUTDIR=dist          产物目录
#   GOPROXY=...           默认 https://goproxy.cn,direct(国内加速)
#   PLATFORMS="os arch ext;..."  覆盖构建矩阵(本地测试用)
#   DRY_RUN=1            只构建不上传(本地验证)
set -euo pipefail

GITEE_OWNER="${GITEE_OWNER:-itmisx}"
GITEE_REPO="${GITEE_REPO:-deepx-code}"
OUTDIR="${OUTDIR:-dist}"
export GOPROXY="${GOPROXY:-https://goproxy.cn,direct}"
export CGO_ENABLED=0

TAG="${TAG:-$(git describe --tags --exact-match 2>/dev/null || git describe --tags 2>/dev/null || true)}"
[ -n "$TAG" ] || { echo "缺少 TAG,且 git describe 失败" >&2; exit 1; }
VER="${TAG#v}"
COMMIT="$(git rev-parse HEAD 2>/dev/null || echo unknown)"
DATE="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
# 与 .goreleaser.yaml 的 ldflags 对齐(版本号注入点必须一致,否则 deepx --version 不对)
LDFLAGS="-s -w -X main.version=${VER} -X main.commit=${COMMIT} -X main.date=${DATE} -X deepx/skill.builtinVersion=${VER}"

# 与 .goreleaser.yaml 一致的 5 平台(windows/arm64 不出包)
PLATFORMS="${PLATFORMS:-darwin amd64 tar.gz;darwin arm64 tar.gz;linux amd64 tar.gz;linux arm64 tar.gz;windows amd64 zip}"

# windows 包要 zip,缺了早点报错
if printf '%s' "$PLATFORMS" | grep -q 'zip' && ! command -v zip >/dev/null 2>&1; then
  echo "需要 zip 打 windows 包,请在构建镜像里装上(apt-get install -y zip)" >&2; exit 1
fi

rm -rf "$OUTDIR"; mkdir -p "$OUTDIR"
ABS_OUT="$(cd "$OUTDIR" && pwd)"
STAGE="$(mktemp -d)"; trap 'rm -rf "$STAGE"' EXIT

echo "==> deepx ${TAG}  (ver=${VER} commit=${COMMIT:0:8})"
OLD_IFS="$IFS"; IFS=';'
for p in $PLATFORMS; do
  IFS=' ' read -r OS ARCH EXT <<< "$(echo "$p" | xargs)"
  bin="deepx"; [ "$OS" = "windows" ] && bin="deepx.exe"
  echo "==> build ${OS}/${ARCH}"
  work="$STAGE/${OS}_${ARCH}"; mkdir -p "$work"
  GOOS="$OS" GOARCH="$ARCH" go build -trimpath -ldflags="$LDFLAGS" -o "$work/$bin" .
  cp README.md "$work/" 2>/dev/null || true
  cp LICENSE* "$work/" 2>/dev/null || true
  name="deepx_${VER}_${OS}_${ARCH}"
  if [ "$EXT" = "zip" ]; then
    ( cd "$work" && zip -qr "${ABS_OUT}/${name}.zip" . )
  else
    tar -czf "${ABS_OUT}/${name}.tar.gz" -C "$work" .
  fi
done
IFS="$OLD_IFS"

echo "==> checksums.txt"
if command -v sha256sum >/dev/null 2>&1; then
  ( cd "$ABS_OUT" && sha256sum * > checksums.txt )
else
  ( cd "$ABS_OUT" && shasum -a 256 * > checksums.txt )
fi
ls -lh "$ABS_OUT"

if [ -n "${DRY_RUN:-}" ]; then echo "DRY_RUN:跳过上传"; exit 0; fi
[ -n "${GITEE_TOKEN:-}" ] || { echo "缺少 GITEE_TOKEN" >&2; exit 1; }

API="https://gitee.com/api/v5/repos/${GITEE_OWNER}/${GITEE_REPO}"
# 不依赖 jq,用 sed 抠 release id(镜像里不一定有 jq)
extract_id() { sed -n 's/.*"id":[[:space:]]*\([0-9]\{1,\}\).*/\1/p' | head -1; }

echo "==> 创建/获取 Gitee 发行版 ${TAG}"
RESP="$(curl -sS --connect-timeout 20 --max-time 60 -X POST "${API}/releases" \
  -H 'Content-Type: application/json' \
  -d "{\"access_token\":\"${GITEE_TOKEN}\",\"tag_name\":\"${TAG}\",\"name\":\"${TAG}\",\"body\":\"deepx ${TAG}\",\"target_commitish\":\"main\"}" || true)"
RID="$(printf '%s' "$RESP" | extract_id)"
if [ -z "$RID" ]; then
  echo "   创建未返回 id(可能已存在),按 tag 取回…"
  RESP="$(curl -sS --connect-timeout 20 --max-time 60 "${API}/releases/tags/${TAG}?access_token=${GITEE_TOKEN}" || true)"
  RID="$(printf '%s' "$RESP" | extract_id)"
fi
[ -n "$RID" ] || { echo "无法创建/获取发行版: $RESP" >&2; exit 1; }
echo "   release id = $RID"

fail=0
for f in "$ABS_OUT"/*; do
  [ -f "$f" ] || continue
  echo "==> 上传 $(basename "$f")"
  code="$(curl -sS -o /tmp/gitee_up.json -w '%{http_code}' \
    --connect-timeout 20 --max-time 300 --retry 2 --retry-delay 5 \
    -X POST "${API}/releases/${RID}/attach_files" \
    -F "access_token=${GITEE_TOKEN}" -F "file=@${f}" || echo 000)"
  case "$code" in
    200|201) echo "   OK" ;;
    *) echo "   失败 http=${code}: $(head -c 200 /tmp/gitee_up.json 2>/dev/null)"; fail=1 ;;
  esac
done
[ "$fail" = 0 ] && echo "==> Gitee 发布完成" || { echo "==> 有文件上传失败" >&2; exit 1; }
