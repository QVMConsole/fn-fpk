package app

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	networkCompatibilityMode = "libvirt"
	directBridgeMode         = "bridge"
	managedNetworkName       = "qvmconsole-fnos"
	managedBridgeName        = "virbr-qvmc"
	ovsDNSMasqService        = "kvm-console-ovs-dnsmasq.service"
	bridgeRestoreService     = "kvm-console-bridges.service"
	directBridgeMapName      = "direct-bridges.tsv"
	vpcVLANMapName           = "vpc-vlans.tsv"
	vpcRuntimeBridgeMapName  = "vpc-runtime-bridges.tsv"
	legacyDomainMinAge       = time.Minute
)

var (
	networkCompatibilityMu sync.Mutex
	interfaceBlockPattern  = regexp.MustCompile(`(?s)<interface\b.*?</interface>`)
	virtualPortPattern     = regexp.MustCompile(`(?s)\s*<virtualport\b[^>]*(?:/>|>.*?</virtualport>)`)
	vlanPattern            = regexp.MustCompile(`(?s)\s*<vlan\b[^>]*>.*?</vlan>`)
	interfaceTypePattern   = regexp.MustCompile(`type=(?:'(?:bridge|direct)'|"(?:bridge|direct)")`)
	sourceElementPattern   = regexp.MustCompile(`<source\b[^>]*/>`)
	singleBridgePattern    = regexp.MustCompile(`bridge='[^']*'`)
	doubleBridgePattern    = regexp.MustCompile(`bridge="[^"]*"`)
	interfaceNamePattern   = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,63}$`)
)

// NetworkCompatibilityState 描述飞牛网络兼容层的最近一次执行结果。
type NetworkCompatibilityState struct {
	Enabled        bool      `json:"enabled"`
	Mode           string    `json:"mode,omitempty"`
	Network        string    `json:"network,omitempty"`
	Bridge         string    `json:"bridge,omitempty"`
	RepairedVMs    []string  `json:"repairedVMs,omitempty"`
	PendingRestart []string  `json:"pendingRestart,omitempty"`
	Errors         []string  `json:"errors,omitempty"`
	UpdatedAt      time.Time `json:"updatedAt,omitempty"`
}

type libvirtNetworkXML struct {
	Name    string `xml:"name"`
	Forward struct {
		Mode string `xml:"mode,attr"`
	} `xml:"forward"`
	Bridge struct {
		Name string `xml:"name,attr"`
	} `xml:"bridge"`
}

type domainXML struct {
	Devices struct {
		Interfaces []domainInterfaceXML `xml:"interface"`
		Disks      []domainDiskXML      `xml:"disk"`
	} `xml:"devices"`
}

type domainDiskXML struct {
	Device string `xml:"device,attr"`
	Source struct {
		File string `xml:"file,attr"`
	} `xml:"source"`
}

type domainInterfaceXML struct {
	Type string `xml:"type,attr"`
	MAC  struct {
		Address string `xml:"address,attr"`
	} `xml:"mac"`
	Source struct {
		Bridge string `xml:"bridge,attr"`
		Dev    string `xml:"dev,attr"`
		Mode   string `xml:"mode,attr"`
	} `xml:"source"`
	VirtualPort struct {
		Type string `xml:"type,attr"`
	} `xml:"virtualport"`
}

type vpcBinding struct {
	VMName           string
	InterfaceOrder   int
	NICModel         string
	BridgeName       string
	BridgeMode       string
	UplinkIF         string
	OriginalUplinkIF string
	VLANID           int
}

type directBridgeMapping struct {
	BridgeName string
	UplinkIF   string
}

// EnsureNetworkCompatibility 配置飞牛可稳定保留的 libvirt NAT 网络，并修复 VPC 网卡定义。
func EnsureNetworkCompatibility(ctx context.Context, cfg Config) (state NetworkCompatibilityState, err error) {
	networkCompatibilityMu.Lock()
	defer networkCompatibilityMu.Unlock()

	state.UpdatedAt = time.Now()
	defer func() {
		state.RepairedVMs = uniqueSorted(state.RepairedVMs)
		state.PendingRestart = uniqueSorted(state.PendingRestart)
		state.Errors = uniqueSorted(state.Errors)
		if err != nil {
			state.Errors = uniqueSorted(append(state.Errors, err.Error()))
		}
		_ = saveNetworkCompatibilityState(cfg, state)
	}()

	if runtime.GOOS != "linux" || !fileExists(cfg.EnvPath()) {
		return state, nil
	}
	if os.Geteuid() != 0 {
		return state, errors.New("配置飞牛网络兼容层需要 root 权限")
	}
	if !isFNOSNetworkService(ctx, cfg) {
		return state, nil
	}
	for _, command := range []string{"virsh", "systemctl"} {
		if _, lookupErr := exec.LookPath(command); lookupErr != nil {
			return state, fmt.Errorf("配置飞牛网络兼容层缺少命令: %s", command)
		}
	}

	state.Enabled = true
	state.Mode = networkCompatibilityMode

	envChanged := readEnvValue(cfg.EnvPath(), "KVM_NETWORK_BACKEND") != networkCompatibilityMode
	if envChanged {
		if backupErr := backupNetworkEnv(cfg); backupErr != nil {
			return state, backupErr
		}
		if replaceErr := replaceEnvValue(cfg.EnvPath(), "KVM_NETWORK_BACKEND", networkCompatibilityMode); replaceErr != nil {
			return state, fmt.Errorf("写入 libvirt 网络兼容配置失败: %w", replaceErr)
		}
	}

	// QVMConsole 的 OVS dnsmasq 与 libvirt NAT 使用相同网关端口，必须先停用。
	_, _ = commandOutput(ctx, "systemctl", "disable", "--now", ovsDNSMasqService)

	network, bridge, networkErr := ensureLibvirtNATNetwork(ctx, cfg)
	if networkErr != nil {
		return state, networkErr
	}
	state.Network = network
	state.Bridge = bridge
	bindings, bindingErr := loadVPCBindings(ctx, cfg)
	if bindingErr != nil {
		return state, bindingErr
	}
	directBridges, mappingErr := loadDirectBridgeMappings(ctx, cfg)
	if mappingErr != nil {
		return state, mappingErr
	}
	commandCompatChanged, commandCompatErr := installNetworkCommandCompatibility(ctx, cfg, bridge, directBridges, bindings)
	if commandCompatErr != nil {
		return state, commandCompatErr
	}

	if (envChanged || commandCompatChanged) && serviceActive(ctx, cfg.ServiceName) {
		if restartErr := controlService(ctx, cfg, "restart"); restartErr != nil {
			return state, fmt.Errorf("启用 libvirt 网络兼容模式后重启 QVMConsole 失败: %w", restartErr)
		}
	}

	repaired, pending, reconcileErrors := reconcileVPCBindings(ctx, cfg, bridge, bindings)
	state.RepairedVMs = append(state.RepairedVMs, repaired...)
	state.PendingRestart = append(state.PendingRestart, pending...)
	state.Errors = append(state.Errors, reconcileErrors...)
	return state, nil
}

// StartNetworkCompatibilityMonitor 持续修复后续由 QVMConsole 新增的逻辑 VPC 绑定。
func StartNetworkCompatibilityMonitor(ctx context.Context, cfg Config, logf func(string, ...any)) {
	go func() {
		run := func() {
			runCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
			state, runErr := EnsureNetworkCompatibility(runCtx, cfg)
			cancel()
			if runErr != nil {
				logf("飞牛网络兼容检查失败: %v", runErr)
				return
			}
			if len(state.RepairedVMs) > 0 {
				logf("飞牛网络兼容层已修复虚拟机网卡: %s", strings.Join(state.RepairedVMs, ", "))
			}
			if len(state.PendingRestart) > 0 {
				logf("以下虚拟机需要重启后启用兼容网卡: %s", strings.Join(state.PendingRestart, ", "))
			}
		}

		run()
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				run()
			}
		}
	}()
}

func isFNOSNetworkService(ctx context.Context, cfg Config) bool {
	if fileExists(filepath.Join(cfg.SystemRoot, "usr", "trim", "bin", "network_service")) {
		return true
	}
	return serviceActive(ctx, "network_service.service")
}

func ensureLibvirtNATNetwork(ctx context.Context, cfg Config) (string, string, error) {
	namesOutput, err := commandOutput(ctx, "virsh", "net-list", "--all", "--name")
	if err != nil {
		return "", "", fmt.Errorf("读取 libvirt 网络失败: %s", namesOutput)
	}
	names := strings.Fields(namesOutput)
	sort.SliceStable(names, func(i, j int) bool {
		return networkPreference(names[i]) < networkPreference(names[j])
	})
	for _, name := range names {
		network, parseErr := readLibvirtNetwork(ctx, name)
		if parseErr == nil && network.Forward.Mode == "nat" && network.Bridge.Name != "" {
			if startErr := activateLibvirtNetwork(ctx, name); startErr != nil {
				return "", "", startErr
			}
			return name, network.Bridge.Name, nil
		}
	}

	prefix, prefixErr := selectNATPrefix(ctx)
	if prefixErr != nil {
		return "", "", prefixErr
	}
	address := prefix.Addr()
	gateway := address.Next()
	start := gateway.Next()
	end := netip.AddrFrom4([4]byte{address.As4()[0], address.As4()[1], address.As4()[2], 254})
	networkXML := fmt.Sprintf(`<network>
  <name>%s</name>
  <forward mode='nat'/>
  <bridge name='%s' stp='on' delay='0'/>
  <ip address='%s' netmask='255.255.255.0'>
    <dhcp><range start='%s' end='%s'/></dhcp>
  </ip>
