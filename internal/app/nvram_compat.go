package app

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	nvramHelperFileName = "qvmconsole-nvram-helper"
	nvramHookFileName   = "50-qvmconsole-nvram-compat"
	nvramHookMarker     = "# qvmconsole-manager: fnOS NVRAM compatibility"
)

type qemuImageInfo struct {
	Format      string `json:"format"`
	VirtualSize int64  `json:"virtual-size"`
}

// EnsureLibvirtCompatibility 安装飞牛旧版 libvirt 所需的启动兼容钩子。
func EnsureLibvirtCompatibility(ctx context.Context, cfg Config) error {
	if runtime.GOOS != "linux" {
		return nil
	}
	if os.Geteuid() != 0 {
		return errors.New("安装 libvirt 兼容组件需要 root 权限")
	}
	if !fileExists(filepath.Join(cfg.InstallDir, "kvm-console")) && !fileExists(cfg.ServiceFile()) {
		return nil
	}
	if _, err := exec.LookPath("setfacl"); err != nil {
		return errors.New("配置虚拟磁盘权限兼容层需要 setfacl，请安装 acl 软件包")
	}

	hooksDir := filepath.Join(cfg.SystemRoot, "etc", "libvirt", "hooks")
	qemuHookDir := filepath.Join(hooksDir, "qemu.d")
	if err := os.MkdirAll(qemuHookDir, 0o755); err != nil {
		return fmt.Errorf("创建 libvirt 钩子目录失败: %w", err)
	}
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("获取管理器程序路径失败: %w", err)
	}
	// libvirtd 的 AppArmor 配置只允许执行 /etc/libvirt/hooks/** 下的程序。
	helperPath := filepath.Join(hooksDir, nvramHelperFileName)
	helperChanged, err := copyExecutableIfChanged(executable, helperPath)
	if err != nil {
		return fmt.Errorf("安装 libvirt 兼容程序失败: %w", err)
	}
	legacyHelper := filepath.Join(cfg.InstallDir, ".fnos-compat", nvramHelperFileName)
	_ = os.Remove(legacyHelper)
	_ = os.Remove(filepath.Dir(legacyHelper))
	hookContent := qemuCompatibilityHookScript(helperPath)
	changed := helperChanged
	compatHook := filepath.Join(qemuHookDir, nvramHookFileName)
	if updated, writeErr := writeExecutableIfChanged(compatHook, hookContent); writeErr != nil {
		return fmt.Errorf("安装 libvirt 启动兼容钩子失败: %w", writeErr)
	} else if updated {
		changed = true
	}

	// libvirt 9 会执行 qemu.d；同时创建主钩子以兼容仅探测主钩子的旧版本。
	mainHook := filepath.Join(hooksDir, "qemu")
	if !fileExists(mainHook) || isManagedNVRAMHook(mainHook) {
		if updated, writeErr := writeExecutableIfChanged(mainHook, hookContent); writeErr != nil {
			return fmt.Errorf("安装 libvirt 主钩子失败: %w", writeErr)
		} else if updated {
			changed = true
		}
	}
	if _, err := ensureStorageAppArmorCompatibility(ctx, cfg); err != nil {
		return err
	}
	if _, err := ensureVMStorageDirectoryAccess(ctx, cfg); err != nil {
		return fmt.Errorf("预置飞牛虚拟机存储目录权限失败: %w", err)
	}
	if changed {
		if err := restartLibvirtForHooks(ctx); err != nil {
			return err
		}
	}
	return nil
}

func copyExecutableIfChanged(source, target string) (bool, error) {
	if sourceHash, sourceErr := fileSHA256(source); sourceErr == nil {
		if targetHash, targetErr := fileSHA256(target); targetErr == nil && sourceHash == targetHash {
			if err := os.Chmod(target, 0o755); err != nil {
				return false, err
			}
			return false, nil
		}
	}
	input, err := os.Open(source)
	if err != nil {
		return false, err
	}
	defer input.Close()
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return false, err
	}
	temporary := target + ".tmp"
	output, err := os.OpenFile(temporary, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return false, err
	}
	_, copyErr := io.Copy(output, input)
	closeErr := output.Close()
	if copyErr != nil {
		_ = os.Remove(temporary)
		return false, copyErr
	}
	if closeErr != nil {
		_ = os.Remove(temporary)
		return false, closeErr
	}
	if err := os.Chmod(temporary, 0o755); err != nil {
		_ = os.Remove(temporary)
		return false, err
	}
	if err := os.Rename(temporary, target); err != nil {
		_ = os.Remove(temporary)
		return false, err
	}
	return true, nil
}

