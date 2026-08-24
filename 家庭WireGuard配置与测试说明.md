# BKNetwork 家庭 WireGuard 配置与测试说明

本文用于把 Windows 的 BKNetwork 接入现有 Ubuntu 家庭 WireGuard 服务器，作为 Cloudflare WARP 的替代网络模式。

## 当前状态

- Ubuntu 上现有的 Android WireGuard 客户端仍在正常使用。
- 截图中的新增 Windows Peer、生成 Windows 配置、导入 Windows WireGuard 等操作**尚未执行**。
- 因此当前服务器没有被本次操作改动，也不会影响 Android 客户端。
- Windows 将作为现有 `wg0` 接口下的第二个 Peer 接入；不要新建第二个 WireGuard 服务端，也不要重复监听端口。

## 已确认的服务器信息

| 项目 | 已确认值 |
| --- | --- |
| WireGuard 接口 | `wg0` |
| Ubuntu 服务端监听端口 | `51820/UDP` |
| 原 Android Peer IPv4 | `10.66.66.2/32` |
| 原 Android Peer IPv6 | `fd42:42:42::2/128` |
| WireGuard 服务端内网 IPv6 | `fd42:42:42::1/64` |
| 当前 Ubuntu 公网 IPv6 | `2409:8a20:f54:5a10:608e:637b:b2e:c9e6` |

> `wg show` 中 Android Peer 显示的 `:38466` 是 Android 客户端当时的临时源端口，不是服务端端口。Windows 配置中必须使用服务端监听端口 `51820`。

当前公网 IPv6 可能会随家庭网络重连而变化。若变化，需要同步更新 Windows 配置中的 `Endpoint`，或后续使用 IPv6 DDNS。

## 开始前的注意事项

- 不要将任何 WireGuard 私钥发送到聊天、截图或公开位置。
- 每个客户端必须使用独立的一对 WireGuard 密钥。
- 每个客户端必须有独立的服务端 `AllowedIPs` 地址。
- 以下示例暂定 Windows 使用 `10.66.66.3/32` 和 `fd42:42:42::3/128`；执行前先检查它们未被占用。
- 不建议执行 `wg-quick save wg0`，以免意外覆盖现有的 `PostUp`、`PostDown`、转发或 NAT 规则。
- 不要删除或修改现有 Android Peer。

## 一、Ubuntu：检查当前状态

在 Ubuntu 终端执行：

```bash
sudo wg show wg0
sudo wg show wg0 listen-port
sudo wg show wg0 allowed-ips
```

应确认监听端口为：

```text
51820
```

确认下列地址没有出现在 `allowed-ips` 的输出中：

```text
10.66.66.3/32
fd42:42:42::3/128
```

## 二、Ubuntu：生成 Windows 客户端密钥

以下命令会在当前目录生成 Windows 客户端的私钥与公钥。请在安全的目录中执行：

```bash
umask 077
wg genkey | tee bknetwork-windows.key | wg pubkey > bknetwork-windows.pub
chmod 600 bknetwork-windows.key
cat bknetwork-windows.pub
```

记录终端显示的公钥；它是后续添加到 Ubuntu 服务端的“Windows 客户端公钥”。

> `bknetwork-windows.key` 是私钥，只用于生成 Windows 配置文件，不要泄露。

## 三、Ubuntu：获取服务端公钥

执行：

```bash
sudo wg show wg0 public-key
```

记录输出；它是后续写入 Windows 配置的“服务端公钥”。

## 四、Ubuntu：新增 Windows Peer

先将下面命令中的 `<Windows客户端公钥>` 替换为第二步得到的内容，再执行：

```bash
sudo wg set wg0 peer "<Windows客户端公钥>" allowed-ips "10.66.66.3/32,fd42:42:42::3/128"
```

为了让 Ubuntu 重启后仍保留该 Peer，将相同信息追加到现有配置文件：

```bash
sudo nano /etc/wireguard/wg0.conf
```

在文件末尾添加：

```ini
# BKNetwork Windows client
[Peer]
PublicKey = <Windows客户端公钥>
AllowedIPs = 10.66.66.3/32, fd42:42:42::3/128
```

保存后确认：

```bash
sudo wg show wg0
```

输出中应同时存在原 Android Peer 和新的 Windows Peer。

## 五、生成 Windows WireGuard 配置文件

在 Ubuntu 当前目录创建一个临时配置文件：