</network>
`, managedNetworkName, managedBridgeName, gateway, start, end)
	path := filepath.Join(cfg.VarDir, "state", "network-compat.xml")
	if writeErr := os.WriteFile(path, []byte(networkXML), 0o600); writeErr != nil {
		return "", "", fmt.Errorf("写入 libvirt 网络定义失败: %w", writeErr)
	}
	defer os.Remove(path)
	if output, defineErr := commandOutput(ctx, "virsh", "net-define", path); defineErr != nil {
		return "", "", fmt.Errorf("定义 libvirt NAT 网络失败: %s", output)
	}
	if startErr := activateLibvirtNetwork(ctx, managedNetworkName); startErr != nil {
		return "", "", startErr
	}
	return managedNetworkName, managedBridgeName, nil
}

func networkPreference(name string) int {
	switch name {
	case "default":
		return 0
	case managedNetworkName:
		return 1
	default:
		return 2
	}
}

func readLibvirtNetwork(ctx context.Context, name string) (libvirtNetworkXML, error) {
	var network libvirtNetworkXML
	output, err := commandOutput(ctx, "virsh", "net-dumpxml", name)
	if err != nil {
		return network, err
	}
	if err := xml.Unmarshal([]byte(output), &network); err != nil {
		return network, err
	}
	return network, nil
}

func activateLibvirtNetwork(ctx context.Context, name string) error {
	if output, err := commandOutput(ctx, "virsh", "net-autostart", name); err != nil {
		return fmt.Errorf("启用 libvirt 网络自启动失败: %s", output)
	}
	activeOutput, err := commandOutput(ctx, "virsh", "net-list", "--name")
	if err != nil {
		return fmt.Errorf("读取活动 libvirt 网络失败: %s", activeOutput)
	}
	for _, active := range strings.Fields(activeOutput) {
		if active == name {
			return nil
		}
	}
	if output, err := commandOutput(ctx, "virsh", "net-start", name); err != nil {
		return fmt.Errorf("启动 libvirt NAT 网络失败: %s", output)
	}
	return nil
}

func selectNATPrefix(ctx context.Context) (netip.Prefix, error) {
	routesOutput, _ := commandOutput(ctx, "ip", "-4", "route", "show")
	var routes []netip.Prefix
	for _, line := range strings.Split(routesOutput, "\n") {
		field := strings.Fields(line)
		if len(field) == 0 || field[0] == "default" {
			continue
		}
		if prefix, parseErr := netip.ParsePrefix(field[0]); parseErr == nil {
			routes = append(routes, prefix.Masked())
		}
	}
	for second := 20; second <= 29; second++ {
		for third := 0; third <= 255; third++ {
			prefix := netip.PrefixFrom(netip.AddrFrom4([4]byte{172, byte(second), byte(third), 0}), 24)
			if !prefixOverlaps(prefix, routes) {
				return prefix, nil
			}
		}
	}
	return netip.Prefix{}, errors.New("没有可用于虚拟机 NAT 的空闲私有网段")
}

func prefixOverlaps(candidate netip.Prefix, existing []netip.Prefix) bool {
	for _, prefix := range existing {
		if prefix.Contains(candidate.Addr()) || candidate.Contains(prefix.Addr()) {
			return true
		}
	}
	return false
}

func reconcileVPCBindings(ctx context.Context, cfg Config, bridge string, bindings []vpcBinding) ([]string, []string, []string) {
	grouped := make(map[string][]vpcBinding)
	for _, binding := range bindings {
		grouped[binding.VMName] = append(grouped[binding.VMName], binding)
	}
	var repaired, pending, failures []string
	for vmName, vmBindings := range grouped {
		changed, needsRestart, repairErr := reconcileVMInterfaces(ctx, cfg, vmName, vmBindings, bridge)
		if repairErr != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", vmName, repairErr))
			continue
		}
		if changed {
			repaired = append(repaired, vmName)
		}
		if needsRestart {
			pending = append(pending, vmName)
		}
	}

	vmNames, listErr := listDomainNames(ctx)
	if listErr != nil {
		failures = append(failures, listErr.Error())
		return repaired, pending, failures
	}
	ovsBridge := strings.TrimSpace(readEnvValue(cfg.EnvPath(), "KVM_OVS_BRIDGE"))
	if ovsBridge == "" {
		ovsBridge = "br-ovs"
	}
	for _, vmName := range vmNames {
		changed, needsRestart, repairErr := reconcileLegacyOVSInterfaces(ctx, cfg, vmName, ovsBridge, bridge)
		if repairErr != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", vmName, repairErr))
			continue
		}
		if changed {
			repaired = append(repaired, vmName)
		}
		if needsRestart {
			pending = append(pending, vmName)
		}
	}
	return repaired, pending, failures
}

func loadVPCBindings(ctx context.Context, cfg Config) ([]vpcBinding, error) {
	if !fileExists(cfg.DatabasePath()) {
		return nil, nil
	}
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(cfg.DatabasePath())+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	rows, err := db.QueryContext(ctx, `
		SELECT b.vm_name,
		       b.interface_order,
		       COALESCE(NULLIF(b.nic_model, ''), 'virtio'),
		       COALESCE(s.bridge_name, ''),
		       COALESCE(s.bridge_mode, ''),
		       COALESCE(n.uplink_if, ''),
		       COALESCE(
		         CASE
		           WHEN lower(trim(COALESCE(s.bridge_mode, ''))) = 'bridge'
		             THEN NULLIF(s.bridge_vlan_id, 0)
		           ELSE NULLIF(s.vlan_id, 0)
		         END,
		         0
		       )
		FROM vpc_vm_bindings b
		LEFT JOIN vpc_switches s ON s.id = b.switch_id
		LEFT JOIN network_bridges n ON n.name = s.bridge_name
		ORDER BY b.vm_name, b.interface_order`)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "no such table") {
			return nil, nil
		}
		return nil, fmt.Errorf("读取 VPC 网卡绑定失败: %w", err)
	}
	defer rows.Close()
	var bindings []vpcBinding
	for rows.Next() {
		var binding vpcBinding
		if err := rows.Scan(
			&binding.VMName,
			&binding.InterfaceOrder,
			&binding.NICModel,
			&binding.BridgeName,
			&binding.BridgeMode,
			&binding.UplinkIF,
			&binding.VLANID,
		); err != nil {
			return nil, err
		}
		binding.OriginalUplinkIF = strings.TrimSpace(binding.UplinkIF)
		binding.UplinkIF = resolveDirectBridgeParent(ctx, binding.OriginalUplinkIF)
		if strings.TrimSpace(binding.VMName) != "" && binding.InterfaceOrder >= 0 {
			bindings = append(bindings, binding)
		}
	}
	return bindings, rows.Err()
}

func loadDirectBridgeMappings(ctx context.Context, cfg Config) ([]directBridgeMapping, error) {
	if !fileExists(cfg.DatabasePath()) {
		return nil, nil
	}
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(cfg.DatabasePath())+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	rows, err := db.QueryContext(ctx, `
		SELECT name, uplink_if
		FROM network_bridges
		WHERE lower(trim(mode)) = 'bridge'
		  AND trim(name) <> ''
		  AND trim(uplink_if) <> ''
		ORDER BY name`)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "no such table") {
			return nil, nil
		}
		return nil, fmt.Errorf("读取桥接网卡映射失败: %w", err)
	}
	defer rows.Close()
	var mappings []directBridgeMapping
	for rows.Next() {
		var mapping directBridgeMapping
		if err := rows.Scan(&mapping.BridgeName, &mapping.UplinkIF); err != nil {
			return nil, err
		}
		mapping.BridgeName = strings.TrimSpace(mapping.BridgeName)
		mapping.UplinkIF = resolveDirectBridgeParent(ctx, strings.TrimSpace(mapping.UplinkIF))
		if interfaceNamePattern.MatchString(mapping.BridgeName) && interfaceNamePattern.MatchString(mapping.UplinkIF) {
			mappings = append(mappings, mapping)
		}
	}
	return mappings, rows.Err()
}

func resolveDirectBridgeParent(ctx context.Context, interfaceName string) string {
	interfaceName = strings.TrimSpace(interfaceName)
	if !interfaceNamePattern.MatchString(interfaceName) {
		return interfaceName
	}
	output, err := commandOutput(ctx, "ovs-vsctl", "port-to-br", interfaceName)
	if err != nil {
		return interfaceName
	}
	bridge := strings.TrimSpace(output)
	if !interfaceNamePattern.MatchString(bridge) {
		return interfaceName
	}
	return bridge
}

func reconcileVMInterfaces(ctx context.Context, cfg Config, vmName string, bindings []vpcBinding, bridge string) (bool, bool, error) {
	inactiveXML, err := virshDomainXML(ctx, vmName, true)
	if err != nil {
		return false, false, err
	}
	interfaces, err := parseDomainInterfaces(inactiveXML)
	if err != nil {
		return false, false, err
	}
	desiredBindings := make(map[int]vpcBinding)
	for _, binding := range bindings {
		desiredBindings[binding.InterfaceOrder] = binding
	}

	changed := false
	needsRestart := false
	rewritten, rewriteCount := rewriteManagedInterfaces(inactiveXML, bridge, desiredBindings)
	if rewriteCount > 0 {
		if err := defineDomainXML(ctx, cfg, vmName, rewritten); err != nil {
			return false, false, err
		}
		changed = true
		if domainRunning(ctx, vmName) {
			needsRestart = true
		}
		inactiveXML = rewritten
		interfaces, _ = parseDomainInterfaces(inactiveXML)
	}

	liveInterfaces := interfaces
	running := domainRunning(ctx, vmName)
	if running {
		if liveXML, liveErr := virshDomainXML(ctx, vmName, false); liveErr == nil {
			liveInterfaces, _ = parseDomainInterfaces(liveXML)
		}
	}

	for _, binding := range bindings {
		if binding.InterfaceOrder < len(interfaces) {
			iface := interfaces[binding.InterfaceOrder]
			if interfaceMatchesBinding(iface, binding, bridge) {
				if running && (binding.InterfaceOrder >= len(liveInterfaces) || !interfaceMatchesBinding(liveInterfaces[binding.InterfaceOrder], binding, bridge)) {
					needsRestart = true
				}
				continue
			}
			// 非 OVS 的直通或用户自定义网卡不由兼容层覆盖。
			continue
		}
		xmlText := buildCompatibleInterfaceXML(vmName, binding, bridge)
		path := filepath.Join(cfg.VarDir, "state", fmt.Sprintf("network-%s-%d.xml", safeFilePart(vmName), binding.InterfaceOrder))
		if err := os.WriteFile(path, []byte(xmlText), 0o600); err != nil {
			return changed, needsRestart, err
		}
		if output, attachErr := commandOutput(ctx, "virsh", "attach-device", vmName, path, "--config"); attachErr != nil {
			_ = os.Remove(path)
			return changed, needsRestart, fmt.Errorf("写入第 %d 个持久网卡失败: %s", binding.InterfaceOrder+1, output)
		}
		changed = true
		interfaces = append(interfaces, expectedDomainInterface(binding, bridge))
		if running && binding.InterfaceOrder >= len(liveInterfaces) {
			if _, liveErr := commandOutput(ctx, "virsh", "attach-device", vmName, path, "--live"); liveErr != nil {
				needsRestart = true
			} else {
				liveInterfaces = append(liveInterfaces, expectedDomainInterface(binding, bridge))
			}
		}
		_ = os.Remove(path)
	}
	return changed, needsRestart, nil
}

func listDomainNames(ctx context.Context) ([]string, error) {
	output, err := commandOutput(ctx, "virsh", "list", "--all", "--name")
	if err != nil {
		return nil, fmt.Errorf("读取 libvirt 虚拟机列表失败: %s", output)
	}
	var names []string
	for _, line := range strings.Split(output, "\n") {
		if name := strings.TrimSpace(line); name != "" {
			names = append(names, name)
		}
	}
	return uniqueSorted(names), nil
}

func reconcileLegacyOVSInterfaces(ctx context.Context, cfg Config, vmName, ovsBridge, bridge string) (bool, bool, error) {
	inactiveXML, err := virshDomainXML(ctx, vmName, true)
	if err != nil {
		return false, false, err
	}
	rewritten, rewriteCount := rewriteLegacyOVSInterfaces(inactiveXML, ovsBridge, bridge)
	if rewriteCount == 0 {
		return false, false, nil
	}
	stable, stableErr := legacyDomainStable(inactiveXML, time.Now(), legacyDomainMinAge)
	if stableErr != nil {
		return false, false, stableErr
	}
	if !stable {
		return false, false, nil
	}
	if err := defineDomainXML(ctx, cfg, vmName, rewritten); err != nil {
		return false, false, err
	}
	return true, domainRunning(ctx, vmName), nil
}

func legacyDomainStable(xmlText string, now time.Time, minimumAge time.Duration) (bool, error) {
	var domain domainXML
	if err := xml.Unmarshal([]byte(xmlText), &domain); err != nil {
		return false, fmt.Errorf("解析虚拟机 XML 失败: %w", err)
	}
	for _, disk := range domain.Devices.Disks {
		if disk.Device != "disk" || strings.TrimSpace(disk.Source.File) == "" {
			continue
		}
		info, err := os.Stat(disk.Source.File)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return false, nil
			}
			return false, fmt.Errorf("检查虚拟机磁盘失败: %w", err)
		}
		if now.Sub(info.ModTime()) < minimumAge {
			return false, nil
		}
	}
	return true, nil
}

func virshDomainXML(ctx context.Context, vmName string, inactive bool) (string, error) {
	args := []string{"dumpxml", vmName}
	if inactive {
		args = append(args, "--inactive")
	}
	output, err := commandOutput(ctx, "virsh", args...)
	if err != nil {
		return "", fmt.Errorf("读取虚拟机 XML 失败: %s", output)
	}
	return output, nil
}

func parseDomainInterfaces(xmlText string) ([]domainInterfaceXML, error) {
	var domain domainXML
	if err := xml.Unmarshal([]byte(xmlText), &domain); err != nil {
		return nil, fmt.Errorf("解析虚拟机 XML 失败: %w", err)
	}
	return domain.Devices.Interfaces, nil
}

func rewriteManagedInterfaces(xmlText, bridge string, desiredBindings map[int]vpcBinding) (string, int) {
	indexes := interfaceBlockPattern.FindAllStringIndex(xmlText, -1)
	if len(indexes) == 0 {
		return xmlText, 0
	}
	var output strings.Builder
	last := 0
	changed := 0
	for order, index := range indexes {
		output.WriteString(xmlText[last:index[0]])
		block := xmlText[index[0]:index[1]]
		if binding, ok := desiredBindings[order]; ok {
			updated := rewriteInterfaceBlock(block, binding, bridge)
			if updated != block {
				block = updated
				changed++
			}
		}
		output.WriteString(block)
		last = index[1]
	}
	output.WriteString(xmlText[last:])
	return output.String(), changed
}

func rewriteLegacyOVSInterfaces(xmlText, ovsBridge, bridge string) (string, int) {
	if strings.TrimSpace(ovsBridge) == "" || strings.TrimSpace(bridge) == "" || ovsBridge == bridge {
		return xmlText, 0
	}
	indexes := interfaceBlockPattern.FindAllStringIndex(xmlText, -1)
	if len(indexes) == 0 {
		return xmlText, 0
	}
	var output strings.Builder
	last := 0
	changed := 0
	for _, index := range indexes {
		output.WriteString(xmlText[last:index[0]])
		block := xmlText[index[0]:index[1]]
		if interfaceUsesBridge(block, ovsBridge) {
			updated := rewriteInterfaceBlock(block, vpcBinding{}, bridge)
			if updated != block {
				block = updated
				changed++
			}
		}
		output.WriteString(block)
		last = index[1]
	}
	output.WriteString(xmlText[last:])
	return output.String(), changed
}

func rewriteInterfaceBlock(block string, binding vpcBinding, bridge string) string {
	if bindingUsesDirectBridge(binding) {
		if !strings.Contains(block, "openvswitch") && !interfaceUsesBridge(block, binding.BridgeName) && !interfaceUsesDirectDevice(block, binding.UplinkIF) && !interfaceUsesDirectDevice(block, binding.OriginalUplinkIF) {
			return block
		}
		block = virtualPortPattern.ReplaceAllString(block, "")
		block = vlanPattern.ReplaceAllString(block, "")
		block = interfaceTypePattern.ReplaceAllString(block, "type='direct'")
		return sourceElementPattern.ReplaceAllString(block, "<source dev='"+binding.UplinkIF+"' mode='bridge'/>")
	}
	block = virtualPortPattern.ReplaceAllString(block, "")
	block = vlanPattern.ReplaceAllString(block, "")
	block = interfaceTypePattern.ReplaceAllString(block, "type='bridge'")
	return sourceElementPattern.ReplaceAllString(block, "<source bridge='"+bridge+"'/>")
}

func interfaceUsesBridge(block, bridge string) bool {
	if bridge == "" {
		return false
	}
	return strings.Contains(block, "bridge='"+bridge+"'") || strings.Contains(block, `bridge="`+bridge+`"`)
}

func interfaceUsesDirectDevice(block, device string) bool {
	if device == "" {
		return false
	}
	return strings.Contains(block, "dev='"+device+"'") || strings.Contains(block, `dev="`+device+`"`)
}

func bindingUsesDirectBridge(binding vpcBinding) bool {
	return strings.EqualFold(strings.TrimSpace(binding.BridgeMode), directBridgeMode) && interfaceNamePattern.MatchString(strings.TrimSpace(binding.UplinkIF))
}

func interfaceMatchesBinding(iface domainInterfaceXML, binding vpcBinding, bridge string) bool {
	if bindingUsesDirectBridge(binding) {
		return iface.Type == "direct" && iface.Source.Dev == binding.UplinkIF && iface.Source.Mode == directBridgeMode
	}
	return iface.Type == "bridge" && iface.Source.Bridge == bridge && iface.VirtualPort.Type != "openvswitch"
}

func expectedDomainInterface(binding vpcBinding, bridge string) domainInterfaceXML {
	var iface domainInterfaceXML
	if bindingUsesDirectBridge(binding) {
		iface.Type = "direct"
		iface.Source.Dev = binding.UplinkIF
		iface.Source.Mode = directBridgeMode
		return iface
	}
	iface.Type = "bridge"
	iface.Source.Bridge = bridge
	return iface
}

func defineDomainXML(ctx context.Context, cfg Config, vmName, xmlText string) error {
	path := filepath.Join(cfg.VarDir, "state", "domain-"+safeFilePart(vmName)+".xml")
	if err := os.WriteFile(path, []byte(xmlText), 0o600); err != nil {
		return err
	}
	defer os.Remove(path)
	if output, err := commandOutput(ctx, "virsh", "define", path); err != nil {
		return fmt.Errorf("更新虚拟机持久网络配置失败: %s", output)
	}
	return nil
}

func domainRunning(ctx context.Context, vmName string) bool {
	output, err := commandOutput(ctx, "virsh", "domstate", vmName)
	return err == nil && strings.TrimSpace(output) == "running"
}

func buildCompatibleInterfaceXML(vmName string, binding vpcBinding, bridge string) string {
	model := strings.TrimSpace(binding.NICModel)
	if model == "" {
		model = "virtio"
	}
	if bindingUsesDirectBridge(binding) {
		return fmt.Sprintf(`<interface type='direct'>
  <mac address='%s'/>
  <source dev='%s' mode='bridge'/>
  <model type='%s'/>
