package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

type RuntimeState struct {
	Channel       string    `json:"channel"`
	Version       string    `json:"version"`
	Port          int       `json:"port"`
	LastOperation string    `json:"lastOperation"`
	UpdatedAt     time.Time `json:"updatedAt"`
}

type SystemStatus struct {
	Installed       bool                      `json:"installed"`
	ServiceActive   bool                      `json:"serviceActive"`
	ServiceEnabled  bool                      `json:"serviceEnabled"`
	Version         string                    `json:"version"`
	Port            int                       `json:"port"`
	Channel         string                    `json:"channel"`
	KVMAvailable    bool                      `json:"kvmAvailable"`
	LibvirtActive   bool                      `json:"libvirtActive"`
	OVSActive       bool                      `json:"ovsActive"`
	Architecture    string                    `json:"architecture"`
	ManagerVersion  string                    `json:"managerVersion"`
	LastOperation   string                    `json:"lastOperation"`
	StateUpdatedAt  time.Time                 `json:"stateUpdatedAt,omitempty"`
	ActiveJob       *JobSnapshot              `json:"activeJob,omitempty"`
	DatabasePresent bool                      `json:"databasePresent"`
	NetworkCompat   NetworkCompatibilityState `json:"networkCompatibility"`
}

func readEnvValue(path, key string) string {
	file, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) == 2 && strings.TrimSpace(parts[0]) == key {
			return strings.Trim(strings.TrimSpace(parts[1]), "\"'")
		}
	}
	return ""
}

func readPort(cfg Config) int {
	for _, key := range []string{"KVM_PORT", "PORT", "SERVER_PORT"} {
		if value := readEnvValue(cfg.EnvPath(), key); value != "" {
			if port, err := strconv.Atoi(value); err == nil {
				return port
			}
		}
	}
	return 8080
}

func commandOutput(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	err := cmd.Run()
	return strings.TrimSpace(output.String()), err
}

func serviceActive(ctx context.Context, name string) bool {
	return exec.CommandContext(ctx, "systemctl", "is-active", "--quiet", name).Run() == nil
}

func serviceEnabled(ctx context.Context, name string) bool {
	return exec.CommandContext(ctx, "systemctl", "is-enabled", "--quiet", name).Run() == nil
}

func ensureLibguestfsAppliance(ctx context.Context, cfg Config) error {
	if runtime.GOOS != "linux" {
		return nil
	}
	superminDir := filepath.Join(cfg.SystemRoot, "usr", "lib", "x86_64-linux-gnu", "guestfs", "supermin.d")
	if !fileExists(superminDir) {
		return errors.New("libguestfs supermin 目录不存在")
	}

	var libraryPath string
	for _, candidate := range []string{
		filepath.Join(cfg.SystemRoot, "usr", "lib", "x86_64-linux-gnu", "libstdc++.so.6"),
		filepath.Join(cfg.SystemRoot, "lib", "x86_64-linux-gnu", "libstdc++.so.6"),
	} {
		resolved, err := filepath.EvalSymlinks(candidate)
		if err == nil && fileExists(resolved) {
			libraryPath = resolved
			break
		}
	}
	if libraryPath == "" {
		return errors.New("libstdc++.so.6 不存在")
	}

	applianceLibraryPath, err := appliancePath(cfg.SystemRoot, libraryPath)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(superminDir, "packages-qvmconsole"), []byte("libstdc++6\n"), 0o644); err != nil {
		return fmt.Errorf("写入 supermin 包清单失败: %w", err)
	}
	if err := os.WriteFile(filepath.Join(superminDir, "hostfiles-qvmconsole"), []byte(applianceLibraryPath+"\n"), 0o644); err != nil {
		return fmt.Errorf("写入 supermin 文件清单失败: %w", err)
	}

	cachePath := filepath.Join(cfg.SystemRoot, "var", "tmp", ".guestfs-0", "appliance.d")
	if err := os.RemoveAll(cachePath); err != nil {
		return fmt.Errorf("清理 libguestfs appliance 缓存失败: %w", err)
	}

	testTool, err := exec.LookPath("libguestfs-test-tool")
	if err != nil {
		return errors.New("libguestfs-test-tool 不存在")
	}
	testCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	command := exec.CommandContext(testCtx, testTool)
	command.Env = append(os.Environ(), "LANG=en_US.UTF-8", "LC_ALL=en_US.UTF-8")
	output, runErr := command.CombinedOutput()
	if runErr != nil {
		return fmt.Errorf("libguestfs appliance 自检失败: %s", outputTail(output, 4000))
	}
	if !bytes.Contains(output, []byte("TEST FINISHED OK")) {
		return errors.New("libguestfs appliance 自检未返回成功标记")
	}
	return nil
}

