# BKNetwork Ubuntu 26.04 使用说明

本文档只适用于 **Ubuntu 26.04 x86_64（amd64）**。BKNetwork 的 Ubuntu 版本没有 Windows 或 macOS 兼容模式；页面、systemd 服务和网络操作都以本机 Ubuntu 为边界。

## 1. 安装基础依赖

在校园网 IPv6 已经能够正常分配到本机物理网卡后，安装基础命令：

```bash
sudo apt update
sudo apt install network-manager iproute2 curl ca-certificates wireguard-tools systemd-resolved python3-gi gir1.2-ayatanaappindicator3-0.1 libayatana-appindicator3-1
```

确认 NetworkManager 正在管理网卡：

```bash
nmcli general status
nmcli device status
ip -6 address show scope global
```

BKNetwork 只选择由 NetworkManager 管理、处于已连接状态、拥有公网 IPv6 的物理网卡。虚拟网卡、仅有链路本地地址的网卡以及仍有独立 IPv4 出口的其他物理网卡不会被当作安全的校园 IPv6 外层。

## 2. 安装 BKNetwork

解压发布包后从包目录执行：

```bash
sudo ./scripts/install.sh
```

安装位置如下：

| 内容 | 位置 |
| --- | --- |
| 主程序 | `/opt/bknetwork/bknetwork` |
| systemd 单元 | `/etc/systemd/system/bknetwork.service` |
| 桌面启动器 | `/usr/share/applications/bknetwork.desktop` |
| 桌面启动脚本 | `/usr/local/bin/bknetwork-launch` |
| 桌面停止脚本 | `/usr/local/bin/bknetwork-stop` |
| 顶部状态栏脚本 | `/usr/local/bin/bknetwork-indicator` |
| root 状态与恢复记录 | `/var/lib/bknetwork` |
| WireGuard 配置 | `/etc/wireguard/<名称>.conf` |

安装不会启用服务、启动隧道或接受任何第三方条款。可以从应用菜单点击 BKNetwork；启动器先用 `pkexec` 请求启动 systemd 服务，再由当前桌面用户打开 `http://127.0.0.1:13335/`。启动后，顶部状态栏会显示 BKNetwork 图标；右键左侧 Dock 中的 BKNetwork 图标并选择“关闭 BKNetwork 全部”，即可请求权限停止后端及其子进程，并检查网络恢复状态。如果没有图形会话，也可以在终端手动打开页面。

顶部状态栏图标使用 GNOME 的 Ayatana AppIndicator 兼容层，需要先安装 `python3-gi`、`gir1.2-ayatanaappindicator3-0.1` 和 `libayatana-appindicator3-1`。浏览器属于独立进程，关闭动作会让 BKNetwork 页面停止响应，但不会强制关闭整个浏览器。

更新已安装版本前先执行 `sudo systemctl stop bknetwork.service`。安装脚本会在复制文件前检查服务状态，发现仍在运行时直接退出，避免旧进程继续使用旧二进制。

从源码树安装已构建的二进制时，可以显式给出二进制路径：

```bash
sudo ./scripts/install.sh /绝对路径/bknetwork
```

脚本会拒绝非 Ubuntu 26.04 或非 amd64 主机，且不会下载软件包。依赖请用 Ubuntu 的 `apt` 单独安装。

## 3. 配置 Cloudflare WARP

WARP 客户端要从 Cloudflare 官方软件源安装：