</interface>
`, deterministicMAC(vmName, binding.InterfaceOrder), binding.UplinkIF, model)
	}
	return fmt.Sprintf(`<interface type='bridge'>
  <mac address='%s'/>
  <source bridge='%s'/>
  <model type='%s'/>
</interface>
`, deterministicMAC(vmName, binding.InterfaceOrder), bridge, model)
}

func deterministicMAC(vmName string, order int) string {
	digest := sha256.Sum256([]byte(fmt.Sprintf("%s:%d", vmName, order)))
	return fmt.Sprintf("52:54:00:%02x:%02x:%02x", digest[0], digest[1], digest[2])
}

func safeFilePart(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:8])
}

func backupNetworkEnv(cfg Config) error {
	dir := filepath.Join(cfg.VarDir, "backups", "network")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := os.ReadFile(cfg.EnvPath())
	if err != nil {
		return fmt.Errorf("读取网络配置失败: %w", err)
	}
	path := filepath.Join(dir, "env-"+time.Now().Format("20060102-150405.000")+".backup")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("备份网络配置失败: %w", err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) > 5 {
		for _, entry := range entries[:len(entries)-5] {
			if !entry.IsDir() {
				_ = os.Remove(filepath.Join(dir, entry.Name()))
			}
		}
	}
	return nil
}

func networkCompatibilityStatePath(cfg Config) string {
	return filepath.Join(cfg.VarDir, "state", "network-compat.json")
}

func saveNetworkCompatibilityState(cfg Config, state NetworkCompatibilityState) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	path := networkCompatibilityStatePath(cfg)
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, data, 0o600); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}

func loadNetworkCompatibilityState(cfg Config) NetworkCompatibilityState {
	var state NetworkCompatibilityState
	data, err := os.ReadFile(networkCompatibilityStatePath(cfg))
	if err == nil {
		_ = json.Unmarshal(data, &state)
	}
	return state
}

func uniqueSorted(values []string) []string {
	seen := make(map[string]struct{})
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func installNetworkCommandCompatibility(ctx context.Context, cfg Config, bridge string, directBridges []directBridgeMapping, bindings []vpcBinding) (bool, error) {
	realVirsh, err := exec.LookPath("virsh")
	if err != nil {
		return false, errors.New("安装网络命令兼容层时未找到 virsh")
	}
	realOVSVsctl, err := exec.LookPath("ovs-vsctl")
	if err != nil {
		return false, errors.New("安装网络命令兼容层时未找到 ovs-vsctl")
	}
	realOVSOfctl, err := exec.LookPath("ovs-ofctl")
	if err != nil {
		return false, errors.New("安装网络命令兼容层时未找到 ovs-ofctl")
	}
	realIP, err := exec.LookPath("ip")
	if err != nil {
		return false, errors.New("安装网络命令兼容层时未找到 ip")
	}
	realQEMUImg, err := exec.LookPath("qemu-img")
	if err != nil {
		return false, errors.New("安装虚拟磁盘命令兼容层时未找到 qemu-img")
	}

	compatDir := filepath.Join(cfg.InstallDir, ".fnos-compat", "network-bin")
	if err := os.MkdirAll(compatDir, 0o755); err != nil {
		return false, fmt.Errorf("创建网络命令兼容目录失败: %w", err)
	}
	mappingPath := filepath.Join(filepath.Dir(compatDir), directBridgeMapName)
	if _, writeErr := writeFileIfChanged(mappingPath, serializeDirectBridgeMappings(directBridges), 0o644); writeErr != nil {
		return false, fmt.Errorf("写入桥接网卡映射失败: %w", writeErr)
	}
	vlanMapPath := filepath.Join(filepath.Dir(compatDir), vpcVLANMapName)
	if _, writeErr := writeFileIfChanged(vlanMapPath, serializeVPCVLANMappings(bindings), 0o644); writeErr != nil {
		return false, fmt.Errorf("写入 VPC VLAN 映射失败: %w", writeErr)
	}
	runtimeBridgeMapPath := filepath.Join(filepath.Dir(compatDir), vpcRuntimeBridgeMapName)
	if _, writeErr := writeFileIfChanged(runtimeBridgeMapPath, serializeVPCRuntimeBridgeMappings(bindings), 0o644); writeErr != nil {
		return false, fmt.Errorf("写入 VPC 运行态桥接映射失败: %w", writeErr)
	}
	changed := false
	scripts := map[string]string{
		"virsh":     virshCompatibilityScript(),
		"ovs-vsctl": ovsVSCTLCompatibilityScript(),
		"ovs-ofctl": ovsOFCTLCompatibilityScript(),
		"ip":        ipCompatibilityScript(),
		"qemu-img":  qemuImgCompatibilityScript(),
	}
	for name, content := range scripts {
		updated, writeErr := writeExecutableIfChanged(filepath.Join(compatDir, name), content)
		if writeErr != nil {
			return false, fmt.Errorf("安装 %s 命令兼容层失败: %w", name, writeErr)
		}
		changed = changed || updated
	}

	dropInDir := filepath.Join(cfg.SystemRoot, "etc", "systemd", "system", cfg.ServiceName+".d")
	if err := os.MkdirAll(dropInDir, 0o755); err != nil {
		return false, fmt.Errorf("创建 QVMConsole systemd 覆盖目录失败: %w", err)
	}
	dropIn := fmt.Sprintf(`[Service]