func appliancePath(systemRoot, path string) (string, error) {
	root := filepath.Clean(systemRoot)
	relative, err := filepath.Rel(root, filepath.Clean(path))
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return "", fmt.Errorf("libguestfs 依赖路径越界: %s", path)
	}
	return "/" + filepath.ToSlash(relative), nil
}

func outputTail(output []byte, limit int) string {
	if len(output) > limit {
		output = output[len(output)-limit:]
	}
	return strings.TrimSpace(string(output))
}

func anyServiceActive(ctx context.Context, names ...string) bool {
	for _, name := range names {
		if serviceActive(ctx, name) {
			return true
		}
	}
	return false
}

func loadRuntimeState(cfg Config) RuntimeState {
	data, err := os.ReadFile(filepath.Join(cfg.VarDir, "state", "runtime.json"))
	if err != nil {
		return RuntimeState{}
	}
	var state RuntimeState
	_ = json.Unmarshal(data, &state)
	return state
}

func saveRuntimeState(cfg Config, state RuntimeState) error {
	state.UpdatedAt = time.Now()
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	target := filepath.Join(cfg.VarDir, "state", "runtime.json")
	temporary := target + ".tmp"
	if err := os.WriteFile(temporary, data, 0o600); err != nil {
		return err
	}
	return os.Rename(temporary, target)
}

func collectStatus(ctx context.Context, cfg Config, jobs *JobManager) SystemStatus {
	state := loadRuntimeState(cfg)
	port := readPort(cfg)
	installed := fileExists(filepath.Join(cfg.InstallDir, "kvm-console")) || fileExists(cfg.ServiceFile())
	version := ""
	active := false
	enabled := false
	libvirt := false
	ovs := false

	if runtime.GOOS == "linux" {
		active = serviceActive(ctx, cfg.ServiceName)
		enabled = serviceEnabled(ctx, cfg.ServiceName)
		libvirt = anyServiceActive(ctx, "libvirtd.service", "virtqemud.service")
		ovs = anyServiceActive(ctx, "openvswitch-switch.service", "openvswitch.service")
		if active {
			version, _ = fetchTargetVersion(ctx, port)
		}
	}
	if version == "" {
		version = state.Version
	}

	return SystemStatus{
		Installed:       installed,
		ServiceActive:   active,
		ServiceEnabled:  enabled,
		Version:         version,
		Port:            port,
		Channel:         state.Channel,
		KVMAvailable:    fileExists(filepath.Join(cfg.SystemRoot, "dev", "kvm")),
		LibvirtActive:   libvirt,
		OVSActive:       ovs,
		Architecture:    runtime.GOARCH,
		ManagerVersion:  ManagerVersion,
		LastOperation:   state.LastOperation,
		StateUpdatedAt:  state.UpdatedAt,
		ActiveJob:       jobs.Active(),
		DatabasePresent: fileExists(cfg.DatabasePath()),
		NetworkCompat:   loadNetworkCompatibilityState(cfg),
	}
}

func fetchTargetVersion(ctx context.Context, port int) (string, error) {
	client := &http.Client{Timeout: 3 * time.Second}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/api/public/version", port), nil)
	if err != nil {
		return "", err
	}
	response, err := client.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("健康检查返回 HTTP %d", response.StatusCode)
	}
	var payload map[string]any
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&payload); err != nil {
		return "", err
	}
	for _, source := range []map[string]any{payload, nestedMap(payload, "data")} {
		for _, key := range []string{"version", "Version", "appVersion"} {
			if value, ok := source[key].(string); ok && value != "" {
				return value, nil
			}
		}
	}
	return "", errors.New("健康检查响应缺少版本号")
}