- [Cloudflare WARP packages](https://pkg.cloudflareclient.com/)
- [Cloudflare Linux client — Get started](https://developers.cloudflare.com/warp-client/get-started/linux/)

官方页面当前把 Ubuntu Resolute（26.04）列为支持版本。以下命令来自官方 apt 流程，执行前请阅读官方条款和仓库说明：

```bash
sudo apt install ca-certificates curl gnupg lsb-release

curl -fsSL https://pkg.cloudflareclient.com/pubkey.gpg \
  | sudo gpg --yes --dearmor \
      --output /usr/share/keyrings/cloudflare-warp-archive-keyring.gpg

echo "deb [signed-by=/usr/share/keyrings/cloudflare-warp-archive-keyring.gpg] https://pkg.cloudflareclient.com/ $(lsb_release -cs) main" \
  | sudo tee /etc/apt/sources.list.d/cloudflare-client.list

sudo apt update
sudo apt install cloudflare-warp
```

首次注册必须使用与 BKNetwork systemd 服务相同的 root 上下文。BKNetwork 不自动注册、不自动接受条款；建议先设为 `warp+doh` 并保持断开，让 BKNetwork 接管后再连接：

```bash
sudo warp-cli registration new
sudo warp-cli mode warp+doh
sudo warp-cli status
```

如果要先由官方命令验证，验证后必须断开；已连接的 WARP 会与 BKNetwork 的状态机冲突：

```bash
sudo warp-cli connect
curl https://www.cloudflare.com/cdn-cgi/trace | grep '^warp='
sudo warp-cli disconnect
```

输出 `warp=on` 只能说明 WARP 客户端自身已连接；BKNetwork 还会检查校园 IPv6 外层、路由、接口状态和 IPv6 连通性。任何检查失败都会显示错误或回滚，不能据此宣称资费结果。

## 4. 配置家庭 WireGuard

安装 `wireguard-tools` 后，由管理员将配置放在 `/etc/wireguard/`：

```bash
sudo install -m 0600 ./家庭配置.conf /etc/wireguard/home.conf
sudo wg-quick strip /etc/wireguard/home.conf >/dev/null
```

页面只显示 `home` 这样的配置名称。BKNetwork 会在 root 上下文中短暂读取所选配置并在内存中校验，不会传输、打印日志或另行保存私钥；`wg-quick` 会按系统工具处理该文件，因此文件必须只允许 root 读取。配置要点：

```ini
[Interface]
Address = 10.64.0.2/32, fd00:64::2/128
# DNS 和 hook 请根据本机 systemd-resolved 配置确认后再使用

[Peer]
# 2001:db8::/32 是文档保留前缀，必须替换为真实公网 IPv6。
Endpoint = [2001:db8:1234::10]:51820
AllowedIPs = 0.0.0.0/0, ::/0
PersistentKeepalive = 25
```

Endpoint 必须是公网 IPv6 字面量，端口前的地址需要方括号。`AllowedIPs` 必须同时包含 `0.0.0.0/0` 和 `::/0`，否则 IPv4 流量不会进入隧道。家庭端需要开放 UDP 端口并配置 IPv4 转发、IPv4 NAT、IPv6 回程路由；这些服务端步骤超出 BKNetwork 的控制范围。

BKNetwork 会拒绝包含 `PreUp`、`PostUp`、`PreDown`、`PostDown` 可执行钩子、`SaveConfig = true` 或非 `Table = auto` 的配置。`DNS =` 仅在系统已安装 `systemd-resolved` 并提供 `/usr/sbin/resolvconf` 兼容接口时使用；其它配置校验或 IPv6 连通性失败会回滚并保留恢复信息。

## 5. 页面和命令行操作

打开页面后：

1. 在“连接方式”中选择 WARP 或 WireGuard。
2. 选择检测到公网 IPv6 的物理网卡；WireGuard 模式还要选择配置名称。
3. 点击连接，等待页面显示“已验证”。
4. 完成使用后点击断开，确认网络状态恢复。

如果之前用普通用户手动运行了只读预览，启动 systemd 服务前先在那个终端按 `Ctrl+C` 结束预览；两者都使用 `127.0.0.1:13335`，不能同时监听同一端口。

root 服务使用以下固定参数：

```text
/opt/bknetwork/bknetwork run --no-browser --state-dir /var/lib/bknetwork
```

只读状态接口和命令：

```bash
systemctl status bknetwork.service
sudo /opt/bknetwork/bknetwork status
curl -s http://127.0.0.1:13335/api/v1/status
```

普通用户打开页面和查看诊断不会改动网络。系统服务的状态日志位于 `/var/lib/bknetwork`，查看它应使用 `sudo /opt/bknetwork/bknetwork status`；普通用户直接执行 `status` 会读取自己的用户状态目录。连接、断开和恢复操作由 root 服务执行。`Ctrl+C`、`systemctl stop` 或 `SIGTERM` 都会触发服务的优雅退出：先关闭 HTTP 服务，再断开 BKNetwork 管理的隧道并恢复原 NetworkManager IPv4 方法。

Dock 的“关闭 BKNetwork 全部”动作调用 `/usr/local/bin/bknetwork-stop`。它停止 `bknetwork.service` 的整个 systemd 控制组，并在服务停止后以管理员权限执行一次 `recover`，用于处理上一次异常退出遗留的恢复记录。即使已先在页面断开 WARP，也不会因桌面用户无权读取 root 的恢复记录而误报失败。顶部状态栏图标会在服务停止后自动退出。

若页面显示“需要恢复”：

```bash
sudo systemctl stop bknetwork.service
sudo /opt/bknetwork/bknetwork recover
```

服务运行时 `recover` 会因状态锁被拒绝。恢复命令只处理 BKNetwork 自己记录的状态，不会主动关闭没有记录的其他 WireGuard 隧道。

“开机启动后台服务”设置只开关 systemd 的 `bknetwork.service`，不代表自动连接 WARP 或 WireGuard，也不会替用户接受 WARP 条款。首次启用前请先确认已安装服务并能手动启动。

## 6. Clash 应用分流

此功能在当前桌面用户下安装一次，随后由 BKNetwork 的 WARP 连接与恢复流程自动切换 GNOME 系统代理：

| 状态 | ChatGPT、quota-float | 其他应用 |
| --- | --- | --- |
| WARP 未启用 | 专用 HTTP 代理进入 Clash | 按原有 Clash 系统代理设置联网 |
| WARP 已启用 | 仍进入 Clash，由 Clash 规则/全局模式选择出口 | GNOME 系统代理暂停，默认路由走 WARP |
| WARP 断开或连接失败 | 专用 Clash 代理保留 | 恢复连接前的 GNOME 系统代理模式 |

这里的“规则/全局”是 Clash 内核转发模式，和“系统代理”开关不同。全局模式下选择 `GLOBAL` 的代理节点；规则模式下确保 `chatgpt.com`、`openai.com` 等相关请求命中可用代理节点。若命中 `DIRECT`，请求虽进入 Clash，仍由 WARP 提供出口。Clash 到代理节点的连接也默认通过 WARP 提供的底层网络。

### 配置应用入口

此功能需要 GNOME 桌面会话以及 `loginctl`、`runuser`、`gsettings`（分别由 `systemd`、`util-linux`、`libglib2.0-bin` 提供，Ubuntu 桌面通常已安装）。先安装包含 `app-proxy` 子命令的 BKNetwork 新版二进制，然后在 **普通桌面用户** 的终端执行，不能使用 `sudo`：

```bash
/opt/bknetwork/bknetwork app-proxy install --port 7897
/opt/bknetwork/bknetwork app-proxy status
```

`7897` 是本机 Clash Verge 当前混合端口；如已修改端口，应填实际 HTTP 或 mixed 端口。quota-float 当前构建没有启用 SOCKS 支持，因此不要填写 SOCKS-only 端口。安装器保存原始文件后，更新 ChatGPT、quota-float 的用户菜单入口及**已有**自动启动入口；不会创建原本不存在的自动启动项。专用启动器设置大小写的 `HTTP_PROXY`、`HTTPS_PROXY`、`ALL_PROXY`，并为 ChatGPT 添加 Electron 代理参数；`localhost`、`127.0.0.1`、`::1` 保持直连，用于本地 API、登录回调与客户端通信。

安装完成后，在合适的时间完全退出 ChatGPT 和 quota-float 再从应用菜单重新打开。已运行的应用或其子进程不会自动接收新的启动参数；仅关闭窗口可能仍保留后台实例。安装器不会关闭正在运行的客户端。

配置标记固定保存在 `~/.local/share/bknetwork/app-proxy/config.json`。WARP 连接前会读取本机活动 GNOME 桌面用户的配置，并把原系统代理模式保存到 root 恢复记录，再以对应用户身份修改 dconf。没有安装应用分流配置的用户、SSH/TTY 会话和 WireGuard 模式不参与这项切换。

### 日常切换

准备共用 WARP 时，在 Clash Verge 中关闭 **TUN** 和**系统代理守卫**，开启所需的系统代理，并保持 Clash 程序运行。随后在 BKNetwork 中连接 WARP：系统代理模式变为 `none`，两个专用启动的应用继续使用 Clash。断开 WARP 后，BKNetwork 恢复先前的系统代理模式，代理端口、PAC URL、绕过列表与 Clash 节点规则均保持原配置。原模式若本来是 `none`，断开后仍是 `none`，不会自行开启系统代理。

本次配置和离线测试不需要关闭当前正在使用的 TUN。BKNetwork 仍会阻止与活动 TUN 同时建立 WARP，避免两个隧道争用默认路由；要启用共用方案时再切换 Clash。此功能不会自动打开或关闭 TUN，也不会修改 Clash 的订阅、规则、节点或界面开关记录。

WARP 使用期间不要再次打开系统代理或代理守卫；否则其他应用也会重新进入 Clash。如果用户已手动选择了不同的非 `none` 代理模式，恢复时会保留该新选择。已经建立的连接可能继续使用原出口，必要时重新连接相关应用。应用自身固定的代理、终端里预先导出的代理变量以及原始 UDP 流量不属于 GNOME HTTP 系统代理的控制范围。

quota-float 的额度请求使用 reqwest，已有环境代理支持，无需改动源码或重新登录。但它内置的“开机启动”开关会重新生成自动启动文件。之后若操作了该开关，应先断开 WARP，再执行 `app-proxy remove` 和 `app-proxy install --port 7897` 重建应用入口。若出现文件冲突，请先根据提示检查相应入口及备份，不能强行覆盖。手动在终端直接运行原始 `quota-float` / `chatgpt` 命令也不会自动使用菜单入口的专用代理；可使用 `bknetwork app-proxy run quota-float` 或 `bknetwork app-proxy run chatgpt`。

### 恢复与移除

如果系统代理恢复失败，BKNetwork 保留恢复记录并提示重试；按前文先停止服务，再运行 `sudo /opt/bknetwork/bknetwork recover`。这会重试网络及系统代理恢复，不能直接删除 root 状态文件。若原桌面用户已注销，请先重新登录该用户，确保用户的 D-Bus 会话可用后再恢复。若 `warp-cli` 缺失或无法确认 WARP 已断开，需要先恢复该依赖再重试；BKNetwork 不会在隧道状态不明时重新启用系统代理。

取消应用分流前，先正常断开 WARP 并完成恢复，再以原桌面用户执行：

```bash
/opt/bknetwork/bknetwork app-proxy remove
```

该命令恢复备份的应用入口。若入口安装后被手动修改，安装器会提示冲突，避免覆盖后续修改。BKNetwork 的系统卸载不删除用户备份；如需取消分流，应先完成这一步。

技术依据：[Electron 代理启动参数](https://www.electronjs.org/zh/docs/latest/api/command-line-switches)、[mihomo 规则/全局模式](https://wiki.metacubex.one/en/config/general/)、[mihomo 流量入口](https://wiki.metacubex.one/en/config/inbound/)。quota-float 的两项额度请求位于其 `src-tauri/src/codex.rs::fetch_snapshot`，共用 `src-tauri/src/lib.rs` 创建的 reqwest 客户端；该客户端保留默认的环境代理读取行为。

## 7. 卸载与风险边界

```bash
sudo /opt/bknetwork/scripts/uninstall.sh
```

卸载会停止服务并尝试恢复网络，然后删除程序、systemd 单元、桌面入口和图标；`/var/lib/bknetwork` 恢复记录会保留。若停止或恢复失败，脚本会中止并保留程序，便于执行 `recover`。

BKNetwork 不保存 WARP 注册密钥，也不会传输、打印日志或另行保存 WireGuard 私钥；配置文件仍留在管理员控制的 `/etc/wireguard`。项目不保证校园网计费、出口 IP、速度或可用性。请遵守校园网、Cloudflare、家庭服务器和所在地区的相关规定。