Environment="PATH=%s:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
Environment="QVMC_REAL_VIRSH=%s"
Environment="QVMC_LIBVIRT_HELPER=%s"
Environment="QVMC_REAL_OVS_VSCTL=%s"
Environment="QVMC_REAL_OVS_OFCTL=%s"
Environment="QVMC_REAL_IP=%s"
Environment="QVMC_REAL_QEMU_IMG=%s"
Environment="QVMC_LIBVIRT_BRIDGE=%s"
Environment="QVMC_OVS_BRIDGE=%s"
Environment="QVMC_DIRECT_BRIDGE_MAP=%s"
Environment="QVMC_VPC_VLAN_MAP=%s"
Environment="QVMC_VPC_RUNTIME_BRIDGE_MAP=%s"
`, compatDir, realVirsh, filepath.Join(cfg.SystemRoot, "etc", "libvirt", "hooks", nvramHelperFileName), realOVSVsctl, realOVSOfctl, realIP, realQEMUImg, bridge, readEnvValue(cfg.EnvPath(), "KVM_OVS_BRIDGE"), mappingPath, vlanMapPath, runtimeBridgeMapPath)
	dropInPath := filepath.Join(dropInDir, "50-fnos-network-compat.conf")
	dropInChanged, writeErr := writeFileIfChanged(dropInPath, dropIn, 0o644)
	if writeErr != nil {
		return false, fmt.Errorf("写入 QVMConsole 网络兼容服务配置失败: %w", writeErr)
	}
	changed = changed || dropInChanged
	restoreDropInDir := filepath.Join(cfg.SystemRoot, "etc", "systemd", "system", bridgeRestoreService+".d")
	if err := os.MkdirAll(restoreDropInDir, 0o755); err != nil {
		return false, fmt.Errorf("创建桥接恢复服务覆盖目录失败: %w", err)
	}
	restoreDropInChanged, writeErr := writeFileIfChanged(
		filepath.Join(restoreDropInDir, "50-fnos-network-compat.conf"),
		"[Service]\nExecStart=\nExecStart=/bin/true\n",
		0o644,
	)
	if writeErr != nil {
		return false, fmt.Errorf("禁用飞牛不兼容的 OVS 桥恢复任务失败: %w", writeErr)
	}
	changed = changed || restoreDropInChanged
	_, _ = commandOutput(ctx, "systemctl", "disable", "--now", bridgeRestoreService)
	if dropInChanged || restoreDropInChanged {
		if output, reloadErr := commandOutput(ctx, "systemctl", "daemon-reload"); reloadErr != nil {
			return false, fmt.Errorf("重新加载网络兼容服务配置失败: %s", output)
		}
	}
	return changed, nil
}

func serializeDirectBridgeMappings(mappings []directBridgeMapping) string {
	var output strings.Builder
	for _, mapping := range mappings {
		if !interfaceNamePattern.MatchString(mapping.BridgeName) || !interfaceNamePattern.MatchString(mapping.UplinkIF) {
			continue
		}
		output.WriteString(mapping.BridgeName)
		output.WriteByte('\t')
		output.WriteString(mapping.UplinkIF)
		output.WriteByte('\n')
	}
	return output.String()
}

func serializeVPCVLANMappings(bindings []vpcBinding) string {
	var output strings.Builder
	for _, binding := range bindings {
		vmName := strings.TrimSpace(binding.VMName)
		if vmName == "" || strings.ContainsAny(vmName, "\r\n\t") || binding.InterfaceOrder < 0 || binding.VLANID <= 0 {
			continue
		}
		output.WriteString(vmName)
		output.WriteByte('\t')
		output.WriteString(fmt.Sprintf("%d", binding.InterfaceOrder))
		output.WriteByte('\t')
		output.WriteString(fmt.Sprintf("%d", binding.VLANID))
		output.WriteByte('\n')
	}
	return output.String()
}

func serializeVPCRuntimeBridgeMappings(bindings []vpcBinding) string {
	var output strings.Builder
	for _, binding := range bindings {
		vmName := strings.TrimSpace(binding.VMName)
		bridgeName := strings.TrimSpace(binding.BridgeName)
		if !strings.EqualFold(strings.TrimSpace(binding.BridgeMode), directBridgeMode) ||
			vmName == "" || strings.ContainsAny(vmName, "\r\n\t") ||
			binding.InterfaceOrder < 0 || !interfaceNamePattern.MatchString(bridgeName) {
			continue
		}
		output.WriteString(vmName)
		output.WriteByte('\t')
		output.WriteString(fmt.Sprintf("%d", binding.InterfaceOrder))
		output.WriteByte('\t')
		output.WriteString(bridgeName)
		output.WriteByte('\n')
	}
	return output.String()
}

func writeFileIfChanged(path, content string, mode os.FileMode) (bool, error) {
	if current, err := os.ReadFile(path); err == nil && string(current) == content {
		if chmodErr := os.Chmod(path, mode); chmodErr != nil {
			return false, chmodErr
		}
		return false, nil
	}
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, []byte(content), mode); err != nil {
		return false, err
	}
	if err := os.Chmod(temporary, mode); err != nil {
		_ = os.Remove(temporary)
		return false, err
	}
	if err := os.Rename(temporary, path); err != nil {
		_ = os.Remove(temporary)
		return false, err
	}
	return true, nil
}

func virshCompatibilityScript() string {
	return `#!/bin/bash
