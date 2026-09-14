#!/usr/bin/env bash

set -Eeuo pipefail

app_dir=/opt/bknetwork
service_name=bknetwork.service
service_path=/etc/systemd/system/$service_name
desktop_path=/usr/share/applications/bknetwork.desktop
launcher_path=/usr/local/bin/bknetwork-launch
stopper_path=/usr/local/bin/bknetwork-stop
indicator_path=/usr/local/bin/bknetwork-indicator
icon_path=/usr/share/icons/hicolor/scalable/apps/bknetwork.svg
doc_dir=/usr/share/doc/bknetwork

die() {
  printf 'uninstall.sh: %s\n' "$*" >&2
  exit 1
}

[[ ${EUID:-$(id -u)} -eq 0 ]] || die "卸载需要 root，请使用 sudo"
[[ -r /etc/os-release ]] || die "无法读取 /etc/os-release"
# shellcheck disable=SC1091
. /etc/os-release
[[ ${ID:-} == ubuntu && ${VERSION_ID:-} == 26.04 ]] || die "只支持 Ubuntu 26.04（当前：${ID:-未知} ${VERSION_ID:-未知}）"

if systemctl is-active --quiet "$service_name"; then
  printf '%s\n' '正在停止 BKNetwork 服务并等待网络恢复…'
  systemctl stop "$service_name" || die "服务停止失败；已保留安装文件，请先检查并执行 recover"
fi
if systemctl is-active --quiet "$service_name"; then
  die "服务停止后仍处于 active；已保留安装文件，请先检查 systemctl status"
fi
[[ -x "$app_dir/bknetwork" ]] || die "找不到已安装的 BKNetwork 二进制，已保留其他文件"
if ! "$app_dir/bknetwork" recover --state-dir /var/lib/bknetwork; then
  die "网络恢复失败；已保留安装文件和 /var/lib/bknetwork，请先修复后重试"
fi
if systemctl is-enabled --quiet "$service_name"; then
  systemctl disable "$service_name" || die "无法禁用 systemd 服务"
fi

rm -f -- "$service_path" "$desktop_path" "$launcher_path" "$stopper_path" "$indicator_path" "$icon_path"
rm -f -- "$doc_dir/README.md" "$doc_dir/Ubuntu使用说明.md" "$doc_dir/LICENSE"
rm -f -- "$app_dir/bknetwork" "$app_dir/bknetwork.svg" "$app_dir/README.md" "$app_dir/Ubuntu使用说明.md" "$app_dir/LICENSE"
rm -f -- "$app_dir/scripts/install.sh" "$app_dir/scripts/uninstall.sh" "$app_dir/scripts/bknetwork-launch" "$app_dir/scripts/bknetwork-stop" "$app_dir/scripts/bknetwork-indicator"
rm -f -- "$app_dir/packaging/bknetwork.service" "$app_dir/packaging/bknetwork.desktop"

systemctl daemon-reload

if command -v update-desktop-database >/dev/null 2>&1; then
  update-desktop-database /usr/share/applications >/dev/null 2>&1 || true
fi

# rmdir 只删除本次安装创建且已经为空的目录；意外留下的文件会被保留。
rmdir --ignore-fail-on-non-empty "$app_dir/scripts" "$app_dir/packaging" "$app_dir" 2>/dev/null || true
rmdir --ignore-fail-on-non-empty "$doc_dir" 2>/dev/null || true

printf '%s\n' 'BKNetwork 程序、systemd 单元、桌面入口和图标已移除。'
printf '%s\n' '恢复记录仍保留在 /var/lib/bknetwork；如需清理，请先确认网络已恢复后手动删除。'
