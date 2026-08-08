package app

import (
	"strings"
	"testing"
)

func TestSerializeVPCVLANMappings(t *testing.T) {
	bindings := []vpcBinding{
		{VMName: "vm-a", InterfaceOrder: 0, VLANID: 101},
		{VMName: "vm-b", InterfaceOrder: 2, VLANID: 4094},
		{VMName: "vm-empty-vlan", InterfaceOrder: 0, VLANID: 0},
		{VMName: "vm-negative-order", InterfaceOrder: -1, VLANID: 200},
		{VMName: "vm\tbad", InterfaceOrder: 0, VLANID: 300},
		{VMName: "", InterfaceOrder: 0, VLANID: 400},
	}

	got := serializeVPCVLANMappings(bindings)
	want := "vm-a\t0\t101\nvm-b\t2\t4094\n"
	if got != want {
		t.Fatalf("serializeVPCVLANMappings() = %q, want %q", got, want)
	}
}

func TestSerializeVPCRuntimeBridgeMappings(t *testing.T) {
	bindings := []vpcBinding{
		{VMName: "vm-a", InterfaceOrder: 0, BridgeName: "v200", BridgeMode: "bridge", UplinkIF: "enp1s0-ovs"},
		{VMName: "vm-b", InterfaceOrder: 2, BridgeName: "v300", BridgeMode: "bridge", UplinkIF: "eth0"},
		{VMName: "vm-c", InterfaceOrder: 1, BridgeName: "v400", BridgeMode: "bridge", UplinkIF: ""},
		{VMName: "vm-nat", InterfaceOrder: 0, BridgeName: "br-ovs", BridgeMode: "nat", UplinkIF: ""},
		{VMName: "vm-empty-bridge", InterfaceOrder: 0, BridgeName: "", BridgeMode: "bridge", UplinkIF: "eth1"},
		{VMName: "vm\tbad", InterfaceOrder: 0, BridgeName: "v500", BridgeMode: "bridge", UplinkIF: "eth2"},
		{VMName: "", InterfaceOrder: 0, BridgeName: "v600", BridgeMode: "bridge", UplinkIF: "eth3"},
	}

	got := serializeVPCRuntimeBridgeMappings(bindings)
	want := "vm-a\t0\tv200\nvm-b\t2\tv300\nvm-c\t1\tv400\n"
	if got != want {
		t.Fatalf("serializeVPCRuntimeBridgeMappings() = %q, want %q", got, want)
	}
}

func TestVirshCompatibilityScriptRestoresVLANMap(t *testing.T) {
	script := virshCompatibilityScript()
	required := []string{
		`VLAN_MAP="${QVMC_VPC_VLAN_MAP:-/opt/kvm-console/.fnos-compat/vpc-vlans.tsv}"`,
		`RUNTIME_BRIDGE_MAP="${QVMC_VPC_RUNTIME_BRIDGE_MAP:-/opt/kvm-console/.fnos-compat/vpc-runtime-bridges.tsv}"`,
		`iface_order = 0`,
		`while ((getline entry < vlan_file) > 0)`,
		`order_key = fields[2] + 0`,
		`fields[1] == domain`,
		`vlan[order_key] = fields[3]`,
		`print "      <vlan>"`,
		`print "        <tag id=\047" vlan[current_order] "\047/>"`,
		`restore_xml "$domain" < "$temporary"`,
	}
	for _, needle := range required {
		if !strings.Contains(script, needle) {
			t.Fatalf("virshCompatibilityScript() missing %q", needle)
		}
	}
}

func TestVirshCompatibilityScriptRestoresDomiflistRuntimeBridgeMap(t *testing.T) {
	script := virshCompatibilityScript()
	required := []string{
		`while ((getline entry < runtime_map_file) > 0)`,
		`runtime_bridge[order_key] = fields[3]`,
		`row_order = seen++`,
		`if (runtime_bridge[row_order] != "")`,
		`$3 = runtime_bridge[row_order]`,
	}
	for _, needle := range required {
		if !strings.Contains(script, needle) {
			t.Fatalf("virshCompatibilityScript() missing %q", needle)
		}
	}
}