set -euo pipefail
REAL="${QVMC_REAL_VIRSH:-/usr/bin/virsh}"
LIBVIRT_HELPER="${QVMC_LIBVIRT_HELPER:-/etc/libvirt/hooks/qvmconsole-nvram-helper}"
OVS_BRIDGE="${QVMC_OVS_BRIDGE:-br-ovs}"
LIBVIRT_BRIDGE="${QVMC_LIBVIRT_BRIDGE:-virbr0}"
BRIDGE_MAP="${QVMC_DIRECT_BRIDGE_MAP:-/opt/kvm-console/.fnos-compat/direct-bridges.tsv}"
VLAN_MAP="${QVMC_VPC_VLAN_MAP:-/opt/kvm-console/.fnos-compat/vpc-vlans.tsv}"
RUNTIME_BRIDGE_MAP="${QVMC_VPC_RUNTIME_BRIDGE_MAP:-/opt/kvm-console/.fnos-compat/vpc-runtime-bridges.tsv}"

	transform_xml() {
		awk -v ovs="$OVS_BRIDGE" -v bridge="$LIBVIRT_BRIDGE" -v map_file="$BRIDGE_MAP" '
	function has_attr(text, name, value, single, doubleq) {
	  single = name "=\047" value "\047"
	  doubleq = name "=\042" value "\042"
	  return index(text, single) || index(text, doubleq)
	}
	BEGIN {
	  while ((getline entry < map_file) > 0) {
	    split(entry, fields, "\t")
	    if (fields[1] != "" && fields[2] != "") direct[fields[1]] = fields[2]
	  }
	  close(map_file)
	}
	{
	  line = $0
	  if (skip_managed_dac) {
	    if (line ~ /<\/seclabel>/) skip_managed_dac = 0
	    next
	  }
	  # 1.0.24-1.0.25 曾禁用整个域的 DAC relabel；这会阻止 libvirt 修正 swtpm socket。
	  if (line ~ /<seclabel[[:space:]]/ && has_attr(line, "type", "none") &&
	      has_attr(line, "model", "dac") && has_attr(line, "relabel", "no")) {
	    if (line ~ /\/>/) {
	      sub(/<seclabel[[:space:]][^>]*\/>/, "", line)
	      if (line ~ /^[[:space:]]*$/) next
	    } else if (line ~ /<\/seclabel>/) {
	      sub(/<seclabel[[:space:]][^>]*>[^<]*<\/seclabel>/, "", line)
	      if (line ~ /^[[:space:]]*$/) next
	    } else {
	      skip_managed_dac = 1
	      next
	    }
	  }
	  if (line ~ /<interface[[:space:]]/) {
	    in_interface = 1
	    header = line
	    before_source = ""
	    source_seen = 0
	    compat = 0
	    next
	  }
	  if (in_interface && !source_seen && line ~ /<source[[:space:]]/) {
	    direct_uplink = ""
	    for (name in direct) {
	      single = "bridge=\047" name "\047"
	      doubleq = "bridge=\042" name "\042"
	      if (index(line, single) || index(line, doubleq)) {
	        direct_uplink = direct[name]
	        break
	      }
	    }
	    if (direct_uplink != "") {
	      gsub(/type=(\047bridge\047|\042bridge\042)/, "type=\047direct\047", header)
	      sub(/<source[^>]*\/>/, "<source dev=\047" direct_uplink "\047 mode=\047bridge\047/>", line)
	      compat = 1
	    } else {
	      single = "bridge=\047" ovs "\047"
	      doubleq = "bridge=\042" ovs "\042"
	      if (index(line, single)) {
	        gsub(single, "bridge=\047" bridge "\047", line)
	        compat = 1
	      }
	      if (index(line, doubleq)) {
	        gsub(doubleq, "bridge=\042" bridge "\042", line)
	        compat = 1
	      }
	    }
	    print header
	    printf "%s", before_source
	    print line
	    source_seen = 1
	    next
	  }
	  if (in_interface && !source_seen) {
	    if (line ~ /<\/interface>/) {
	      print header
	      printf "%s", before_source
	      print line
	      in_interface = 0
	      next
	    }
	    before_source = before_source line ORS
	    next
	  }
	  if (skip_virtual) {
	    if (line ~ /<\/virtualport>/) skip_virtual = 0
      next
    }
    if (in_interface && line ~ /<virtualport/ && line ~ /openvswitch/) {
      compat = 1
      if (line !~ /\/>/) skip_virtual = 1
      next
    }
    if (skip_vlan) {
      if (line ~ /<\/vlan>/) skip_vlan = 0
      next
    }
    if (in_interface && compat && line ~ /<vlan/) {
      if (line !~ /<\/vlan>/) skip_vlan = 1
      next
    }
	  print line
	  if (line ~ /<\/interface>/) {
	    in_interface = 0
	    compat = 0
	  }
	}'
}