func writeExecutableIfChanged(path, content string) (bool, error) {
	if current, err := os.ReadFile(path); err == nil && string(current) == content {
		if chmodErr := os.Chmod(path, 0o755); chmodErr != nil {
			return false, chmodErr
		}
		return false, nil
	}
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, []byte(content), 0o755); err != nil {
		return false, err
	}
	if err := os.Chmod(temporary, 0o755); err != nil {
		_ = os.Remove(temporary)
		return false, err
	}
	if err := os.Rename(temporary, path); err != nil {
		_ = os.Remove(temporary)
		return false, err
	}
	return true, nil
}

func qemuCompatibilityHookScript(helperPath string) string {
	quotedHelper := shellSingleQuote(helperPath)
	return "#!/bin/sh\n" + nvramHookMarker + "\n" +
		"if [ \"${2:-}\" != \"prepare\" ] || [ \"${3:-}\" != \"begin\" ]; then\n" +
		"  exit 0\n" +
		"fi\n" +
		"HELPER=" + quotedHelper + "\n" +
		"if [ ! -x \"$HELPER\" ]; then\n" +
		"  exit 0\n" +
		"fi\n" +
		"exec \"$HELPER\" qemu-hook \"$@\"\n"
}

func isManagedNVRAMHook(path string) bool {
	content, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return strings.Contains(string(content), nvramHookMarker) || strings.Contains(string(content), nvramHelperFileName)
}

func shellSingleQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func restartLibvirtForHooks(ctx context.Context) error {
	restarted := false
	for _, service := range []string{"libvirtd.service", "virtqemud.service"} {
		if !serviceActive(ctx, service) {
			continue
		}
		output, err := commandOutput(ctx, "systemctl", "restart", service)
		if err != nil {
			return fmt.Errorf("重新加载 libvirt 钩子失败: %s", output)
		}
		restarted = true
	}
	if !restarted {
		return errors.New("libvirt 服务未运行，启动兼容钩子尚未加载")
	}
	return nil
}

// RunQEMUHook 处理 libvirt qemu prepare/begin 事件。
func RunQEMUHook(args []string, input io.Reader, output io.Writer, errorOutput io.Writer) int {
	if len(args) < 3 || args[1] != "prepare" || args[2] != "begin" {
		return 0
	}
	data, err := io.ReadAll(io.LimitReader(input, 8<<20))
	if err != nil {
		fmt.Fprintf(errorOutput, "读取虚拟机 XML 失败: %v\n", err)
		return 1
	}
	var domain hookDomainXML
	if err := xml.Unmarshal(data, &domain); err != nil {
		fmt.Fprintf(errorOutput, "解析虚拟机 XML 失败: %v\n", err)
		return 1
	}
	adjusted, err := ensureDomainStorageAccess(domain)
	if err != nil {
		fmt.Fprintf(errorOutput, "修复虚拟机 %s 的飞牛卷访问权限失败: %v\n", args[0], err)
		return 1
	}
	if adjusted > 0 {
		fmt.Fprintf(output, "已为虚拟机 %s 修复 %d 个飞牛卷磁盘文件的 QEMU 访问权限\n", args[0], adjusted)
	}
	xmlFormat := strings.ToLower(strings.TrimSpace(domain.OS.NVRAM.Format))
	if xmlFormat == "qcow2" || (xmlFormat != "" && xmlFormat != "raw") {
		return 0
	}
	nvramPath := filepath.Clean(strings.TrimSpace(domain.OS.NVRAM.Path))
	if nvramPath == "." || nvramPath == "" {
		return 0
	}
	allowedRoot := envOr("QVMC_NVRAM_ROOT", "/var/lib/libvirt/qemu/nvram")
	if err := requirePathWithin(allowedRoot, nvramPath); err != nil {
		fmt.Fprintln(errorOutput, err)
		return 1
	}
	converted, err := convertNVRAMToRaw(context.Background(), nvramPath)
	if err != nil {
		fmt.Fprintf(errorOutput, "修复虚拟机 %s 的 UEFI NVRAM 失败: %v\n", args[0], err)
		return 1
	}
	if converted {
		fmt.Fprintf(output, "已将虚拟机 %s 的 UEFI NVRAM 转换为飞牛 libvirt 兼容的 raw 格式\n", args[0])
	}
	return 0
}

