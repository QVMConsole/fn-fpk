package app

import (
	"context"
	"database/sql"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

var (
	fnOSVolumeNamePattern = regexp.MustCompile(`^vol[0-9]+$`)
	qemuUserPattern       = regexp.MustCompile(`^\s*user\s*=\s*["']?([^\s"'#]+)`)
)

type hookDomainXML struct {
	OS struct {
		NVRAM struct {
			Format string `xml:"format,attr"`
			Path   string `xml:",chardata"`
		} `xml:"nvram"`
	} `xml:"os"`
	Devices struct {
		Disks []hookDiskXML `xml:"disk"`
	} `xml:"devices"`
}

type hookDiskXML struct {
	Device       string               `xml:"device,attr"`
	Source       hookDiskSourceXML    `xml:"source"`
	ReadOnly     *struct{}            `xml:"readonly"`
	BackingStore *hookBackingStoreXML `xml:"backingStore"`
}

type hookDiskSourceXML struct {
	File string `xml:"file,attr"`
}

type hookBackingStoreXML struct {
	Source       hookDiskSourceXML    `xml:"source"`
	BackingStore *hookBackingStoreXML `xml:"backingStore"`
}

// ensureVMStorageDirectoryAccess 提前为 QVMConsole 存储池目录配置访问 ACL 和继承 ACL。
func ensureVMStorageDirectoryAccess(ctx context.Context, cfg Config) (int, error) {
	directories, err := findVMStorageDirectories(ctx, cfg)
	if err != nil {
		return 0, err
	}
	if len(directories) == 0 {
		return 0, nil
	}
	username, uid, _, err := resolveQEMUUser()
	if err != nil {
		return 0, err
	}
	if uid == "0" {
		return 0, nil
	}
	prepared := 0
	for _, directory := range directories {
		root, ok := fnOSStorageRoot(directory)
		if !ok {
			continue
		}
		if err := grantQEMUStorageDirectoryACL(directory, root, uid); err != nil {
			return 0, fmt.Errorf("为 QEMU 用户 %s 预置存储目录权限失败: %w", username, err)
		}
		prepared++
	}
	return prepared, nil
}

func findVMStorageDirectories(ctx context.Context, cfg Config) ([]string, error) {
	candidates := make(map[string]struct{})
	addDirectory := func(path string) {
		path = filepath.Clean(strings.TrimSpace(path))
		if !filepath.IsAbs(path) || path == string(filepath.Separator) {
			return
		}
		realPath, err := filepath.EvalSymlinks(path)
		if err != nil {
			return
		}
		info, err := os.Stat(realPath)
		if err != nil || !info.IsDir() {
			return
		}
		if _, ok := fnOSStorageRoot(realPath); ok {
			candidates[realPath] = struct{}{}
		}
	}

	if roots, err := storageAppArmorRoots(cfg.SystemRoot); err == nil {
		for _, root := range roots {
			addDirectory(filepath.Join(root, "vm-disks"))
		}
	}
	addDirectory(readEnvValue(cfg.EnvPath(), "KVM_CLONE_DIR"))

	if fileExists(cfg.DatabasePath()) {
		database, err := sql.Open("sqlite", "file:"+filepath.ToSlash(cfg.DatabasePath())+"?mode=ro&_pragma=busy_timeout(5000)")
		if err != nil {
			return nil, fmt.Errorf("打开 QVMConsole 数据库失败: %w", err)
		}
		defer database.Close()
		database.SetMaxOpenConns(1)
		rows, queryErr := database.QueryContext(ctx, `
			SELECT DISTINCT trim(mount_path)
			FROM host_storage_pools
			WHERE trim(mount_path) <> ''`)
		if queryErr != nil {
			if !strings.Contains(strings.ToLower(queryErr.Error()), "no such table") {
				return nil, fmt.Errorf("读取 QVMConsole 存储池失败: %w", queryErr)
			}
		} else {
			defer rows.Close()
			for rows.Next() {
				var mountPath string
				if err := rows.Scan(&mountPath); err != nil {
					return nil, err
				}
				addDirectory(filepath.Join(mountPath, "vm-disks"))
			}
			if err := rows.Err(); err != nil {
				return nil, err
			}
		}
	}

	directories := make([]string, 0, len(candidates))
	for directory := range candidates {
		directories = append(directories, directory)
	}
	sort.Strings(directories)
	return directories, nil
}

func grantQEMUStorageDirectoryACL(directory, root, uid string) error {
	if !pathWithinRoot(root, directory) && directory != root {
		return fmt.Errorf("存储目录超出飞牛卷: %s", directory)
	}
	if _, err := normalizeQEMUOwnedStorageDirectory(directory, uid); err != nil {
		return err
	}
	for current := directory; ; current = filepath.Dir(current) {
		if !pathWithinRoot(root, current) && current != root {
			return fmt.Errorf("存储目录超出飞牛卷: %s", directory)
		}
		if _, err := setPathACL(current, uid, "--x"); err != nil {
			return err
		}
		if current == root {
			break
		}
	}
	command := exec.Command("setfacl", "-m", fmt.Sprintf("d:u::rwx,d:u:%s:rwx", uid), "--", directory)
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("为 %s 设置磁盘继承 ACL 失败: %s", directory, outputTail(output, 2000))
	}
	return nil
}