restore_xml() {
	local domain="${1:-}"
	awk -v domain="$domain" -v ovs="$OVS_BRIDGE" -v bridge="$LIBVIRT_BRIDGE" -v map_file="$BRIDGE_MAP" -v vlan_file="$VLAN_MAP" '
	BEGIN {
	  iface_order = 0
	  while ((getline entry < map_file) > 0) {
	    split(entry, fields, "\t")
	    if (fields[1] != "" && fields[2] != "") reverse[fields[2]] = fields[1]
	  }
	  close(map_file)
	  while ((getline entry < vlan_file) > 0) {
	    split(entry, fields, "\t")
	    order_key = fields[2] + 0
	    if (fields[1] == domain && fields[2] ~ /^[0-9]+$/ && fields[3] ~ /^[0-9]+$/ && fields[3] > 0) {
	      vlan[order_key] = fields[3]
	    }
	  }
	  close(vlan_file)
	}
	{
	  line = $0
	  if (line ~ /<interface[[:space:]]/) {
	    in_interface = 1
	    header = line
	    before_source = ""
	    source_seen = 0
	    current_order = iface_order
	    next
	  }
	  if (in_interface && !source_seen && line ~ /<source[[:space:]]/) {
	    display_bridge = ""
	    direct_uplink = ""
	    single = "bridge=\047" bridge "\047"
	    doubleq = "bridge=\042" bridge "\042"
	    if (index(line, single) || index(line, doubleq)) {
	      display_bridge = ovs
	    } else {
	      for (uplink in reverse) {
	        single = "dev=\047" uplink "\047"
	        doubleq = "dev=\042" uplink "\042"
	        if (index(line, single) || index(line, doubleq)) {
	          display_bridge = reverse[uplink]
	          direct_uplink = uplink
	          break
	        }
	      }
	    }
	    if (display_bridge != "") {
	      gsub(/type=(\047direct\047|\042direct\042)/, "type=\047bridge\047", header)
	      sub(/<source[^>]*\/>/, "<source bridge=\047" display_bridge "\047/>", line)
	    }
	    print header
	    printf "%s", before_source
	    print line
	    if (display_bridge != "") print "      <virtualport type=\047openvswitch\047/>"
	    if (display_bridge != "" && vlan[current_order] != "") {
	      print "      <vlan>"
	      print "        <tag id=\047" vlan[current_order] "\047/>"
	      print "      </vlan>"
	    }
	    source_seen = 1
	    next
	  }
	  if (in_interface && !source_seen) {
	    if (line ~ /<\/interface>/) {
	      print header
	      printf "%s", before_source
	      print line
	      in_interface = 0
	      iface_order++
	      next
	    }
	    before_source = before_source line ORS
	    next
	  }
	  print line
	  if (line ~ /<\/interface>/) {
	    in_interface = 0
	    iface_order++
	  }
	}'
}

