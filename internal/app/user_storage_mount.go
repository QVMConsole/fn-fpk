package app

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
)

const (
	userStorageImageEnv   = "KVM_USER_STORAGE_IMAGE"
	userStorageMountPoint = "/var/lib/kvm-user-storage"
)

type userStorageFstabEntry struct {
	source    string
	target    string
	commented bool
}

// EnsureUserStorageMountCompatibility 将上游强制挂载迁移为不会阻断 fnOS 启动的自动挂载。
func EnsureUserStorageMountCompatibility(ctx context.Context, cfg Config) (bool, error) {
	fstabPath := filepath.Join(cfg.SystemRoot, "etc", "fstab")
	data, err := os.ReadFile(fstabPath)
	if err != nil && !os.IsNotExist(err) {
		return false, fmt.Errorf("读取 /etc/fstab 失败: %w", err)
	}

	imagePath := strings.TrimSpace(readEnvValue(cfg.EnvPath(), userStorageImageEnv))
	if imagePath == "" {
		qvmconsolePresent := fileExists(cfg.EnvPath()) || fileExists(filepath.Join(cfg.InstallDir, "kvm-console")) || fileExists(cfg.ServiceFile())
		imagePath = detectUserStorageImageFromFstab(string(data), qvmconsolePresent)
	}
	if imagePath == "" {
		return false, nil
	}
	imagePath = path.Clean(imagePath)
	if !path.IsAbs(imagePath) || imagePath == "/" {
		return false, fmt.Errorf("用户存储镜像路径无效: %s", imagePath)
	}

	backingMount := userStorageBackingMount(ctx, imagePath)
	updated, changed := rewriteUserStorageFstab(string(data), imagePath, backingMount)
	if !changed {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(fstabPath), 0o755); err != nil {
		return false, fmt.Errorf("创建 /etc/fstab 所在目录失败: %w", err)
	}
	if _, err := writeFileIfChanged(fstabPath, updated, 0o644); err != nil {
		return false, fmt.Errorf("更新 /etc/fstab 用户存储挂载失败: %w", err)
	}
	return true, nil
}

// RemoveUserStorageMountCompatibility 删除 QVMConsole 专用挂载，保留镜像和用户数据。
func RemoveUserStorageMountCompatibility(cfg Config) (bool, error) {
	fstabPath := filepath.Join(cfg.SystemRoot, "etc", "fstab")
	data, err := os.ReadFile(fstabPath)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("读取 /etc/fstab 失败: %w", err)
	}
	updated, changed := removeUserStorageFstabEntries(string(data))
	if !changed {
		return false, nil
	}
	if _, err := writeFileIfChanged(fstabPath, updated, 0o644); err != nil {
		return false, fmt.Errorf("清理 /etc/fstab 用户存储挂载失败: %w", err)
	}
	return true, nil
}

func userStorageBackingMount(ctx context.Context, imagePath string) string {
	output, err := commandOutput(ctx, "findmnt", "--noheadings", "--raw", "--output", "TARGET", "--target", path.Dir(imagePath))
	if err != nil {
		return "/"
	}
	mountPoint := path.Clean(strings.TrimSpace(output))
	if !path.IsAbs(mountPoint) || mountPoint == userStorageMountPoint {
		return "/"
	}
	return mountPoint
}

func detectUserStorageImageFromFstab(content string, includeCommented bool) string {
	commentedImage := ""
	for _, line := range strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n") {
		entry, ok := parseUserStorageFstabLine(line)
		if !ok || !path.IsAbs(entry.source) {
			continue
		}
		if !entry.commented {
			return path.Clean(entry.source)
		}
		if includeCommented && commentedImage == "" {
			commentedImage = path.Clean(entry.source)
		}
	}
	return commentedImage
}

func rewriteUserStorageFstab(content, imagePath, backingMount string) (string, bool) {
	lines := strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n")
	output := make([]string, 0, len(lines)+1)
	for _, line := range lines {
		if _, ok := parseUserStorageFstabLine(line); ok {
			continue
		}
		output = append(output, line)
	}

	for len(output) > 0 && output[len(output)-1] == "" {
		output = output[:len(output)-1]
	}
	options := []string{
		"loop",
		"prjquota",
		"nofail",
		"x-systemd.automount",
		"x-systemd.mount-timeout=30s",
	}
	backingMount = path.Clean(strings.TrimSpace(backingMount))
	if path.IsAbs(backingMount) && backingMount != "/" && backingMount != userStorageMountPoint {
		options = append(options, "x-systemd.requires-mounts-for="+escapeFstabField(backingMount))
	}
	output = append(output, fmt.Sprintf(
		"%s %s ext4 %s 0 0",
		escapeFstabField(path.Clean(imagePath)),
		escapeFstabField(userStorageMountPoint),
		strings.Join(options, ","),
	))
	updated := strings.Join(output, "\n") + "\n"
	normalizedOriginal := strings.ReplaceAll(content, "\r\n", "\n")
	return updated, updated != normalizedOriginal
}

func removeUserStorageFstabEntries(content string) (string, bool) {
	lines := strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n")
	output := make([]string, 0, len(lines))
	changed := false
	for _, line := range lines {
		if _, ok := parseUserStorageFstabLine(line); ok {
			changed = true
			continue
		}
		output = append(output, line)
	}
	if !changed {
		return content, false
	}
	for len(output) > 0 && output[len(output)-1] == "" {
		output = output[:len(output)-1]
	}
	if len(output) == 0 {
		return "", true
	}
	return strings.Join(output, "\n") + "\n", true
}

func parseUserStorageFstabLine(line string) (userStorageFstabEntry, bool) {
	trimmed := strings.TrimSpace(line)
	commented := false
	if strings.HasPrefix(trimmed, "#") {
		commented = true
		trimmed = strings.TrimSpace(strings.TrimPrefix(trimmed, "#"))
	}
	fields := strings.Fields(trimmed)
	if len(fields) < 4 {
		return userStorageFstabEntry{}, false
	}
	entry := userStorageFstabEntry{
		source:    unescapeFstabField(fields[0]),
		target:    unescapeFstabField(fields[1]),
		commented: commented,
	}
	if path.Clean(entry.target) != userStorageMountPoint {
		return userStorageFstabEntry{}, false
	}
	return entry, true
}

func escapeFstabField(value string) string {
	return strings.NewReplacer(
		`\`, `\134`,
		" ", `\040`,
		"\t", `\011`,
		"#", `\043`,
	).Replace(value)
}

func unescapeFstabField(value string) string {
	return strings.NewReplacer(
		`\040`, " ",
		`\011`, "\t",
		`\043`, "#",
		`\134`, `\`,
	).Replace(value)
}