```bash
nano bknetwork-windows.conf
```

写入以下内容，并替换两个占位值：

```ini
[Interface]
PrivateKey = <bknetwork-windows.key中的内容>
Address = 10.66.66.3/32, fd42:42:42::3/128
DNS = 1.1.1.1, 2606:4700:4700::1111

[Peer]
PublicKey = <Ubuntu服务端公钥>
Endpoint = [2409:8a20:f54:5a10:608e:637b:b2e:c9e6]:51820
AllowedIPs = 0.0.0.0/0, ::/0
PersistentKeepalive = 25
```

为避免私钥被其他账户读取：

```bash
chmod 600 bknetwork-windows.conf
```

将该文件通过安全方式传到 Windows；传输完成后，可从 Ubuntu 删除本地临时密钥和配置副本：

```bash
rm -f bknetwork-windows.key bknetwork-windows.pub bknetwork-windows.conf
```

> 删除命令只应在你确认 Windows 已安全保存并成功导入配置之后执行。

## 六、Windows：导入与 BKNetwork 测试

1. 安装或打开官方 WireGuard for Windows。
2. 导入 `bknetwork-windows.conf`。
3. 保持配置名称使用英文、数字、`-`、`_` 等 ASCII 字符，例如 `bknetwork-windows`。
4. 启动 BKNetwork，刷新“家庭网络 WireGuard”配置列表。
5. 选择 `bknetwork-windows`，再开启“家庭网络 WireGuard”。
6. 在 Ubuntu 终端观察握手：

```bash
watch -n 1 'sudo wg show wg0'
```

当 Windows 成功连接后，新 Peer 的 `latest handshake` 会更新，`transfer` 收发流量会增加。

## BKNetwork v2.0.0 的说明

- BKNetwork 不会将 WireGuard 服务端端口写死；实际连接端口以 Windows 导入的 `.conf` 文件中的 `Endpoint` 为准。
- 当前已构建的 v2.0.0 功能版本可用于测试。
- 但早期 UI/说明示例仍可能写有旧端口 `41580`，后续需要统一改为正确的 `51820` 并重新构建 v2.0.0 测试包。
- Windows 配置中的正确 Endpoint 形式为：

```ini
Endpoint = [2409:8a20:f54:5a10:608e:637b:b2e:c9e6]:51820
```

## 故障排查

### 没有握手

检查：

- Windows 配置中的服务端端口是否为 `51820`。
- IPv6 Endpoint 是否仍是当前 Ubuntu 公网 IPv6。
- Ubuntu 是否仍在监听 UDP `51820`：`sudo wg show wg0 listen-port`。
- Windows 客户端公钥是否已正确加入 `wg0`。
- Windows 与 Ubuntu 的 `AllowedIPs` 地址是否一致。

### 有握手但不能访问网络

握手只证明 Windows 与 Ubuntu 之间的加密链路可达，不代表 Ubuntu 已能替 Windows 转发互联网流量。依次检查：

- Windows 的 `[Peer]` 必须包含 `AllowedIPs = 0.0.0.0/0, ::/0`。仅写 `::/0` 时，BKNetwork 关闭物理网卡 IPv4 后，Windows 的 IPv4 流量没有出口。
- Windows 的 `[Interface]` 应设置隧道内 DNS，例如 `DNS = 1.1.1.1, 2606:4700:4700::1111`。
- Ubuntu 必须启用 `net.ipv4.ip_forward=1`；若要转发隧道内 IPv6，也应启用 `net.ipv6.conf.all.forwarding=1`。
- Ubuntu 防火墙必须允许 `wg0` 与实际出口网卡之间的 `FORWARD` 流量。
- Ubuntu 必须对 `10.66.66.0/24` 配置 IPv4 NAT/MASQUERADE；IPv6 则需要 NAT66 或可路由回客户端的前缀。

新版 BKNetwork 会在握手后检查双栈默认路由，并从 WireGuard 的 IPv4 地址实际连接公网和测试 Windows DNS。任一步失败都会停止隧道并恢复双栈，不再把“握手成功”误报为“可以上网”。

### 家庭网络重连后失效

检查 Ubuntu 的公网 IPv6 是否变化：

```bash
ip -6 addr show scope global
```

如果公网 IPv6 已变，请更新 Windows 配置中 `Endpoint` 的地址后重新导入或重启该隧道。
