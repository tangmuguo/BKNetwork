# BKNetwork for Ubuntu 26.04

BKNetwork 是一个只面向 **Ubuntu 26.04 x86_64（amd64）** 的本机网络控制台。它把校园网 IPv6 外层网络、Cloudflare WARP 和已导入的 WireGuard 配置放在同一个本地 Web 界面中管理。

详细安装和配置步骤见 [Ubuntu 使用说明](Ubuntu使用说明.md)。

本版本不支持 Windows 或 macOS，也不会为这些系统提供兼容层。程序只监听 `127.0.0.1:13335`；普通用户可以查看状态，改变网络需要由 root 运行的 systemd 服务或 `sudo` 命令完成。

## 能做什么

- 检查 NetworkManager、`iproute2`、WARP、WireGuard 和 DNS 验证依赖。
- 选择已连接且带公网 IPv6 的物理网卡，临时关闭它的 IPv4，隧道内部仍提供 IPv4 / IPv6 双栈。
- 使用 Cloudflare WARP 或 `/etc/wireguard` 中的 WireGuard 配置建立隧道。
- 可选的 Clash 应用分流：开启 WARP 时暂停 GNOME 系统代理，ChatGPT 与 quota-float 使用各自的 Clash 代理；断开或连接失败时恢复原系统代理模式。
- 断开隧道时恢复连接前保存的 NetworkManager IPv4 配置；如果恢复中断，保留状态供 `recover` 重试。
- 通过本机 Web 页面查看实时状态。前端只保留项目 GitHub 仓库入口，不依赖其他外部页面跳转。

“IPv6 免流”取决于校园网的计费规则。BKNetwork 验证本机的 IPv6 外层、隧道和双栈连通性，无法替代校园网账单验证。

## 安装发布包

发布包是一个 Ubuntu 26.04 amd64 的 `.tar.gz`，包含单个内嵌 Web 前端的静态 Go 二进制、安装脚本、systemd 单元、带 Dock 右键关闭动作的桌面入口和说明文档：

```bash
tar -xzf bknetwork-ubuntu26.04-amd64-3.0.0-ubuntu.tar.gz
cd bknetwork-ubuntu26.04-amd64-3.0.0-ubuntu
sudo ./scripts/install.sh
```

安装脚本只写入 `/opt/bknetwork`、systemd 单元、桌面入口、启动/停止/状态栏脚本和图标，并执行 `systemctl daemon-reload`。它不会自动连接 WARP/WireGuard，也不会自动启用开机启动。安装后可以从应用菜单启动 **BKNetwork**；桌面启动器会请求标准的 `pkexec` 授权启动服务，然后在当前桌面用户的上下文中打开浏览器。启动后，顶部状态栏会显示 BKNetwork 图标；右键左侧 Dock 中的 BKNetwork 图标并选择“关闭 BKNetwork 全部”，即可停止后端、其子进程，并尝试恢复网络状态。

浏览器由系统单独管理；关闭动作会让 BKNetwork 页面停止响应，但不会强制关闭整个浏览器或其它浏览器标签页。顶部状态栏图标需要 `python3-gi` 和 Ayatana AppIndicator 运行库。

更新已安装版本前请先执行 `sudo systemctl stop bknetwork.service`；安装脚本发现服务仍在运行时会拒绝覆盖文件。

卸载时运行：

```bash
sudo /opt/bknetwork/scripts/uninstall.sh
```

卸载会先尝试停止服务并恢复 BKNetwork 管理的网络，保留 `/var/lib/bknetwork` 中的恢复记录，避免在需要排查时丢失状态。

## 前置依赖

基础检查需要 NetworkManager、`iproute2`、`curl`、`resolvectl`；WireGuard 模式还需要 `wireguard-tools`，WARP 模式还需要 Cloudflare 客户端。在 Ubuntu 26.04 上可以一次安装：

```bash
sudo apt update
sudo apt install network-manager iproute2 curl ca-certificates wireguard-tools systemd-resolved python3-gi gir1.2-ayatanaappindicator3-0.1 libayatana-appindicator3-1
```