func nestedMap(value map[string]any, key string) map[string]any {
	if result, ok := value[key].(map[string]any); ok {
		return result
	}
	return map[string]any{}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func checkPortAvailable(port int) error {
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return fmt.Errorf("端口 %d 已被占用", port)
	}
	return listener.Close()
}

func validatePort(port int) error {
	if port < 1024 || port > 65535 {
		return errors.New("端口必须在 1024 到 65535 之间")
	}
	return nil
}

func controlService(ctx context.Context, cfg Config, action string) error {
	if runtime.GOOS != "linux" {
		return errors.New("服务控制仅支持 Linux")
	}
	if action != "start" && action != "stop" && action != "restart" {
		return errors.New("无效的服务操作")
	}
	output, err := commandOutput(ctx, "systemctl", action, cfg.ServiceName)
	if err != nil {
		return fmt.Errorf("服务%s失败: %s", serviceActionName(action), output)
	}
	return nil
}

func serviceActionName(action string) string {
	switch action {
	case "start":
		return "启动"
	case "stop":
		return "停止"
	default:
		return "重启"
	}
}

func replaceEnvValue(path, key, value string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	found := false
	for index, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, key+"=") {
			lines[index] = key + "=" + value
			found = true
		}
	}
	if !found {
		lines = append(lines, key+"="+value)
	}
	temporary := path + ".manager.tmp"
	if err := os.WriteFile(temporary, []byte(strings.Join(lines, "\n")), 0o600); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}

func changePort(ctx context.Context, cfg Config, port int) error {
	if err := validatePort(port); err != nil {
		return err
	}
	oldPort := readPort(cfg)
	if port == oldPort {
		return nil
	}
	if err := checkPortAvailable(port); err != nil {
		return err
	}
	original, err := os.ReadFile(cfg.EnvPath())
	if err != nil {
		return fmt.Errorf("读取配置失败: %w", err)
	}
	if err := replaceEnvValue(cfg.EnvPath(), "KVM_PORT", strconv.Itoa(port)); err != nil {
		return fmt.Errorf("写入端口失败: %w", err)
	}

	rollback := func() {
		_ = os.WriteFile(cfg.EnvPath(), original, 0o600)
		if _, lookupErr := exec.LookPath("ufw"); lookupErr == nil {
			_, _ = commandOutput(context.Background(), "ufw", "delete", "allow", fmt.Sprintf("%d/tcp", port))
			_, _ = commandOutput(context.Background(), "ufw", "allow", fmt.Sprintf("%d/tcp", oldPort))
		}
		_, _ = commandOutput(context.Background(), "systemctl", "restart", cfg.ServiceName)
	}
	if _, err := exec.LookPath("ufw"); err == nil {
		_, _ = commandOutput(ctx, "ufw", "delete", "allow", fmt.Sprintf("%d/tcp", oldPort))
		if output, err := commandOutput(ctx, "ufw", "allow", fmt.Sprintf("%d/tcp", port)); err != nil {
			rollback()
			return fmt.Errorf("更新防火墙失败: %s", output)
		}
	}
	if err := controlService(ctx, cfg, "restart"); err != nil {
		rollback()
		return err
	}
	healthCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if _, err := waitForHealth(healthCtx, port); err != nil {
		rollback()
		return fmt.Errorf("新端口健康检查失败，已恢复原配置: %w", err)
	}
	state := loadRuntimeState(cfg)
	state.Port = port
	state.LastOperation = "port-change"
	return saveRuntimeState(cfg, state)
}

func waitForHealth(ctx context.Context, port int) (string, error) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	var lastErr error
	for {
		version, err := fetchTargetVersion(ctx, port)
		if err == nil {
			return version, nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("等待服务就绪超时: %v", lastErr)
		case <-ticker.C:
		}
	}
}