// RunNVRAMHook 保留旧版辅助程序的命令入口。
func RunNVRAMHook(args []string, input io.Reader, output io.Writer, errorOutput io.Writer) int {
	return RunQEMUHook(args, input, output, errorOutput)
}

func requirePathWithin(root, path string) error {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	relative, err := filepath.Rel(root, path)
	if err != nil || relative == "." || relative == ".." || filepath.IsAbs(relative) || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return fmt.Errorf("NVRAM 路径不在允许目录内: %s", path)
	}
	return nil
}

func convertNVRAMToRaw(ctx context.Context, path string) (bool, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() {
		return false, errors.New("NVRAM 文件不是普通文件")
	}
	imageInfo, err := readQemuImageInfo(ctx, path)
	if err != nil {
		return false, err
	}
	if strings.ToLower(imageInfo.Format) != "qcow2" {
		return false, nil
	}

	lockPath := path + ".fnos-compat.lock"
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return false, fmt.Errorf("获取 NVRAM 转换锁失败: %w", err)
	}
	_ = lock.Close()
	defer os.Remove(lockPath)

	qemuImg, err := exec.LookPath("qemu-img")
	if err != nil {
		return false, errors.New("qemu-img 不存在")
	}
	temporary := fmt.Sprintf("%s.fnos-raw-%d", path, os.Getpid())
	backup := fmt.Sprintf("%s.fnos-qcow2-%d", path, time.Now().UnixNano())
	defer os.Remove(temporary)
	convertCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	command := exec.CommandContext(convertCtx, qemuImg, "convert", "-f", "qcow2", "-O", "raw", path, temporary)
	if output, runErr := command.CombinedOutput(); runErr != nil {
		return false, fmt.Errorf("qemu-img 转换失败: %s", outputTail(output, 2000))
	}
	convertedInfo, err := readQemuImageInfo(convertCtx, temporary)
	if err != nil {
		return false, fmt.Errorf("校验转换结果失败: %w", err)
	}
	if strings.ToLower(convertedInfo.Format) != "raw" || convertedInfo.VirtualSize != imageInfo.VirtualSize {
		return false, errors.New("转换后的 NVRAM 格式或容量异常")
	}
	if err := os.Chmod(temporary, info.Mode().Perm()); err != nil {
		return false, err
	}
	if err := preserveFileOwner(temporary, info); err != nil {
		return false, err
	}
	if err := os.Rename(path, backup); err != nil {
		return false, err
	}
	if err := os.Rename(temporary, path); err != nil {
		_ = os.Rename(backup, path)
		return false, err
	}
	if err := os.Remove(backup); err != nil {
		return true, fmt.Errorf("NVRAM 已转换，但清理临时备份失败: %w", err)
	}
	return true, nil
}

func readQemuImageInfo(ctx context.Context, path string) (qemuImageInfo, error) {
	qemuImg, err := exec.LookPath("qemu-img")
	if err != nil {
		return qemuImageInfo{}, errors.New("qemu-img 不存在")
	}
	command := exec.CommandContext(ctx, qemuImg, "info", "-U", "--output=json", path)
	output, runErr := command.CombinedOutput()
	if runErr != nil {
		return qemuImageInfo{}, fmt.Errorf("读取 NVRAM 格式失败: %s", outputTail(output, 2000))
	}
	var info qemuImageInfo
	if err := json.Unmarshal(output, &info); err != nil {
		return qemuImageInfo{}, fmt.Errorf("解析 NVRAM 格式失败: %w", err)
	}
	return info, nil
}
