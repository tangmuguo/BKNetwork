BKNetwork
=========

BKNetwork 是一个轻量级本地服务，带有内置 Web 管理界面，用于简化用户针对北科校园网络的部分管理，并提供免流功能。

**仅支持 Windows x64** 系统。仅在 Win11 测试，不保证支持 Win10 使用。

[Github:tangmuguo/BKNetwork](https://github.com/tangmuguo/BKNetwork)

## 下载与安装

在右侧栏 Releases 页面下载带版本号的 `.zip` 压缩包，解压后双击运行 `bknetwork.exe`，首次运行需要点击弹窗中 `更多信息` 并确定继续运行。这是因为软件没有微软签名，不必担心。如果遇到提示需要管理员权限，请选择同意。

## 使用说明

见Windows BKnetwork使用说明

## 家庭网络 WireGuard（可选）

如果家里的 Ubuntu 服务器提供可访问的公网 IPv6，且 UDP `51820` 已放行，可以用家庭 WireGuard 替代 Cloudflare WARP。先在 **官方 WireGuard for Windows** 导入客户端配置，在 BKNetwork 页面选择该隧道即可；程序不会读取或保存 WireGuard 私钥。客户端应使用 `Endpoint = [家庭公网IPv6]:51820`、`AllowedIPs = 0.0.0.0/0, ::/0`、隧道内 `DNS` 和 `PersistentKeepalive = 25`。Ubuntu 还需要正确配置 IPv4/IPv6 转发，以及 IPv4 NAT 和 NAT66/回程路由；只有开放 UDP 端口并不足够。

BKNetwork 会让 WARP 和家庭 WireGuard 互斥：开启家庭模式时会关闭 WARP 并切换所选物理网卡为仅 IPv6。隧道内部仍保留 IPv4/IPv6 双栈，访问 IPv4 网站时也经 IPv6 外层传输，保留校园 IPv6 不计费的使用方式。

为避免 Windows 的 `Forwarding/WeakHostSend` 导致外层报文重新进入隧道，程序会在启动前临时关闭所选物理网卡的这两个 IPv6 选项，启动后为每个 IPv6 Endpoint 添加经该物理 IPv6 网关的 `/128` 临时路由，并核对实际出口。只接受 IPv6 Endpoint；不会通过启用物理 IPv4 来修复连接。程序还会校验实际安装的双栈隧道路由、物理网卡保持仅 IPv6，并从隧道 IPv4 地址测试公网和 Windows DNS。未满足这些条件时不会报告免流成功。

断开、切换 WARP 或启动失败时，程序会先确认隧道停止，再删除本次添加的端点路由并恢复原 IPv6 转发选项。原状态保存在 `%APPDATA%\BKNetwork\home-routing.json`（不含密钥），支持程序重启后的恢复；已有路由和其他网卡不受这些修复操作影响。若隧道已经停止但端点清理失败，仍会恢复普通双栈，并保留记录、提示再次关闭重试，不会误报为完全恢复。这些保护在 **BKNetwork 的家庭网络开关** 中执行，单独点击官方 WireGuard 的连接按钮不会执行此流程。

## 与 quota-float 共用

从 v2.0.1 起，继续使用现有的 **ChatGPT → Clash Verge 分流** 开关和 Clash HTTP/mixed 地址，无需增加按钮或修改 quota-float。开启分流、启动 BKNetwork 或修改代理端口时，程序会为当前 Windows 用户、当前会话中正在运行的 `quota-float.exe` 单独设置 `HTTP_PROXY`、`HTTPS_PROXY`、`ALL_PROXY`，并重启一次，使额度请求使用同一个 Clash 出口。稍后才启动 quota-float 也会在约 3 秒内被检测和适配；用户主动退出额度工具后不会被自动打开。

这是因为 quota-float 的原生 HTTP 客户端只在启动时读取代理，不执行 ChatGPT 客户端使用的 PAC，而且把 HTTP 403 也显示为“登录失效”。适配只改变 quota-float 子进程的 HTTP/HTTPS 代理环境；其他应用继续遵循现有 PAC 分流，系统环境变量、quota-float 源码和 Codex 登录文件均不修改。额度工具会短暂重开，新实例通过 Windows 父进程属性继承原 quota-float 的权限，支持管理员 BKNetwork 与普通权限 quota-float 同时运行；原用户身份和权限保持不变。新实例无法准备成功时保留原实例，并在原卡片中显示失败原因。

关闭分流或从托盘正常退出 BKNetwork，会重新启动本次已适配且仍在运行的 quota-float，恢复该 Windows 用户的默认环境。恢复失败会重试一次；若仍失败，保留原实例并在卡片或日志提示完全退出、重新打开 quota-float，避免误报恢复成功。环境来自原用户的 Windows 环境配置；若需要自定义 `CODEX_HOME`，应配置为用户环境变量，而不是仅在某个启动终端中临时设置。适配用于交互式桌面会话，Windows 服务不会跨会话接管其他用户的额度工具。若代理适配成功后仍显示未登录，应再检查 Codex 自身登录状态；BKNetwork 不刷新或替换登录令牌。

## v2.0.2 资源占用与状态响应优化

本版保留现有开关、状态刷新周期、免流判定、路由保护和失败恢复流程，减少重复采集及后台资源开销：

- 完整快照将网卡信息、IPv4 和 IPv6 协议绑定合并到一次 PowerShell 调用，正常情况下从 3 个进程减少到 1 个；查询失败时仍回退到原有独立查询。
- 同时到达的 HTTP / WebSocket 状态请求共用在途采集，完成后不缓存；网络操作前后隔离在途结果，下一次刷新重新读取当前状态。
- 网页刷新合并重叠请求，并避免相同内容的重复渲染；WebSocket 断开后及时释放后台连接和事件订阅。
- quota-float 维持约 3 秒的发现周期；进程名直接使用 UTF-16 缓冲匹配，减少遍历系统进程时的临时字符串分配，仍逐个校验候选进程的身份和会话。
- WARP 连接成功仍要求“已连接 + IPv6 外层校验通过”连续满足，稳定窗口由 8 秒改为 5 秒；终态失败宽限由 4 秒改为 1 秒，Connecting / Checking / Configuring 继续等待，断开稳定窗口由 750 毫秒改为 500 毫秒。
- 页面快照中的 WARP status 和 `/api/v1/warp-status` 共用专用 4 秒读取超时；查询失败、超时或未返回有效状态时，页面保留上一份有效 WARP 状态，等后续成功读取再更新。
- 外部 IPv6 地址查询单次超时由 5 秒改为 2 秒，重试次数不变。

IPv6 外层等待仍为 12 秒、协议切换 18 秒、单轮 WARP 连接 24 秒、家庭 WireGuard 30 秒 / 15 秒；通用超时、重试次数、路由校验和协议回退逻辑保持不变。进程调用次数的减少不等同于整机 CPU 或内存的同比降幅，实际改善取决于打开的页面数量、操作频率及 WARP 等外部程序。

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

   * 单独使用“仅 IPv6”模式且未连接隧道时，无法访问仅支持 IPv4 的网站。WARP/家庭 WireGuard 成功建立隧道后，可以把内层 IPv4 流量封装在 IPv6 外层中；校园认证页面如需 IPv4，可先用普通双栈完成登录。

   * 实时流量监控，推荐 [Sniffnet](https://sniffnet.net/)

5. 欢迎提 issue。或者先问问你的 AI

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

`releases/bknetwork-v2.0.2/` 目录里会包含 `bknetwork.exe` 和最新的 `web/`，程序运行时会自动加载同步后的前端页面。

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
- 控制接口：`/api/v1/switch`（切换 IPv4/IPv6）、`/api/v1/warp`（控制 warp-cli）、`/api/v1/home-network`（控制官方客户端已导入的家庭 WireGuard 隧道）、`/api/v1/chatgpt-proxy`（配置 ChatGPT → Clash PAC 分流，并同步 quota-float 子进程代理）。
- PAC：`/api/v1/chatgpt-proxy.pac` — 仅供本机 Windows 系统代理读取。
- 实时事件：WebSocket 路径为 `/ws`，会发送 `hello`、`network.status`、`heartbeat` 等事件。

注：改变网络绑定或控制 `warp-cli` 的命令需要以管理员权限执行，接口会在权限不足或命令不可用时返回错误信息并通过 WebSocket 发布事件。

**常见问题与排查**

- 首次打开页面状态加载慢：服务会合并重复的网络快照请求，并批量读取网卡信息；外部命令（如 `warp-cli`）或系统网络查询仍可能耗时。可查看 `%APPDATA%\BKNetwork\bknetwork.log`；需要调试时，请先退出当前实例，再运行 `go run ./cmd/bknetwork`。
- `warp-cli not found`：如果未安装 Cloudflare WARP 客户端，`/api/v1/warp` 会返回错误并在日志中给出提示。安装后确保 `warp-cli` 在 PATH 中可访问。
- 权限不足：修改网卡绑定等操作需要管理员权限，若在非管理员上下文运行会收到 403 或相应错误信息。

**开发与测试**

仓库包含部分单元测试，可用以下命令运行测试：

```bash
cd BKNetwork
go test ./...
```

性能优化的离线回归可使用以下命令（不会启动 BKNetwork 主程序）：

```powershell
go test -race -p 2 -skip '^TestRestartProcess' ./...
go vet ./...
node --check web/app.js
node --test scripts/app.test.cjs
```

这里跳过会构建、启动临时进程的 `TestRestartProcess*` 集成测试；其余 Go 测试使用隔离资源，前端测试模拟网络请求及 DOM。完整 `go test ./...` 仍可按需运行临时进程的权限与恢复验证。

**目录结构（相关）**

- `cmd/bknetwork` — 程序入口与平台相关的包装代码（服务安装、桌面集成等）
- `internal/handlers` — HTTP 处理器，包含网络快照采集、warp 控制等逻辑
- `internal/quotafloat` — quota-float 的进程检测、代理环境隔离和重启恢复
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

