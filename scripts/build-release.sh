#!/usr/bin/env bash

set -Eeuo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
root_dir=$(cd -- "$script_dir/.." && pwd -P)
release_dir=${BKNETWORK_RELEASE_DIR:-"$root_dir/releases"}

die() {
  printf 'build-release.sh: %s\n' "$*" >&2
  exit 1
}

command -v go >/dev/null 2>&1 || die "未找到 go；发布构建需要 Go 1.25 或更高版本"
command -v tar >/dev/null 2>&1 || die "未找到 tar"
command -v sha256sum >/dev/null 2>&1 || die "未找到 sha256sum"

[[ -f "$root_dir/go.mod" ]] || die "脚本必须从 BKNetwork 源码树运行"
[[ -f "$root_dir/web/embed.go" ]] || die "缺少 web/embed.go，无法构建内嵌前端"

case "$release_dir" in
  /*) ;;
  *) release_dir="$root_dir/$release_dir" ;;
esac

work_dir=$(mktemp -d "${TMPDIR:-/tmp}/bknetwork-release.XXXXXX")
archive_tmp=''
cleanup() {
  if [[ -n "$archive_tmp" && -e "$archive_tmp" ]]; then
    rm -f -- "$archive_tmp"
  fi
  rm -rf -- "$work_dir"
}
trap cleanup EXIT

mkdir -p -- "$release_dir"

stage_dir="$work_dir/package"
binary="$stage_dir/bknetwork"
mkdir -p -- "$stage_dir"

printf '%s\n' '正在构建 Ubuntu 26.04 amd64 静态二进制…'
(
  cd -- "$root_dir"
  CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags='-s -w' -o "$binary" ./cmd/bknetwork
)

[[ -x "$binary" ]] || die "Go 构建没有生成可执行文件"
chmod 0755 -- "$binary"
version_output=$("$binary" version) || die "无法从构建出的二进制读取版本"
version=$(printf '%s\n' "$version_output" | awk 'NR == 1 {print $2}')
[[ -n "$version" ]] || die "无法解析应用版本：$version_output"
version=${version#v}
version=${version//[^[:alnum:]._-]/-}

package_name="bknetwork-ubuntu26.04-amd64-$version"
package_dir="$work_dir/$package_name"
mv -- "$stage_dir" "$package_dir"
binary="$package_dir/bknetwork"
mkdir -p -- "$package_dir/scripts" "$package_dir/packaging"

copy_required() {
  local source=$1
  local target=$2
  [[ -f "$source" ]] || die "缺少发布文件：$source"
  install -m 0644 -- "$source" "$target"
}

copy_executable() {
  local source=$1
  local target=$2
  [[ -f "$source" ]] || die "缺少发布脚本：$source"
  install -m 0755 -- "$source" "$target"
}

copy_required "$root_dir/README.md" "$package_dir/README.md"
copy_required "$root_dir/Ubuntu使用说明.md" "$package_dir/Ubuntu使用说明.md"
copy_required "$root_dir/LICENSE" "$package_dir/LICENSE"
copy_executable "$root_dir/scripts/install.sh" "$package_dir/scripts/install.sh"
copy_executable "$root_dir/scripts/uninstall.sh" "$package_dir/scripts/uninstall.sh"
copy_executable "$root_dir/scripts/bknetwork-launch" "$package_dir/scripts/bknetwork-launch"
copy_executable "$root_dir/scripts/bknetwork-stop" "$package_dir/scripts/bknetwork-stop"
copy_executable "$root_dir/scripts/bknetwork-indicator" "$package_dir/scripts/bknetwork-indicator"
copy_required "$root_dir/packaging/bknetwork.service" "$package_dir/packaging/bknetwork.service"
copy_required "$root_dir/packaging/bknetwork.desktop" "$package_dir/packaging/bknetwork.desktop"
copy_required "$root_dir/web/favicon.svg" "$package_dir/bknetwork.svg"

if command -v file >/dev/null 2>&1; then
  binary_description=$(file -b "$binary")
  if [[ "$binary_description" == *'dynamically linked'* ]]; then
    die "构建结果不是静态二进制：$binary_description"
  fi
fi

archive_name="$package_name.tar.gz"
archive="$release_dir/$archive_name"
archive_tmp="$release_dir/.$archive_name.tmp.$$"
tar -C "$work_dir" -czf "$archive_tmp" "$package_name"
mv -f -- "$archive_tmp" "$archive"
archive_tmp=''
(cd -- "$release_dir" && sha256sum "$archive_name" > "$archive_name.sha256")

printf '已生成：%s\n' "$archive"
printf '校验文件：%s\n' "$archive.sha256"