lookup_uplink() {
	awk -F '\t' -v bridge="$1" '$1 == bridge {print $2; exit}' "$BRIDGE_MAP" 2>/dev/null
}

case "${1:-}" in
  attach-device|detach-device)
    if [ -f "${3:-}" ]; then
      temporary="$(mktemp /tmp/qvmc-virsh-interface.XXXXXX.xml)"
      trap 'rm -f "$temporary"' EXIT
      transform_xml < "$3" > "$temporary"
	  if [ "$1" = "attach-device" ] && [ -x "$LIBVIRT_HELPER" ] && ! "$LIBVIRT_HELPER" storage-xml < "$temporary"; then
	    echo "挂载前应用飞牛虚拟磁盘权限兼容失败" >&2
	    exit 1
	  fi
      args=("$@")
      args[2]="$temporary"
      exec "$REAL" "${args[@]}"
    fi
    ;;
	define)
	  if [ -f "${2:-}" ]; then
	    temporary="$(mktemp /tmp/qvmc-virsh-domain.XXXXXX.xml)"
	    trap 'rm -f "$temporary"' EXIT
	    transform_xml < "$2" > "$temporary"
	    exec "$REAL" define "$temporary" "${@:3}"
	  fi
	  ;;
	start)
	  domain="${2:-}"
	  if [ -n "$domain" ]; then
	    original="$(mktemp /tmp/qvmc-virsh-start.XXXXXX.xml)"
	    transformed="${original}.transformed"
	    trap 'rm -f "$original" "$transformed"' EXIT
	    if "$REAL" dumpxml "$domain" --inactive > "$original" 2>/dev/null; then
	      transform_xml < "$original" > "$transformed"
	      if ! cmp -s "$original" "$transformed"; then
	        "$REAL" define "$transformed" >/dev/null
	      fi
	      # 部分飞牛 libvirt 启动链路不会可靠触发生命周期钩子，启动前同步执行兼容程序。
	      if [ -x "$LIBVIRT_HELPER" ] && ! "$LIBVIRT_HELPER" qemu-hook "$domain" prepare begin - < "$transformed"; then
	        echo "启动前应用飞牛 libvirt 兼容失败: $domain" >&2
	        exit 1
	      fi
	    fi
	    rm -f "$original" "$transformed"
	    trap - EXIT
	  fi
	  exec "$REAL" "$@"
	  ;;
	dumpxml)
	  domain=""
	  for argument in "${@:2}"; do
	    case "$argument" in
	      -*) ;;
	      *) domain="$argument"; break ;;
	    esac
	  done
	  temporary="$(mktemp /tmp/qvmc-virsh-dump.XXXXXX.xml)"
	  trap 'rm -f "$temporary"' EXIT
	  if "$REAL" "$@" > "$temporary"; then
	    restore_xml "$domain" < "$temporary"
	    exit 0
	  else
	    status=$?
	    exit "$status"
	  fi
	  ;;
	attach-interface)
	  args=("$@")
	  direct_mode=0
	  for index in "${!args[@]}"; do
	    if [ "${args[$index]}" = "$OVS_BRIDGE" ]; then
	      args[$index]="$LIBVIRT_BRIDGE"
	    else
	      uplink="$(lookup_uplink "${args[$index]}")"
	      if [ -n "$uplink" ]; then
	        args[$index]="$uplink"
	        direct_mode=1
	      fi
	    fi
	  done
	  if [ "$direct_mode" -eq 1 ]; then
	    for index in "${!args[@]}"; do
	      [ "${args[$index]}" != "bridge" ] || args[$index]="direct"
	    done
	  fi
	  exec "$REAL" "${args[@]}"
	  ;;
	domiflist)
	  domain=""
	  for argument in "${@:2}"; do
	    case "$argument" in
	      -*) ;;
	      *) domain="$argument"; break ;;
	    esac
	  done
	  "$REAL" "$@" | awk -v domain="$domain" -v source="$LIBVIRT_BRIDGE" -v target="$OVS_BRIDGE" -v map_file="$BRIDGE_MAP" -v runtime_map_file="$RUNTIME_BRIDGE_MAP" '
	    BEGIN {
	      while ((getline entry < map_file) > 0) {
	        split(entry, fields, "\t")
	        if (fields[1] != "" && fields[2] != "") reverse[fields[2]] = fields[1]
	      }
	      close(map_file)
	      while ((getline entry < runtime_map_file) > 0) {
	        split(entry, fields, "\t")
	        order_key = fields[2] + 0
	        if (fields[1] == domain && fields[2] ~ /^[0-9]+$/ && fields[3] != "") runtime_bridge[order_key] = fields[3]
	      }
	      close(runtime_map_file)
	    }
	    NR > 2 && NF >= 5 {
	      row_order = seen++
	      if (runtime_bridge[row_order] != "") {
	        $3 = runtime_bridge[row_order]
	      } else if ($3 == source) {
	        $3 = target
	      }
	    }
	    # 保留 direct 类型，避免混合 VPC/桥接网卡被上游误判为运行态切换网络。
	    NR > 2 && $2 == "direct" && reverse[$3] != "" {$3 = reverse[$3]}
	    {print}'
	  exit ${PIPESTATUS[0]}
    ;;
esac

exec "$REAL" "$@"
`
}

func qemuImgCompatibilityScript() string {
	return `#!/bin/bash
set -euo pipefail
REAL="${QVMC_REAL_QEMU_IMG:-/usr/bin/qemu-img}"
LIBVIRT_HELPER="${QVMC_LIBVIRT_HELPER:-/etc/libvirt/hooks/qvmconsole-nvram-helper}"

if [ "${1:-}" != "create" ]; then
	exec "$REAL" "$@"
fi

"$REAL" "$@" || exit $?

