package linuxnet

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type nmDevice struct {
	Name       string
	Type       string
	State      string
	Connection string
	Managed    bool
}

func (m *Manager) collectInterfaces(ctx context.Context) ([]Interface, error) {
	if m.interfaceProvider != nil {
		interfaces, err := m.interfaceProvider(ctx)
		if err != nil {
			return nil, err
		}
		copyInterfaces := append([]Interface(nil), interfaces...)
		for index := range copyInterfaces {
			copyInterfaces[index].IPv4 = append([]string(nil), copyInterfaces[index].IPv4...)
			copyInterfaces[index].IPv6 = append([]string(nil), copyInterfaces[index].IPv6...)
		}
		sort.Slice(copyInterfaces, func(i, j int) bool { return copyInterfaces[i].Name < copyInterfaces[j].Name })
		return copyInterfaces, nil
	}

	nmDevices := map[string]nmDevice{}
	var nmErr error
	if m.commandAvailable(m.commands.nmcli) {
		raw, err := m.run(ctx, m.commands.nmcli, "-t", "--escape", "no", "-f", "DEVICE,TYPE,STATE,CONNECTION", "device", "status")
		if err != nil {
			nmErr = fmt.Errorf("nmcli device status：%w", err)
		} else {
			nmDevices = parseNMDeviceStatus(raw)
		}
	} else {
		nmErr = fmt.Errorf("未找到 %s", m.commands.nmcli)
	}

	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	result := make([]Interface, 0, len(interfaces))
	for _, netInterface := range interfaces {
		name := strings.TrimSpace(netInterface.Name)
		if name == "" {
			continue
		}
		item := Interface{
			Name:     name,
			Type:     fallbackInterfaceType(netInterface),
			State:    "down",
			IPv4:     []string{},
			IPv6:     []string{},
			Physical: physicalInterface(name),
		}
		if netInterface.Flags&net.FlagUp != 0 {
			item.State = "up"
		}
		if device, found := nmDevices[name]; found {
			item.Type = fallbackText(device.Type, item.Type)
			item.State = normalizeNMState(device.State, item.State)
		}
		addresses, addrsErr := netInterface.Addrs()
		if addrsErr != nil {
			if nmErr != nil {
				return nil, fmt.Errorf("读取网卡 %s 地址失败（%v；%v）", name, addrsErr, nmErr)
			}
			return nil, fmt.Errorf("读取网卡 %s 地址失败：%w", name, addrsErr)
		}
		for _, address := range addresses {
			value := strings.TrimSpace(address.String())
			ip := parseAddress(value)
			if ip == nil {
				continue
			}
			if ip.To4() != nil {
				item.IPv4 = append(item.IPv4, value)
			} else {
				item.IPv6 = append(item.IPv6, value)
			}
		}
		sort.Strings(item.IPv4)
		sort.Strings(item.IPv6)
		result = append(result, item)
	}
	if nmErr != nil {
		// Netlink is still useful for read-only status, but Connect requires
		// NetworkManager data and will reject the interface later.
		return result, nmErr
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result, nil
}

func parseNMDeviceStatus(raw string) map[string]nmDevice {
	result := make(map[string]nmDevice)
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if line == "" {
			continue
		}
		fields := strings.Split(line, ":")
		if len(fields) < 4 {
			continue
		}
		name := strings.TrimSpace(fields[0])
		if name == "" {
			continue
		}
		result[name] = nmDevice{
			Name:       name,
			Type:       strings.TrimSpace(fields[1]),
			State:      strings.TrimSpace(fields[2]),
			Connection: strings.TrimSpace(strings.Join(fields[3:], ":")),
			Managed:    true,
		}
	}
	return result
}

func normalizeNMState(value, fallback string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	switch {
	case strings.HasPrefix(value, "connected"):
		return "connected"
	case strings.HasPrefix(value, "connecting"):
		return "connecting"
	case strings.HasPrefix(value, "disconnected"):
		return "down"
	case strings.HasPrefix(value, "unavailable"):
		return "down"
	case strings.HasPrefix(value, "deactivating"):
		return "down"
	default:
		return fallback
	}
}

func fallbackInterfaceType(iface net.Interface) string {
	if iface.Name == "lo" {
		return "loopback"
	}
	if iface.Flags&net.FlagLoopback != 0 {
		return "loopback"
	}
	if physicalInterface(iface.Name) {
		return "physical"
	}
	return "virtual"
}

func physicalInterface(name string) bool {
	if name == "" || name == "lo" {
		return false
	}
	// A physical Linux netdevice has a device symlink below sysfs.  Checking
	// the symlink target rather than the interface name avoids treating tun,
	// docker, bridge, and WireGuard devices as campus uplinks.
	info, err := os.Stat(filepath.Join("/sys/class/net", name, "device"))
	return err == nil && info != nil
}

func (m *Manager) activePhysicalIPv4(ctx context.Context, selected string) ([]string, error) {
	interfaces, err := m.collectInterfaces(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]string, 0)
	for _, iface := range interfaces {
		if !iface.Physical || iface.Name == selected || !interfaceStateUsable(iface.State) {
			continue
		}
		for _, address := range iface.IPv4 {
			ip := parseAddress(address)
			if ip != nil && ip.To4() != nil && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() {
				result = append(result, iface.Name+":"+address)
			}
		}
	}
	return result, nil
}
