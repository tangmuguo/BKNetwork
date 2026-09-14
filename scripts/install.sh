#!/usr/bin/env bash

set -Eeuo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
source_root=$(cd -- "$script_dir/.." && pwd -P)
app_dir=/opt/bknetwork
service_name=bknetwork.service
service_path=/etc/systemd/system/$service_name
desktop_path=/usr/share/applications/bknetwork.desktop
launcher_path=/usr/local/bin/bknetwork-launch
stopper_path=/usr/local/bin/bknetwork-stop
indicator_path=/usr/local/bin/bknetwork-indicator
icon_dir=/usr/share/icons/hicolor/scalable/apps
icon_path=$icon_dir/bknetwork.svg
doc_dir=/usr/share/doc/bknetwork

die() {
  printf 'install.sh: %s\n' "$*" >&2
  exit 1
}

install_file() {
  local mode=$1
  local source=$2
  local target=$3
  if [[ "$(readlink -f -- "$source")" != "$(readlink -f -- "$target")" ]]; then
    install -m "$mode" -- "$source" "$target"
  fi
}

usage() {
  printf '用法：sudo %s [二进制路径]\n' "$0"
  printf '二进制路径可省略，默认读取发布包或源码树根目录下的 bknetwork。\n'
}

[[ $# -le 1 ]] || { usage >&2; exit 2; }
[[ ${EUID:-$(id -u)} -eq 0 ]] || die "安装需要 root，请使用 sudo"
command -v systemctl >/dev/null 2>&1 || die "找不到 systemctl；请在 Ubuntu 26.04 的 systemd 主机上安装"

[[ -r /etc/os-release ]] || die "无法读取 /etc/os-release"
# shellcheck disable=SC1091
. /etc/os-release
[[ ${ID:-} == ubuntu && ${VERSION_ID:-} == 26.04 ]] || die "只支持 Ubuntu 26.04（当前：${ID:-未知} ${VERSION_ID:-未知}）"
architecture=$(dpkg --print-architecture 2>/dev/null || true)
if [[ -z "$architecture" ]]; then
  architecture=$(uname -m)
fi
[[ "$architecture" == amd64 || "$architecture" == x86_64 ]] || die "只支持 x86_64/amd64（当前：$architecture）"

binary_path=${1:-"$source_root/bknetwork"}
if [[ "$binary_path" != /* ]]; then
  binary_path="$(pwd -P)/$binary_path"
fi
[[ -f "$binary_path" && -x "$binary_path" ]] || die "找不到可执行二进制：$binary_path"

service_template="$source_root/packaging/bknetwork.service"
desktop_template="$source_root/packaging/bknetwork.desktop"
[[ -f "$service_template" ]] || die "缺少 packaging/bknetwork.service"
[[ -f "$desktop_template" ]] || die "缺少 packaging/bknetwork.desktop"

for required_file in \
  "$script_dir/install.sh" \
  "$script_dir/uninstall.sh" \
  "$script_dir/bknetwork-launch" \
  "$script_dir/bknetwork-stop" \
  "$script_dir/bknetwork-indicator" \
  "$source_root/README.md" \
  "$source_root/Ubuntu使用说明.md" \
  "$source_root/LICENSE"; do
  [[ -f "$required_file" ]] || die "缺少发布文件：$required_file"
done

icon_source=''
for candidate in "$source_root/bknetwork.svg" "$source_root/web/favicon.svg"; do
  if [[ -f "$candidate" ]]; then
    icon_source=$candidate
    break
  fi
done
[[ -n "$icon_source" ]] || die "缺少 bknetwork.svg 或 web/favicon.svg"

if systemctl is-active --quiet "$service_name"; then
  die "BKNetwork 服务正在运行；请先执行 sudo systemctl stop $service_name，再重新安装"
fi

install -d -m 0755 "$app_dir" "$app_dir/scripts" "$app_dir/packaging"
install -d -m 0700 /var/lib/bknetwork
install -d -m 0755 "$doc_dir" "$icon_dir"

if [[ "$binary_path" != "$app_dir/bknetwork" ]]; then
  install_file 0755 "$binary_path" "$app_dir/bknetwork"
fi
install_file 0755 "$script_dir/install.sh" "$app_dir/scripts/install.sh"
install_file 0755 "$script_dir/uninstall.sh" "$app_dir/scripts/uninstall.sh"
install_file 0755 "$script_dir/bknetwork-launch" "$app_dir/scripts/bknetwork-launch"
install_file 0755 "$script_dir/bknetwork-stop" "$app_dir/scripts/bknetwork-stop"
install_file 0755 "$script_dir/bknetwork-indicator" "$app_dir/scripts/bknetwork-indicator"
install_file 0644 "$service_template" "$app_dir/packaging/bknetwork.service"
install_file 0644 "$desktop_template" "$app_dir/packaging/bknetwork.desktop"
install_file 0644 "$icon_source" "$app_dir/bknetwork.svg"

install_file 0644 "$service_template" "$service_path"
install_file 0644 "$desktop_template" "$desktop_path"
install_file 0755 "$script_dir/bknetwork-launch" "$launcher_path"
install_file 0755 "$script_dir/bknetwork-stop" "$stopper_path"
install_file 0755 "$script_dir/bknetwork-indicator" "$indicator_path"
install_file 0644 "$icon_source" "$icon_path"

for document in README.md Ubuntu使用说明.md LICENSE; do
  install_file 0644 "$source_root/$document" "$app_dir/$document"
  install_file 0644 "$source_root/$document" "$doc_dir/$document"
done

systemctl daemon-reload

if command -v update-desktop-database >/dev/null 2>&1; then
  update-desktop-database /usr/share/applications >/dev/null 2>&1 || true
fi

printf '%s\n' 'BKNetwork 已安装到 /opt/bknetwork。'
printf '%s\n' 'systemd 单元已刷新，但服务尚未启动且不会自动连接隧道。'
printf '%s\n' '可从应用菜单启动；右键 Dock 中的 BKNetwork 图标可选择“关闭 BKNetwork 全部”。'
printf '%s\n' '启动后会在顶部状态栏显示 BKNetwork 图标（需要 python3-gi 和 Ayatana AppIndicator 运行库）。'