// ensureDomainStorageAccess 为域实际引用的飞牛卷文件补充 QEMU 最小 ACL。
func ensureDomainStorageAccess(domain hookDomainXML) (int, error) {
	requested := make(map[string]bool)
	for _, disk := range domain.Devices.Disks {
		writable := disk.ReadOnly == nil && !strings.EqualFold(strings.TrimSpace(disk.Device), "cdrom")
		collectFnOSDiskPath(requested, disk.Source.File, writable)
		collectBackingStorePaths(requested, disk.BackingStore)
	}
	return ensureStoragePathsAccess(requested)
}

func ensureStoragePathsAccess(requested map[string]bool) (int, error) {
	if len(requested) == 0 {
		return 0, nil
	}

	username, uid, gid, err := resolveQEMUUser()
	if err != nil {
		return 0, err
	}
	if uid == "0" {
		return 0, nil
	}
	if _, err := exec.LookPath("setfacl"); err != nil {
		return 0, errors.New("缺少 setfacl，请安装 acl 软件包后重试")
	}

	paths := make([]string, 0, len(requested))
	for path := range requested {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	adjusted := 0
	for _, path := range paths {
		root, ok := fnOSStorageRoot(path)
		if !ok {
			continue
		}
		changed, err := grantQEMUPathACL(path, root, uid, gid, requested[path])
		if err != nil {
			return 0, fmt.Errorf("为 QEMU 用户 %s 配置磁盘访问权限失败: %w", username, err)
		}
		if changed {
			adjusted++
		}
	}
	return adjusted, nil
}

// RunStoragePathCompatibility 为命令兼容层刚创建的磁盘文件补充 QEMU 访问权限。
func RunStoragePathCompatibility(args []string, output io.Writer, errorOutput io.Writer) int {
	if len(args) != 2 || args[0] != "--writable" {
		fmt.Fprintln(errorOutput, "用法: storage-path --writable <磁盘路径>")
		return 2
	}
	path := filepath.Clean(strings.TrimSpace(args[1]))
	if !filepath.IsAbs(path) {
		fmt.Fprintf(errorOutput, "磁盘路径必须是绝对路径: %s\n", path)
		return 2
	}
	requested := make(map[string]bool, 1)
	collectFnOSDiskPath(requested, path, true)
	adjusted, err := ensureStoragePathsAccess(requested)
	if err != nil {
		fmt.Fprintf(errorOutput, "修复飞牛卷磁盘访问权限失败: %v\n", err)
		return 1
	}
	if adjusted > 0 {
		fmt.Fprintf(output, "已修复新建磁盘的 QEMU 访问权限: %s\n", path)
	}
	return 0
}

// RunStorageXMLCompatibility 为 virsh attach-device 使用的磁盘 XML 补充 QEMU 访问权限。
func RunStorageXMLCompatibility(input io.Reader, output io.Writer, errorOutput io.Writer) int {
	data, err := io.ReadAll(io.LimitReader(input, 1<<20))
	if err != nil {
		fmt.Fprintf(errorOutput, "读取设备 XML 失败: %v\n", err)
		return 1
	}
	var disk hookDiskXML
	if err := xml.Unmarshal(data, &disk); err != nil {
		fmt.Fprintf(errorOutput, "解析设备 XML 失败: %v\n", err)
		return 1
	}
	requested := make(map[string]bool)
	writable := disk.ReadOnly == nil && !strings.EqualFold(strings.TrimSpace(disk.Device), "cdrom")
	collectFnOSDiskPath(requested, disk.Source.File, writable)
	collectBackingStorePaths(requested, disk.BackingStore)
	adjusted, err := ensureStoragePathsAccess(requested)
	if err != nil {
		fmt.Fprintf(errorOutput, "修复待挂载磁盘访问权限失败: %v\n", err)
		return 1
	}
	if adjusted > 0 {
		fmt.Fprintf(output, "已修复 %d 个待挂载磁盘文件的 QEMU 访问权限\n", adjusted)
	}
	return 0
}

func collectFnOSDiskPath(paths map[string]bool, source string, writable bool) {
	source = filepath.Clean(strings.TrimSpace(source))
	if source == "." || !filepath.IsAbs(source) {
		return
	}
	realPath, err := filepath.EvalSymlinks(source)
	if err != nil {
		return
	}
	info, err := os.Stat(realPath)
	if err != nil || !info.Mode().IsRegular() {
		return
	}
	if _, ok := fnOSStorageRoot(realPath); !ok {
		return
	}
	paths[realPath] = paths[realPath] || writable
}

func collectBackingStorePaths(paths map[string]bool, backing *hookBackingStoreXML) {
	for current := backing; current != nil; current = current.BackingStore {
		collectFnOSDiskPath(paths, current.Source.File, false)
	}
}

func fnOSStorageRoot(path string) (string, bool) {
	cleaned := filepath.Clean(path)
	if configured := strings.TrimSpace(os.Getenv("QVMC_STORAGE_ROOTS")); configured != "" {
		for _, root := range filepath.SplitList(configured) {
			root = filepath.Clean(strings.TrimSpace(root))
			if root != "." && pathWithinRoot(root, cleaned) {
				return root, true
			}
		}
		return "", false
	}

	parts := strings.Split(strings.TrimPrefix(filepath.ToSlash(cleaned), "/"), "/")
	if len(parts) < 2 || !fnOSVolumeNamePattern.MatchString(parts[0]) {
		return "", false
	}
	return filepath.Join(string(filepath.Separator), parts[0]), true
}

func pathWithinRoot(root, path string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != "." && relative != ".." && !filepath.IsAbs(relative) &&
		!strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func resolveQEMUUser() (string, string, string, error) {
	candidates := make([]string, 0, 4)
	if configured := strings.TrimSpace(os.Getenv("QVMC_QEMU_USER")); configured != "" {
		candidates = append(candidates, configured)
	}
	if configured := readConfiguredQEMUUser(); configured != "" {
		candidates = append(candidates, configured)
	}
	candidates = append(candidates, "libvirt-qemu", "qemu")

	seen := make(map[string]struct{}, len(candidates))
	for _, candidate := range candidates {
		if _, ok := seen[candidate]; ok {
			continue
		}
		seen[candidate] = struct{}{}
		account, err := user.Lookup(candidate)
		if err == nil {
			return account.Username, account.Uid, account.Gid, nil
		}
	}
	return "", "", "", errors.New("未找到 libvirt 使用的 QEMU 系统用户")
}

func readConfiguredQEMUUser() string {
	path := envOr("QVMC_QEMU_CONFIG", "/etc/libvirt/qemu.conf")
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		matches := qemuUserPattern.FindStringSubmatch(line)
		if len(matches) == 2 {
			return strings.TrimSpace(matches[1])
		}
	}
	return ""
}

func grantQEMUPathACL(path, root, uid, gid string, writable bool) (bool, error) {
	changed := false
	for directory := filepath.Dir(path); ; directory = filepath.Dir(directory) {
		if !pathWithinRoot(root, directory) && directory != root {
			return false, fmt.Errorf("磁盘路径超出飞牛存储卷: %s", path)
		}
		if directory == filepath.Dir(path) {
			updated, err := normalizeQEMUOwnedStorageDirectory(directory, uid)
			if err != nil {
				return false, err
			}
			changed = changed || updated
		}
		updated, err := setPathACL(directory, uid, "--x")
		if err != nil {
			return false, err
		}
		if updated {
			changed = true
		}
		if directory == root {
			break
		}
	}
	permissions := "r--"
	if writable {
		permissions = "rw-"
	}
	groupUpdated, err := ensureQEMUFileGroup(path, gid)
	if err != nil {
		return false, err
	}
	updated, err := setPathACL(path, uid, permissions)
	if err != nil {
		return false, err
	}
	modeUpdated, err := ensureQEMUFileModeAccess(path, writable)
	return changed || groupUpdated || updated || modeUpdated, err
}

// ensureQEMUFileGroup 兼容 trima 卷按文件组与 mode 校验普通文件访问的行为。
func ensureQEMUFileGroup(path, gid string) (bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	numericGID, err := strconv.ParseUint(gid, 10, 32)
	if err != nil {
		return false, fmt.Errorf("解析 QEMU 用户组失败 %s: %w", gid, err)
	}
	currentGID, known := fileOwnerGID(info)
	if known && uint64(currentGID) == numericGID {
		return false, nil
	}
	if err := os.Chown(path, -1, int(numericGID)); err != nil {
		return false, fmt.Errorf("设置 QEMU 磁盘文件组失败 %s: %w", path, err)
	}
	return true, nil
}

// ensureQEMUFileModeAccess 兼容 trima 卷仅写入 ACL、不同步文件 mode 的行为。
func ensureQEMUFileModeAccess(path string, writable bool) (bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	current := info.Mode().Perm()
	required := requiredQEMUFileMode(writable)
	updated := current | required
	if updated == current {
		return false, nil
	}
	if err := os.Chmod(path, updated); err != nil {
		return false, fmt.Errorf("同步 QEMU 磁盘文件模式失败 %s: %w", path, err)
	}
	return true, nil
}

func requiredQEMUFileMode(writable bool) os.FileMode {
	if writable {
		return 0o660
	}
	return 0o440
}

// normalizeQEMUOwnedStorageDirectory 避免 QEMU 命中权限为 --- 的目录属主条目后跳过命名 ACL。
func normalizeQEMUOwnedStorageDirectory(directory, uid string) (bool, error) {
	info, err := os.Stat(directory)
	if err != nil {
		return false, err
	}
	ownerUID, ownerKnown := fileOwnerUID(info)
	numericUID, numericErr := strconv.ParseUint(uid, 10, 32)
	if !ownerKnown || numericErr != nil || uint64(ownerUID) != numericUID {
		return false, nil
	}
	// 只调整属主并保留现有组；随后由 setPathACL 写入 QEMU 的命名 ACL。
	if err := os.Chown(directory, 0, -1); err != nil {
		return false, fmt.Errorf("将 QEMU 磁盘目录属主规范为 root 失败 %s: %w", directory, err)
	}
	return true, nil
}

func setPathACL(path, uid, permissions string) (bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	entryID := uid
	entryPermissions := "---"
	effectivePermissions := "---"
	ownerUID, ownerKnown := fileOwnerUID(info)
	numericUID, numericErr := strconv.ParseUint(uid, 10, 32)
	if ownerKnown && numericErr == nil && uint64(ownerUID) == numericUID {
		entryID = ""
		entryPermissions = ownerACLPermissions(info.Mode().Perm())
		effectivePermissions = entryPermissions
	} else if existing, effective, readErr := readNamedUserACL(path, uid); readErr == nil {
		entryPermissions = existing
		effectivePermissions = effective
	}
	if permissionsContain(effectivePermissions, permissions) {
		return false, nil
	}
	permissions = mergeACLPermissions(entryPermissions, permissions)
	command := exec.Command("setfacl", "-m", fmt.Sprintf("u:%s:%s", entryID, permissions), "--", path)
	if output, err := command.CombinedOutput(); err != nil {
		return false, fmt.Errorf("setfacl %s 失败: %s", path, outputTail(output, 2000))
	}
	return true, nil
}

func readNamedUserACL(path, uid string) (string, string, error) {
	command := exec.Command("getfacl", "-cpn", "--", path)
	output, err := command.CombinedOutput()
	if err != nil {
		return "", "", err
	}
	prefix := "user:" + uid + ":"
	for _, line := range strings.Split(string(output), "\n") {
		if strings.HasPrefix(line, prefix) {
			fields := strings.Fields(strings.TrimPrefix(line, prefix))
			if len(fields) == 0 || len(fields[0]) < 3 {
				continue
			}
			permissions := fields[0][:3]
			effective := permissions
			for _, field := range fields[1:] {
				if strings.HasPrefix(field, "#effective:") {
					value := strings.TrimPrefix(field, "#effective:")
					if len(value) >= 3 {
						effective = value[:3]
					}
				}
			}
			return permissions, effective, nil
		}
	}
	return "---", "---", nil
}

func ownerACLPermissions(mode os.FileMode) string {
	permissions := []byte("---")
	if mode&0o400 != 0 {
		permissions[0] = 'r'
	}
	if mode&0o200 != 0 {
		permissions[1] = 'w'
	}
	if mode&0o100 != 0 {
		permissions[2] = 'x'
	}
	return string(permissions)
}

func mergeACLPermissions(current, required string) string {
	merged := []byte("---")
	for index, permission := range []byte("rwx") {
		if (len(current) > index && current[index] == permission) ||
			(len(required) > index && required[index] == permission) {
			merged[index] = permission
		}
	}
	return string(merged)
}

func permissionsContain(current, required string) bool {
	for index, permission := range []byte("rwx") {
		if len(required) > index && required[index] == permission &&
			(len(current) <= index || current[index] != permission) {
			return false
		}
	}
	return true
}