WARP 还需要 Cloudflare 官方 Linux 客户端。请按照 [Cloudflare WARP 软件源](https://pkg.cloudflareclient.com/) 和 [Linux 客户端说明](https://developers.cloudflare.com/warp-client/get-started/linux/) 安装 `cloudflare-warp`。Cloudflare 官方软件源已列出 Ubuntu Resolute（26.04）。BKNetwork 不代替安装、注册或接受 WARP 条款。

首次使用 WARP 时，在**将要运行服务的同一 root 上下文**中完成注册并确认模式：

```bash
sudo warp-cli registration new
sudo warp-cli mode warp+doh
sudo warp-cli status
```

其中 `registration new` 只需首次执行。`warp+doh` 是 BKNetwork 接受的 WARP 模式；请在 BKNetwork 接管前保持 WARP 未连接，避免外部隧道与服务状态冲突。BKNetwork 不会自动接受条款，也不会把注册步骤写入安装脚本。服务以 root 运行时，注册必须由 root 完成；不要只在普通用户的 WARP 上下文中注册后期待 systemd 服务复用它。

WireGuard 配置由管理员预先放入 `/etc/wireguard/<名称>.conf`，文件权限应为 `0600`。BKNetwork 会在 root 上下文中短暂读取所选配置并在内存中校验，不会传输、打印日志或另行保存私钥；配置文件仍由 `/etc/wireguard` 管理。为了让校园 IPv6 作为外层网络，配置应满足：

```ini
[Peer]
# 文档示例使用保留前缀；请替换为真实的公网 IPv6 地址。
Endpoint = [2001:db8:1234::10]:51820
AllowedIPs = 0.0.0.0/0, ::/0
PersistentKeepalive = 25
```

真实 Endpoint 必须是公网 IPv6 字面量并保留方括号；不要填写域名、IPv4 地址或私有地址。家庭端还必须正确配置 UDP 端口、IPv4 转发、IPv4 NAT 与 IPv6 回程路由。仅放行 UDP 端口不能保证隧道可用。BKNetwork 会拒绝包含 `PreUp`、`PostUp`、`PreDown`、`PostDown` 可执行钩子、`SaveConfig = true` 或非 `Table = auto` 的配置；`DNS =` 仅在系统已安装 `systemd-resolved` 并提供 `/usr/sbin/resolvconf` 兼容接口时使用。

## 启动、状态与恢复

安装后可用下面的命令检查服务和只读 JSON 状态：

```bash
systemctl status bknetwork.service
sudo /opt/bknetwork/bknetwork status
```

普通用户执行 `status` 只读取状态，不改变网络。手动控制服务时：

```bash
# 如果此前在终端运行过普通用户预览，请先按 Ctrl+C 结束它，释放 127.0.0.1:13335
sudo systemctl start bknetwork.service
xdg-open http://127.0.0.1:13335/
sudo systemctl stop bknetwork.service
```

图形化关闭方式：右键左侧 Dock 中的 **BKNetwork** 图标，选择“关闭 BKNetwork 全部”。该动作以管理员权限停止 `bknetwork.service`，并额外检查是否存在未完成的网络恢复记录；即使已先在页面断开 WARP，也不会因普通用户无权读取恢复记录而误报恢复失败。

停止服务会先让 HTTP 服务退出，再断开 BKNetwork 管理的隧道并恢复原网络。若操作中断或恢复失败，页面会显示待恢复状态；先停止 `bknetwork.service`，再执行：

```bash
sudo /opt/bknetwork/bknetwork recover
```

`recover` 使用同一个状态锁，服务仍在运行时会拒绝执行，避免并发改写网络。

命令行也可以直接启动本地服务：

```bash
/opt/bknetwork/bknetwork run                 # 普通用户，只读预览
sudo /opt/bknetwork/bknetwork run --no-browser
sudo /opt/bknetwork/bknetwork run --state-dir /var/lib/bknetwork --no-browser
```

服务启动参数固定使用 `--no-browser`，所以 root 服务永远不会替 root 打开图形浏览器。`--state-dir` 必须是绝对路径；默认 root 状态目录为 `/var/lib/bknetwork`，普通用户默认为用户配置目录下的 BKNetwork 目录。

页面的“开机启动后台服务”设置只执行 `systemctl enable/disable bknetwork.service`，不会连接任何隧道。未安装服务时保存会返回明确错误。

## 与 Clash Verge 共用

Clash 保持运行，可以选择规则或全局模式。先在普通桌面用户下配置应用入口（不要加 `sudo`）：

```bash
/opt/bknetwork/bknetwork app-proxy install --port 7897
/opt/bknetwork/bknetwork app-proxy status
```

端口应为 Clash 的 HTTP/混合端口；本机当前为 `7897`。配置只影响 ChatGPT、quota-float 的菜单入口和已有自动启动入口，不修改账号或凭据。安装后应在方便时完全退出并重新打开这两个应用；正在运行的实例不会被关闭。

之后，WARP 连接前保存并暂停当前桌面用户的 GNOME 系统代理，其他遵循系统代理的应用改用 WARP 默认路由；WARP 断开、连接失败或执行 `recover` 时恢复原来的 `manual` / `auto` / `none` 模式。未启用 WARP 时仍由原来的 Clash 设置管理流量。详细限制与恢复方法见 [Ubuntu 使用说明](Ubuntu使用说明.md#6-clash-应用分流)。

WARP 共用时需关闭 Clash **TUN** 和**系统代理守卫**，保留 Clash 本地代理端口；BKNetwork 不自动切换 TUN。Clash 规则模式仍按用户规则决定节点，`DIRECT` 规则会使用 WARP 出口。HTTP 系统代理、应用代理与 TUN 是不同的流量入口。

## 从源码构建

源码构建只针对 Ubuntu 26.04 amd64。需要 Go 1.25 或更高版本：

```bash
./scripts/build-release.sh
```

脚本使用 `GOOS=linux GOARCH=amd64 CGO_ENABLED=0` 构建，前端通过 Go `embed` 编入二进制，然后在 `releases/` 生成 tar.gz 和 SHA-256 校验文件；发布包同时包含 Dock 关闭动作和顶部状态栏图标所需的脚本。脚本不会执行安装、启动服务或连接隧道。运行测试：

```bash
go test ./...
```

## 历史资料与许可证

旧的 Windows 配置记录和家庭网络测试记录保存在源码目录 `docs/legacy/` 中，仅作历史参考，不随 Ubuntu 发布包分发。项目许可证见 [LICENSE](LICENSE)。
