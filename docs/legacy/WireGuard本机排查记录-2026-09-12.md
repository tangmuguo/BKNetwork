# WireGuard 本机排查记录

排查时间：2026-09-12，Asia/Shanghai。对象：Windows 上的 `BKNetwork-ganlu` 隧道。

本次对运行环境仅做了只读检查，取得网络状态与 WireGuard 日志；随后按用户要求修改 BKNetwork 源码并进行离线测试。没有连接 WireGuard、切换 Clash、修改路由或网卡设置，也没有访问或修改 Ubuntu。用户需要继续使用 Clash，因此没有进行短时复现。临时采集脚本和日志副本在整理后删除；原始 WireGuard 日志仍保留在程序自己的 Data 目录。

## 结论与边界

最强证据是 WireGuard 自己记录的 **WLAN Forwarding/WeakHostSend 路由回环告警**。第一轮连接时，客户端确实检测到了会导致回环的本机状态；这与发送计数迅速增长、接收很少的现象高度吻合。

但不能据此认定两轮故障已经完全定位：截图对应的第二轮没有重复该告警，排查时 WLAN 的相关选项已全部为 Disabled，用户确认两轮之间仅断开重连、未手动修改这些选项。没有连接期间的路由快照、网卡计数对照或抓包，尚不能确认第二轮仍是同一回环，也不能确认是谁改变了相关状态。

## 已实现的源码修复

- 启动前保存所选物理网卡 IPv6 的原 Forwarding/WeakHostSend 状态，临时禁用并校验；不修改 IPv4 转发选项或其他网卡。
- 通过 `wg show <隧道> endpoints` 读取公开运行信息，拒绝 IPv4 Endpoint，为每个 IPv6 Endpoint 添加到所选物理 IPv6 网关的 `/128` 临时路由并校验真实出口。
- 继续关闭所选物理网卡 IPv4，保持隧道内双栈；核对实际 Windows 路由和物理网卡仅 IPv6，在联网验证后再次检查，保留校园 IPv6 免流路径。
- 原设置与本次新增路由保存在用户配置目录的 `BKNetwork/home-routing.json`，不含密钥；先记录再修改，支持程序重启、启动失败、停止与切换 WARP 后恢复。既有路由不纳入删除清单，新增路由用独立度量值标记，降低检查/添加之间其他程序插入普通路由时被误删的风险；接口 GUID 用于防止网卡索引复用导致误操作。
- 失败提示不再把“有发送无接收”优先归因 Ubuntu；自动恢复失败会保留记录并报告错误；已确认隧道停止但清理失败时，仍恢复普通双栈，允许再次关闭重试清理。

修复通过 BKNetwork 的家庭网络开关生效。官方 WireGuard 独立连接不经过此流程。离线测试执行真实生成的 PowerShell 脚本，但全部网络命令由假函数替代；覆盖状态恢复、路由竞争、缺失 IPv4 隧道路由、物理 IPv4 被重新开启等情况。按用户要求未进行实机断网复现，不能据此承诺截图第二轮的所有问题都已消除。

## 直接证据

日志来自官方命令 `wireguard.exe /dumplog`，未读取或导出私钥。

| 本地时间 | 观察到的事实 |
| --- | --- |
| 21:05:01.833595 | `Warning: the "WLAN" interface has Forwarding/WeakHostSend enabled, which will cause routing loops` |
| 21:05:01.859118 | 收到服务端握手响应。 |
| 21:05:17.631614 | 客户端因约 15 秒没有收到回包而重新握手。 |
| 21:05:58 至 21:06:30 | 服务端大约每 5 秒重新发起握手；客户端反复建立密钥对。 |
| 21:07:56 至 21:09:04 | 第二轮连接；21:07:57 收到握手响应，21:08:26 起服务端又约每 5 秒发起握手。该轮未记录上述 WLAN 告警。 |
| 21:14 及后续只读检查 | WLAN 的 IPv4、IPv6 `Forwarding`、`WeakHostSend`、`WeakHostReceive` 都是 Disabled；WireGuard 未连接。 |

截图记录：发送 5.89 GiB、接收 306.18 KiB、MTU 1280；IPv6 Endpoint；`AllowedIPs = 0.0.0.0/0, ::/0`。发送/接收是累计计数，不能从一个截图算出实际速率。

此前另一条 `bknetwork-windows` 隧道在 2026-08-26 13:28:18 也出现过相同 WLAN 告警。这增加了本机网络状态反复触发问题的可能性，但不能证明所有历史故障都具有同一原因。

## 为什么会出现“握手了却没有网络”

握手说明某些认证报文可以往返，不能证明业务数据持续可达。若外层 WireGuard UDP 报文又被送回隧道，会再次封装、继续计入发送量；计数可能迅速增加，而有效业务数据没有正常到达目的地。没有物理网卡或服务端的对应统计，不能把 5.89 GiB 当作已经正常发到公网的有效流量。

官方 Windows 客户端针对该问题检查 Endpoint 路由和外层接口的 `ForwardingEnabled || WeakHostSend`。这条告警有明确的客户端路由条件，并非笼统的联网失败提示。[WireGuard 检查源码](https://raw.githubusercontent.com/WireGuard/wireguard-windows/master/tunnel/pitfalls.go)

该检查在隧道接口配置时异步运行，不是持续的逐包回环检测。因此，第二轮没有告警本身不足以证明不存在回环；也不能据此宣称第二轮一定存在回环。[WireGuard 检查调用时机](https://raw.githubusercontent.com/WireGuard/wireguard-windows/master/tunnel/interfacewatcher.go)

截图中的单 Peer 双栈 `/0` 配置还会启用 Windows WireGuard 的防泄漏规则。隧道无法正常传输时，普通网络也不能自动成为备用出口，表现为整机无网。这解释了断网表现，不代表防泄漏机制本身是故障根因。[WireGuard 官方网络说明](https://git.zx2c4.com/wireguard-windows/about/docs/netquirk.md)

## 后续验证与处理方向

1. 在以后允许短时断网时，比较连接前后 WLAN IPv6 的 `Forwarding`、`WeakHostSend`，并同步采集 Endpoint 路由、WireGuard 与 WLAN 的发送增量。当前端点使用 IPv6，外层 IPv6 状态尤其相关。
2. 如果故障现场这些选项变成 Enabled，针对实际外层接口关闭相关转发/弱主机选项，再验证是否恢复；同时追踪是哪个共享、桥接或网络组件重新启用了它们。当前它们已是 Disabled，重复修改不能算完成修复。
3. 如果这些选项保持 Disabled 而故障仍出现，再比较客户端物理网卡与服务端的 UDP 收发，以区分其他本机回环/过滤问题、传输路径问题和服务端数据转发问题。

用于核对选项的只读命令：

```powershell
Get-NetIPInterface -InterfaceAlias 'WLAN' |
    Format-Table AddressFamily,Forwarding,WeakHostSend,WeakHostReceive
```

本次没有证据支持将故障归因于 Clash；当前正在运行的 Clash 状态不代表出故障时的状态。也没有证据足以判定 Ubuntu 配置错误。实机联网效果尚未验证。
