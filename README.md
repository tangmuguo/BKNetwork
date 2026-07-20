BKNetwork
=========

> 独立 Android IPv6-only 隧道客户端见 [`android/ipv6-only-client`](android/ipv6-only-client/README.md)。它使用标准 WireGuard 服务器，强制 IPv6 外层且不回退到 IPv4。

BKNetwork 是一个轻量级本地服务，带有内置 Web 管理界面，用于简化用户针对北科校园网络的部分管理，并提供免流功能。

**仅支持 Windows x64** 系统。仅在 Win11 测试，不保证支持 Win10 使用。

[Github:tangmuguo/BKNetwork](https://github.com/tangmuguo/BKNetwork)

## 下载与安装

在右侧栏 Releases 页面下载带版本号的 `.zip` 压缩包，解压后双击运行 `bknetwork.exe`，首次运行需要点击弹窗中 `更多信息` 并确定继续运行。这是因为软件没有微软签名，不必担心。如果遇到提示需要管理员权限，请选择同意。

## 使用说明

见Windows BKnetwork使用说明

## Q&A

1. 免流模式真的能实现免流吗？

   - 使用 ipv6 不计费，北科只计算ipv4流量然后收费

2. Warp 免流模式有什么特点？

   - `Warp 免流模式`：网速快，延迟较低，steam和wegame可下载。需要安装 Cloudflare WARP 客户端，少数情况无法连接

3. 无法使用Warp免流

   - Warp 确实偶尔连不上，稍后再试（一般都可以的，可以重试个三次）
   - 类似 Mihomo/Clash TUN、VMware、Tailscale 和蓝牙的虚拟网卡可能抢占默认路由或 DNS。开启前请先关闭其他 VPN/TUN 模式；新版 BKNetwork 会显示 Cloudflare 实际识别到的冲突网卡，并在 WARP 连接失败时自动恢复双栈；新版BKNetwork支持了和Clash verge的系统代理模式同时使用，clash负责代理ChatGPT相关流量，其余流量由warp接管

4. 经验

   * Warp 免流模式会先等待 Cloudflare 自己的网络视图移除 IPv4，并在连接稳定期持续确认“目标物理网卡 + 仅 IPv6 外层”。如果 WARP 已连接但仍可能走 IPv4，界面只会显示普通 WARP，不会误报为免流成功
   * Cloudflare 偶尔会显示 `CF_HAPPY_EYEBALLS_MITM_FAILURE`。BKNetwork 会清理本轮旧隧道并自动开始下一轮，不再要求用户反复点击开关；全部失败时才恢复原协议和双栈
   * 关闭浏览器不会关闭后台或 WARP。重新从托盘打开页面后，页面会根据 Cloudflare 实际状态和物理网卡恢复 WARP 开关
   * 与 Clash Verge 共存时必须关闭 Clash TUN；否则 Mihomo 虚拟网卡可能被 Cloudflare 识别为外层网卡，WARP 免流校验会主动拒绝连接

   * 免流模式不能访问仅支持ipv4的网站（例如校园网登录页），但是放心，2026年了，仅支持ipv4的网站较少

   * 实时流量监控，推荐 [Sniffnet](https://sniffnet.net/)

5. 欢迎提 issue。或者先问问你的 ai 朋友

## 开发者指南

**构建二进制文件**

在项目根目录执行：

```bash
cd BKNetwork
go build -o bknetwork.exe ./cmd/bknetwork
```

当前目录会生成 `bknetwork.exe`。

在 Windows 上构建并打包为服务或分发给别的机器时，建议在与目标平台相同的环境中构建（比如使用带有相同 GOOS/GOARCH 的交叉编译或在目标 Windows 主机上构建）。

如果要连同最新前端一起发布，请使用仓库里的发布脚本，它会先同步 `web/` 再构建可分发目录和 zip 压缩包：

```bash
cd BKNetwork
.\scripts\build-release.ps1
```

或右键使用 powershell 运行。

`release/` 目录里会包含 `bknetwork.exe` 和最新的 `web/`，程序运行时会自动加载同步后的前端页面。

在非 Windows 平台上交叉编译 Windows x64 二进制文件：

```bash
# 在 Linux/macOS 环境交叉编译为 Windows amd64
GOOS=windows GOARCH=amd64 go build -o bknetwork.exe ./cmd/bknetwork
```

**以服务方式安装（Windows）**

生成 `bknetwork.exe` 后，使用管理员权限运行安装命令：

```bash
# 以管理员身份打开 PowerShell
.\bknetwork.exe install
.\bknetwork.exe start
```

程序使用 `github.com/kardianos/service` 做为服务包装，安装/启动/停止命令均由可执行文件暴露（参见 `cmd/bknetwork` 目录下的实现）

**HTTP / WebSocket 接口**

- 静态 Web UI：根路径（`/`）会提供 `web` 目录下的文件。
- REST 状态接口：`/api/v1/status` — 返回最近一次网络快照与服务状态。
- 控制接口：`/api/v1/switch`（切换 IPv4/IPv6）、`/api/v1/warp`（控制 warp-cli）、`/api/v1/chatgpt-proxy`（配置 ChatGPT → Clash PAC 分流）。
- PAC：`/api/v1/chatgpt-proxy.pac` — 仅供本机 Windows 系统代理读取。
- 实时事件：WebSocket 路径为 `/ws`，会发送 `hello`、`network.status`、`heartbeat` 等事件。

注：改变网络绑定或控制 `warp-cli` 的命令需要以管理员权限执行，接口会在权限不足或命令不可用时返回错误信息并通过 WebSocket 发布事件。

**常见问题与排查**

- 首次打开页面状态加载慢：服务在采集网络快照时会调用若干 PowerShell 和外部命令（如 `warp-cli`），可能耗时。建议在疑难排查时直接在服务器主机上运行 `go run ./cmd/bknetwork` 并观察输出日志。
- `warp-cli not found`：如果未安装 Cloudflare WARP 客户端，`/api/v1/warp` 会返回错误并在日志中给出提示。安装后确保 `warp-cli` 在 PATH 中可访问。
- 权限不足：修改网卡绑定等操作需要管理员权限，若在非管理员上下文运行会收到 403 或相应错误信息。

**开发与测试**

仓库包含部分单元测试，可用以下命令运行测试：

```bash
cd BKNetwork
go test ./...
```

**目录结构（相关）**

- `cmd/bknetwork` — 程序入口与平台相关的包装代码（服务安装、桌面集成等）
- `internal/handlers` — HTTP 处理器，包含网络快照采集、warp 控制等逻辑
- `internal/events` — 事件总线，用于将事件广播到 WebSocket 订阅者
- `web/` — 前端静态资源


## 免责声明

本软件仅供学习和研究使用，请勿用于任何非法用途。使用本软件产生的一切后果由用户自行承担。

具体包括但不限于：

- 本软件对因使用或无法使用而导致的任何直接、间接、特殊、偶然或后果性损害不承担责任，包括但不限于数据丢失、业务中断或利润损失。
- 开发者不保证本软件适用于任何特定目的，也不保证本软件完全没有缺陷或错误。
- 用户应当遵守相关法律法规使用本软件，不得利用本软件进行任何非法或违规操作。
- 用户应在遵守校园网及相关网络工具使用条款和规定的前提下使用本软件，开发者不对用户违规使用的行为及其后果负责。
- 因使用本软件而导致的账号被限制、设备被隔离或其他损失，开发者不承担任何责任。

使用本软件即表示你已充分理解并同意本免责声明的全部条款。

---

感谢支持！欢迎请我杯喝的QwQ

![image-20260720145024415](./README.assets/image-20260720145024415.png)