# qemu-img create 的输出文件是创建成功后参数列表中最后一个普通文件。
# 从末尾查找可兼容 -b 等前置文件参数，同时跳过常见容量参数。
args=("$@")
disk_path=""
for ((index=${#args[@]} - 1; index >= 1; index--)); do
	value="${args[$index]}"
	if [[ "$value" =~ ^[0-9]+([KMGTPEkmgtpe]([iI]?[bB])?)?$ ]]; then
		continue
	fi
	if [ -f "$value" ]; then
		disk_path="$(readlink -f -- "$value" 2>/dev/null || true)"
		[ -n "$disk_path" ] && break
	fi
done

if [ -n "$disk_path" ]; then
	if [ ! -x "$LIBVIRT_HELPER" ]; then
		echo "新建磁盘后缺少飞牛虚拟磁盘权限兼容程序" >&2
		exit 1
	fi
	"$LIBVIRT_HELPER" storage-path --writable "$disk_path"
fi
`
}

func ovsVSCTLCompatibilityScript() string {
	return `#!/bin/bash
set -euo pipefail
REAL="${QVMC_REAL_OVS_VSCTL:-/usr/bin/ovs-vsctl}"
OVS_BRIDGE="${QVMC_OVS_BRIDGE:-br-ovs}"
LIBVIRT_BRIDGE="${QVMC_LIBVIRT_BRIDGE:-virbr0}"
BRIDGE_MAP="${QVMC_DIRECT_BRIDGE_MAP:-/opt/kvm-console/.fnos-compat/direct-bridges.tsv}"
ARGS=" $* "

valid_name() {
	[[ "$1" =~ ^[A-Za-z0-9_.:-]{1,63}$ ]]
}

lookup_uplink() {
	awk -F '\t' -v bridge="$1" '$1 == bridge {print $2; exit}' "$BRIDGE_MAP" 2>/dev/null
}

remember_bridge() {
	local bridge="$1" uplink="$2" temporary
	valid_name "$bridge" && valid_name "$uplink" || return 1
	mkdir -p "$(dirname "$BRIDGE_MAP")"
	temporary="${BRIDGE_MAP}.tmp.$$"
	awk -F '\t' -v bridge="$bridge" '$1 != bridge' "$BRIDGE_MAP" 2>/dev/null > "$temporary" || true
	printf '%s\t%s\n' "$bridge" "$uplink" >> "$temporary"
	chmod 0644 "$temporary"
	mv -f "$temporary" "$BRIDGE_MAP"
}

forget_bridge() {
	local bridge="$1" temporary
	[ -f "$BRIDGE_MAP" ] || return 0
	temporary="${BRIDGE_MAP}.tmp.$$"
	awk -F '\t' -v bridge="$bridge" '$1 != bridge' "$BRIDGE_MAP" > "$temporary"
	chmod 0644 "$temporary"
	mv -f "$temporary" "$BRIDGE_MAP"
}

args=("$@")
for index in "${!args[@]}"; do
	case "${args[$index]}" in
	  add-br)
	    bridge="${args[$((index + 1))]:-}"
	    if [ -n "$bridge" ]; then exit 0; fi
	    ;;
	  add-port)
	    bridge="${args[$((index + 1))]:-}"
	    port="${args[$((index + 2))]:-}"
	    if [ "$bridge" != "$OVS_BRIDGE" ] && [ -e "/sys/class/net/$port/device" ]; then
	      remember_bridge "$bridge" "$port"
	      exit 0
	    fi
	    if [ "$bridge" = "$OVS_BRIDGE" ] || [ -n "$(lookup_uplink "$bridge")" ]; then exit 0; fi
	    ;;
	  del-br)
	    bridge="${args[$((index + 1))]:-}"
	    if [ -n "$(lookup_uplink "$bridge")" ]; then
	      forget_bridge "$bridge"
	      exit 0
	    fi
	    ;;
	esac
done

if [[ "$ARGS" == *" list-br "* ]]; then
	{ echo "$OVS_BRIDGE"; awk -F '\t' 'NF >= 2 {print $1}' "$BRIDGE_MAP" 2>/dev/null || true; } | awk 'NF && !seen[$0]++'
	exit 0
fi
if [[ "$ARGS" == *" list Bridge "* && "$ARGS" == *" --format=json "* ]]; then
	printf '{"data":[["%s",["set",[]]]],"headings":["name","ports"]}\n' "$OVS_BRIDGE"
	exit 0
fi
if [[ "$ARGS" == *" br-exists "* ]]; then
	bridge="${args[-1]}"
	if [ "$bridge" = "$OVS_BRIDGE" ] || [ -n "$(lookup_uplink "$bridge")" ]; then exit 0; fi
fi
if [[ "$ARGS" == *" list-ports "* ]]; then
	bridge="${args[-1]}"
	if [ "$bridge" = "$OVS_BRIDGE" ]; then
	  /usr/sbin/bridge link show master "$LIBVIRT_BRIDGE" 2>/dev/null | awk -F: '{gsub(/^[[:space:]]+|[[:space:]]+$/, "", $2); split($2, value, "@"); if (value[1] != "") print value[1]}'
	  exit 0
	fi
	uplink="$(lookup_uplink "$bridge")"
	if [ -n "$uplink" ]; then echo "$uplink"; exit 0; fi
fi
if [[ "$ARGS" == *" get Port "*" qos "* ]]; then
	port="${args[-2]}"
	if [ "$port" = "$OVS_BRIDGE" ] || [ -n "$(lookup_uplink "$port")" ]; then echo '[]'; exit 0; fi
fi
if [[ "$ARGS" == *" get Port "*" name "* ]]; then
  name="${@: -2:1}"
  if [ -e "/sys/class/net/$name" ]; then
    printf '"%s"\n' "$name"
    exit 0
  fi
fi
if [[ "$ARGS" == *" get Interface "*" ofport "* ]]; then
  name="${@: -2:1}"
  if [ -e "/sys/class/net/$name" ]; then
    echo '1'
    exit 0
  fi
fi
if [[ "$ARGS" == *" set Port "* || "$ARGS" == *" remove Port "* || "$ARGS" == *" clear Port "* ]]; then
	exit 0
fi
if [[ "$ARGS" == *" $OVS_BRIDGE "* ]]; then
	exit 0
fi
for argument in "${args[@]}"; do
	if [ -n "$(lookup_uplink "$argument")" ]; then exit 0; fi
done

exec "$REAL" "$@"
`
}

func ovsOFCTLCompatibilityScript() string {
	return `#!/bin/bash
set -euo pipefail
REAL="${QVMC_REAL_OVS_OFCTL:-/usr/bin/ovs-ofctl}"
OVS_BRIDGE="${QVMC_OVS_BRIDGE:-br-ovs}"
BRIDGE_MAP="${QVMC_DIRECT_BRIDGE_MAP:-/opt/kvm-console/.fnos-compat/direct-bridges.tsv}"
for argument in "$@"; do
	if [ "$argument" = "$OVS_BRIDGE" ] || awk -F '\t' -v bridge="$argument" '$1 == bridge {found=1} END {exit !found}' "$BRIDGE_MAP" 2>/dev/null; then
	  case " $* " in
	    *" dump-flows "*) echo 'NXST_FLOW reply (xid=0x0):' ;;
	    *" show "*) printf 'OFPT_FEATURES_REPLY (xid=0x0): dpid:0000000000000000\n LOCAL(%s): addr:00:00:00:00:00:00\n' "$argument" ;;
	  esac
    exit 0
  fi
done
exec "$REAL" "$@"
`
}

func ipCompatibilityScript() string {
	return `#!/bin/bash
set -euo pipefail
REAL="${QVMC_REAL_IP:-/usr/sbin/ip}"
OVS_BRIDGE="${QVMC_OVS_BRIDGE:-br-ovs}"
LIBVIRT_BRIDGE="${QVMC_LIBVIRT_BRIDGE:-virbr0}"
BRIDGE_MAP="${QVMC_DIRECT_BRIDGE_MAP:-/opt/kvm-console/.fnos-compat/direct-bridges.tsv}"
ARGS=" $* "
args=("$@")
for index in "${!args[@]}"; do
	argument="${args[$index]}"
	target=""
	if [ "$argument" = "$OVS_BRIDGE" ]; then
	  target="$LIBVIRT_BRIDGE"
	else
	  target="$(awk -F '\t' -v bridge="$argument" '$1 == bridge {print $2; exit}' "$BRIDGE_MAP" 2>/dev/null)"
	fi
	[ -n "$target" ] || continue
	if [[ "$ARGS" == *" show "* ]]; then
	  args[$index]="$target"
	  exec "$REAL" "${args[@]}"
	fi
	# 飞牛兼容模式下桥接名是逻辑别名，不能迁移或清空宿主机物理口配置。
	exit 0
done
exec "$REAL" "$@"
`
}
