package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

const (
	storageAppArmorBlockBegin = "# BEGIN qvmconsole-manager fnOS storage access"
	storageAppArmorBlockEnd   = "# END qvmconsole-manager fnOS storage access"
)

// ensureStorageAppArmorCompatibility 允许 libvirt 为飞牛卷磁盘生成并加载 QEMU 访问规则。
func ensureStorageAppArmorCompatibility(ctx context.Context, cfg Config) (bool, error) {
	apparmorModule := filepath.Join(cfg.SystemRoot, "sys", "module", "apparmor")
	apparmorDir := filepath.Join(cfg.SystemRoot, "etc", "apparmor.d")
	if !fileExists(apparmorModule) || !fileExists(apparmorDir) {
		return false, nil
	}

	roots, err := storageAppArmorRoots(cfg.SystemRoot)
	if err != nil {
		return false, err
	}
	if len(roots) == 0 {
		return false, nil
	}

	helperRules := buildStorageAppArmorRules(roots, "r")
	qemuRules := buildStorageAppArmorRules(roots, "rwk")
	helperLocal := filepath.Join(apparmorDir, "local", "usr.lib.libvirt.virt-aa-helper")
	// 飞牛的 libvirt-qemu abstraction 只包含 local 文件；同时保留 .d 规则以兼容其他发行版。
	qemuLocal := filepath.Join(apparmorDir, "local", "abstractions", "libvirt-qemu")
	qemuDropIn := filepath.Join(apparmorDir, "abstractions", "libvirt-qemu.d", "qvmconsole-manager-storage")

	helperChanged, err := writeManagedAppArmorBlock(helperLocal, helperRules)
	if err != nil {
		return false, fmt.Errorf("写入 virt-aa-helper 飞牛卷规则失败: %w", err)
	}
	qemuLocalChanged, err := writeManagedAppArmorBlock(qemuLocal, qemuRules)
	if err != nil {
		return false, fmt.Errorf("写入 QEMU 飞牛卷 local 规则失败: %w", err)
	}
	qemuDropInChanged, err := writeManagedAppArmorBlock(qemuDropIn, qemuRules)
	if err != nil {
		return false, fmt.Errorf("写入 QEMU 飞牛卷 drop-in 规则失败: %w", err)
	}
	changed := helperChanged || qemuLocalChanged || qemuDropInChanged
	if changed {
		profile := filepath.Join(apparmorDir, "usr.lib.libvirt.virt-aa-helper")
		if err := reloadAppArmorProfile(ctx, profile); err != nil {
			return false, err
		}
	}
	return changed, nil
}

func storageAppArmorRoots(systemRoot string) ([]string, error) {
	configured := strings.TrimSpace(os.Getenv("QVMC_STORAGE_ROOTS"))
	if configured != "" {
		roots := make([]string, 0)
		for _, root := range filepath.SplitList(configured) {
			root = filepath.Clean(strings.TrimSpace(root))
			if !validAppArmorStorageRoot(root) {
				return nil, fmt.Errorf("飞牛存储根目录不适合写入 AppArmor 规则: %s", root)
			}
			roots = append(roots, root)
		}
		return uniqueSortedPaths(roots), nil
	}

	entries, err := os.ReadDir(filepath.Clean(systemRoot))
	if err != nil {
		return nil, fmt.Errorf("扫描飞牛存储卷失败: %w", err)
	}
	roots := make([]string, 0)
	for _, entry := range entries {
		if entry.IsDir() && fnOSVolumeNamePattern.MatchString(entry.Name()) {
			roots = append(roots, filepath.Join(string(filepath.Separator), entry.Name()))
		}
	}
	return uniqueSortedPaths(roots), nil
}

func validAppArmorStorageRoot(root string) bool {
	return filepath.IsAbs(root) && root != string(filepath.Separator) &&
		!strings.ContainsAny(root, "\x00\r\n")
}

func uniqueSortedPaths(paths []string) []string {
	seen := make(map[string]struct{}, len(paths))
	result := make([]string, 0, len(paths))
	for _, path := range paths {
		path = filepath.Clean(path)
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		result = append(result, path)
	}
	sort.Strings(result)
	return result
}

func buildStorageAppArmorRules(roots []string, filePermissions string) string {
	var rules strings.Builder
	for _, root := range roots {
		root = strings.TrimRight(filepath.ToSlash(root), "/")
		fmt.Fprintf(&rules, "%s r,\n", quoteAppArmorPath(root+"/"))
		fmt.Fprintf(&rules, "%s r,\n", quoteAppArmorPath(root+"/**/"))
		fmt.Fprintf(&rules, "%s %s,\n", quoteAppArmorPath(root+"/**"), filePermissions)
	}
	return rules.String()
}

func quoteAppArmorPath(path string) string {
	path = strings.ReplaceAll(path, `\`, `\\`)
	path = strings.ReplaceAll(path, `"`, `\"`)
	return `"` + path + `"`
}

func writeManagedAppArmorBlock(path, rules string) (bool, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return false, err
	}
	block := storageAppArmorBlockBegin + "\n" + rules + storageAppArmorBlockEnd + "\n"
	updated, err := replaceManagedAppArmorBlock(string(data), block)
	if err != nil {
		return false, err
	}
	if updated == string(data) {
		return false, nil
	}
	if err := os.WriteFile(path, []byte(updated), 0o644); err != nil {
		return false, err
	}
	return true, nil
}

func replaceManagedAppArmorBlock(existing, block string) (string, error) {
	start := strings.Index(existing, storageAppArmorBlockBegin)
	end := strings.Index(existing, storageAppArmorBlockEnd)
	if (start >= 0) != (end >= 0) || (start >= 0 && end < start) {
		return "", errors.New("现有 AppArmor 管理标记不完整")
	}
	if start >= 0 {
		end += len(storageAppArmorBlockEnd)
		if end < len(existing) && existing[end] == '\n' {
			end++
		}
		return existing[:start] + block + existing[end:], nil
	}
	trimmed := strings.TrimRight(existing, "\n")
	if trimmed == "" {
		return block, nil
	}
	return trimmed + "\n\n" + block, nil
}

func reloadAppArmorProfile(ctx context.Context, profile string) error {
	if !fileExists(profile) {
		return nil
	}
	parser, err := exec.LookPath("apparmor_parser")
	if err != nil {
		return nil
	}
	command := exec.CommandContext(ctx, parser, "-r", profile)
	if output, runErr := command.CombinedOutput(); runErr != nil {
		return fmt.Errorf("重载 virt-aa-helper AppArmor 规则失败: %s", outputTail(output, 2000))
	}
	return nil
}
