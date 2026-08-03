package app

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
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

type JobRequest struct {
	Action     string `json:"action"`
	Channel    string `json:"channel,omitempty"`
	Port       int    `json:"port,omitempty"`
	StorageDir string `json:"storageDir,omitempty"`
	Purge      bool   `json:"purge,omitempty"`
}

type JobSnapshot struct {
	ID         string     `json:"id"`
	Action     string     `json:"action"`
	Channel    string     `json:"channel,omitempty"`
	Status     string     `json:"status"`
	Progress   int        `json:"progress"`
	Message    string     `json:"message"`
	Error      string     `json:"error,omitempty"`
	CreatedAt  time.Time  `json:"createdAt"`
	StartedAt  *time.Time `json:"startedAt,omitempty"`
	FinishedAt *time.Time `json:"finishedAt,omitempty"`
}

type managedJob struct {
	JobSnapshot
	request     JobRequest
	logPath     string
	subscribers map[chan string]struct{}
}

type JobManager struct {
	cfg    Config
	mu     sync.RWMutex
	jobs   map[string]*managedJob
	active string
	client *http.Client
}

func NewJobManager(cfg Config) *JobManager {
	manager := &JobManager{
		cfg:    cfg,
		jobs:   make(map[string]*managedJob),
		client: &http.Client{Timeout: 30 * time.Minute},
	}
	manager.loadIndex()
	return manager
}

func (m *JobManager) Start(request JobRequest) (JobSnapshot, error) {
	if err := validateJobRequest(request, m.cfg.Catalog); err != nil {
		return JobSnapshot{}, err
	}
	m.mu.Lock()
	if m.active != "" {
		m.mu.Unlock()
		return JobSnapshot{}, errors.New("已有安装维护任务正在运行")
	}
	now := time.Now()
	id := now.Format("20060102-150405.000000000")
	job := &managedJob{
		JobSnapshot: JobSnapshot{
			ID:        id,
			Action:    request.Action,
			Channel:   request.Channel,
			Status:    "queued",
			Progress:  0,
			Message:   "任务已进入队列",
			CreatedAt: now,
		},
		request:     request,
		logPath:     filepath.Join(m.cfg.VarDir, "jobs", id+".log"),
		subscribers: make(map[chan string]struct{}),
	}
	m.jobs[id] = job
	m.active = id
	m.mu.Unlock()
	m.saveIndex()
	go m.run(job)
	return job.JobSnapshot, nil
}

func validateJobRequest(request JobRequest, catalog ReleaseCatalog) error {
	switch request.Action {
	case "install", "update", "switch":
		if _, ok := catalog.FindChannel(request.Channel); !ok {
			return errors.New("请选择有效的发行渠道")
		}
		if request.Action == "install" {
			if request.Port == 0 {
				request.Port = 8080
			}
			if err := validatePort(request.Port); err != nil {
				return err
			}
			if _, err := normalizeStorageDirectory(request.StorageDir); err != nil {
				return err
			}
		} else if request.StorageDir != "" {
			return errors.New("用户存储空间只能在首次安装时选择")
		}
	case "repair", "uninstall":
	default:
		return errors.New("无效的任务类型")
	}
	return nil
}

func (m *JobManager) Active() *JobSnapshot {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.active == "" {
		return nil
	}
	job := m.jobs[m.active]
	if job == nil {
		return nil
	}
	copy := job.JobSnapshot
	return &copy
}

func (m *JobManager) Get(id string) (JobSnapshot, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	job, ok := m.jobs[id]
	if !ok {
		return JobSnapshot{}, false
	}
	return job.JobSnapshot, true
}

func (m *JobManager) Recent() []JobSnapshot {
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := make([]JobSnapshot, 0, len(m.jobs))
	for _, job := range m.jobs {
		result = append(result, job.JobSnapshot)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].CreatedAt.After(result[j].CreatedAt) })
	if len(result) > 20 {
		result = result[:20]
	}
	return result
}

func (m *JobManager) Subscribe(id string) (<-chan string, func(), error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	job, ok := m.jobs[id]
	if !ok {
		return nil, nil, errors.New("任务不存在")
	}
	channel := make(chan string, 64)
	job.subscribers[channel] = struct{}{}
	if data, err := os.ReadFile(job.logPath); err == nil && len(data) > 0 {
		channel <- string(data)
	}
	cancel := func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		if _, exists := job.subscribers[channel]; exists {
			delete(job.subscribers, channel)
			close(channel)
		}
	}
	return channel, cancel, nil
}

func (m *JobManager) update(job *managedJob, progress int, message string) {
	m.mu.Lock()
	job.Progress = progress
	job.Message = message
	m.mu.Unlock()
	m.saveIndex()
	m.log(job, message)
}

func (m *JobManager) log(job *managedJob, message string) {
	message = redactLog(message)
	line := fmt.Sprintf("[%s] %s\n", time.Now().Format("15:04:05"), strings.TrimRight(message, "\r\n"))
	file, err := os.OpenFile(job.logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err == nil {
		_, _ = file.WriteString(line)
		_ = file.Close()
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	for subscriber := range job.subscribers {
		select {
		case subscriber <- line:
		default:
		}
	}
}

func redactLog(value string) string {
	for _, key := range []string{"KVM_SECURITY_SECRET", "KVM_ADMIN_PASS", "PASSWORD", "TOKEN"} {
		searchFrom := 0
		for {
			upper := strings.ToUpper(value)
			relativeIndex := strings.Index(upper[searchFrom:], key+"=")
			if relativeIndex < 0 {
				break
			}
			index := searchFrom + relativeIndex
			end := strings.IndexAny(value[index:], " \r\n")
			if end < 0 {
				end = len(value) - index
			}
			value = value[:index] + key + "=***" + value[index+end:]
			searchFrom = index + len(key) + len("=***")
			if searchFrom >= len(value) {
				break
			}
		}
	}
	for _, pattern := range []*regexp.Regexp{
		regexp.MustCompile(`(?i)(--password\s+[^:\s"']+:password:)([^\s"']+)`),
		regexp.MustCompile(`(?i)(\b[^:\s"']+:password:)([^\s"']+)`),
		regexp.MustCompile(`(?i)(["']?(?:password|token|secret)["']?\s*[:=]\s*["']?)([^,\s"']+)`),
	} {
		value = pattern.ReplaceAllString(value, `${1}***`)
	}
	return value
}

func (m *JobManager) run(job *managedJob) {
	now := time.Now()
	m.mu.Lock()
	job.Status = "running"
	job.StartedAt = &now
	m.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := m.execute(ctx, job)
	finished := time.Now()
	m.mu.Lock()
	job.FinishedAt = &finished
	if err != nil {
		job.Status = "failed"
		job.Error = err.Error()
		job.Message = "任务执行失败"
	} else {
		job.Status = "succeeded"
		job.Progress = 100
		job.Message = "任务执行完成"
	}
	m.active = ""
	m.mu.Unlock()
	m.saveIndex()
	if err != nil {
		m.log(job, "错误: "+err.Error())
	} else {
		m.log(job, "任务执行完成")
	}
}

func (m *JobManager) execute(ctx context.Context, job *managedJob) error {
	request := job.request
	if err := m.preflight(ctx, request); err != nil {
		return err
	}
	m.update(job, 8, "运行环境检查通过")

	var backup string
	preservedInstall := request.Action == "install" && (fileExists(filepath.Join(m.cfg.InstallDir, ".env")) || fileExists(filepath.Join(m.cfg.InstallDir, "data")))
	fstabPath := filepath.Join(m.cfg.SystemRoot, "etc", "fstab")
	needsBackup := request.Action != "uninstall" && (fileExists(m.cfg.InstallDir) || fileExists(fstabPath))
	if needsBackup {
		m.update(job, 12, "正在创建回滚备份")
		wasActive := serviceActive(ctx, m.cfg.ServiceName)
		if wasActive {
			if err := controlService(ctx, m.cfg, "stop"); err != nil {
				return fmt.Errorf("停止服务以创建一致性备份失败: %w", err)
			}
		}
		var err error
		backupLabel := request.Action
		if preservedInstall {
			backupLabel = "reinstall"
		}
		backup, err = createSystemBackup(m.cfg, backupLabel)
		if wasActive {
			if startErr := controlService(ctx, m.cfg, "start"); err == nil && startErr != nil {
				err = fmt.Errorf("备份完成后恢复服务失败: %w", startErr)
			}
		}
		if err != nil {
			return fmt.Errorf("创建备份失败: %w", err)
		}
		if backup != "" {
			m.log(job, "备份已创建: "+backup)
		}
	}

	rollback := func(reason error) error {
		if backup == "" || request.Action == "uninstall" {
			return reason
		}
		m.log(job, "操作失败，正在恢复备份")
		_ = controlService(context.Background(), m.cfg, "stop")
		_ = os.RemoveAll(m.cfg.InstallDir)
		_ = os.Remove(m.cfg.ServiceFile())
		if restoreErr := restoreSystemBackup(m.cfg, backup); restoreErr != nil {
			return fmt.Errorf("%v；恢复备份同时失败: %v", reason, restoreErr)
		}
		if _, compatErr := EnsureUserStorageMountCompatibility(context.Background(), m.cfg); compatErr != nil {
			return fmt.Errorf("%v；已恢复操作前备份，但修复用户存储启动挂载失败: %v", reason, compatErr)
		}
		_, _ = commandOutput(context.Background(), "systemctl", "daemon-reload")
		_ = controlService(context.Background(), m.cfg, "restart")
		return fmt.Errorf("%v；已恢复操作前备份", reason)
	}

	var archivePath string
	var channel Channel
	if request.Action == "install" || request.Action == "update" || request.Action == "switch" {
		channel, _ = m.cfg.Catalog.FindChannel(request.Channel)
		m.update(job, 18, "正在准备受信任的无人值守脚本")
		scriptPath, err := m.downloadArtifact(ctx, job, m.cfg.Catalog.Executor, true)
		if err != nil {
			return rollback(err)
		}
		if err := validateScriptPolicy(scriptPath); err != nil {
			return rollback(err)
		}
		if request.Action == "install" {
			if err := validateScriptStorageCLI(scriptPath); err != nil {
				return rollback(err)
			}
		}
		m.update(job, 34, "正在下载"+channel.Name+"发行包")
		archivePath, err = m.downloadArtifact(ctx, job, channel.artifact, true)
		if err != nil {
			return rollback(err)
		}
		if err := validateReleaseArchive(archivePath); err != nil {
			return rollback(err)
		}
		m.update(job, 58, "发行包校验通过")
		mode := request.Action
		if mode == "switch" {
			mode = "update"
		}
		args := []string{scriptPath, "--non-interactive", "--mode", mode, "--release-source", archivePath}
		if request.Action == "install" {
			port := request.Port
			if port == 0 {
				port = 8080
			}
			storage, err := resolveStorageDirectory(ctx, request.StorageDir)
			if err != nil {
				return rollback(err)
			}
			m.log(job, "用户存储空间: "+storage.Path)
			args = append(args, "--port", fmt.Sprintf("%d", port), "--storage-dir", storage.Path)
		}
		m.update(job, 64, "正在执行"+jobActionName(request.Action))
		if err := m.runCommand(ctx, job, nil, "bash", args...); err != nil {
			return rollback(err)
		}
		if preservedInstall && backup != "" {
			m.update(job, 78, "正在恢复保留的配置和数据")
			if err := restoreSystemBackup(m.cfg, backup); err != nil {
				return rollback(fmt.Errorf("恢复保留配置失败: %w", err))
			}
			_, _ = commandOutput(ctx, "systemctl", "daemon-reload")
			if err := controlService(ctx, m.cfg, "restart"); err != nil {
				return rollback(err)
			}
		}
	} else if request.Action == "repair" {
		m.update(job, 35, "正在准备修复脚本")
		scriptPath, err := m.downloadArtifact(ctx, job, m.cfg.Catalog.Executor, false)
		if err != nil {
			return rollback(err)
		}
		if err := validateScriptPolicy(scriptPath); err != nil {
			return rollback(err)
		}
		m.update(job, 55, "正在修复 QVMConsole 配置")
		if err := m.runCommand(ctx, job, strings.NewReader("y\n"), "bash", scriptPath, "--non-interactive", "--mode", "repair"); err != nil {
			return rollback(err)
		}
	} else if request.Action == "uninstall" {
		m.update(job, 35, "正在准备卸载脚本")
		scriptPath, err := m.downloadArtifact(ctx, job, m.cfg.Catalog.Executor, false)
		if err != nil {
			return err
		}
		if err := validateScriptPolicy(scriptPath); err != nil {
			return err
		}
		purgeAnswer := "N"
		if request.Purge {
			purgeAnswer = "Y"
		}
		input := strings.NewReader("UNINSTALL\nY\n" + purgeAnswer + "\n")
		m.update(job, 58, "正在卸载 QVMConsole")
		if err := m.runCommand(ctx, job, input, "bash", scriptPath, "--non-interactive", "--mode", "uninstall"); err != nil {
			return err
		}
		if _, err := RemoveUserStorageMountCompatibility(m.cfg); err != nil {
			return err
		}
		_, _ = commandOutput(ctx, "systemctl", "daemon-reload")
		state := loadRuntimeState(m.cfg)
		state.Channel = ""
		state.Version = ""
		state.LastOperation = "uninstall"
		return saveRuntimeState(m.cfg, state)
	}

	m.update(job, 80, "正在配置用户存储安全启动挂载")
	fstabChanged, err := EnsureUserStorageMountCompatibility(ctx, m.cfg)
	if err != nil {
		return rollback(err)
	}
	if fstabChanged {
		m.log(job, "已将用户存储迁移为不阻断 fnOS 启动的自动挂载")
		_, _ = commandOutput(ctx, "systemctl", "daemon-reload")
	}

	m.update(job, 82, "正在验证 Linux 离线初始化环境")
	if err := ensureLibguestfsAppliance(ctx, m.cfg); err != nil {
		return rollback(err)
	}
	m.log(job, "libguestfs appliance 自检通过")

	m.update(job, 84, "正在配置飞牛 libvirt 启动兼容层")
	if err := EnsureLibvirtCompatibility(ctx, m.cfg); err != nil {
		return rollback(err)
	}
	m.log(job, "飞牛虚拟磁盘 ACL、AppArmor 与 UEFI NVRAM 兼容层已就绪")

	m.update(job, 85, "正在配置飞牛虚拟机网络兼容层")
	networkState, err := EnsureNetworkCompatibility(ctx, m.cfg)
	if err != nil {
		return rollback(err)
	}
	if networkState.Enabled {
		m.log(job, fmt.Sprintf("飞牛网络兼容层已就绪: %s / %s", networkState.Network, networkState.Bridge))
	}
	if len(networkState.PendingRestart) > 0 {
		m.log(job, "以下虚拟机需要重启后启用兼容网卡: "+strings.Join(networkState.PendingRestart, ", "))
	}

	m.update(job, 86, "正在等待 QVMConsole 服务就绪")
	port := readPort(m.cfg)
	healthCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	version, err := waitForHealth(healthCtx, port)
	if err != nil {
		return rollback(err)
	}
	state := loadRuntimeState(m.cfg)
	if channel.ID != "" {
		state.Channel = channel.ID
	}
	state.Version = version
	state.Port = port
	state.LastOperation = request.Action
	if err := saveRuntimeState(m.cfg, state); err != nil {
		return rollback(err)
	}
	m.update(job, 96, fmt.Sprintf("服务健康检查通过，版本 %s", version))
	return nil
}

func (m *JobManager) preflight(ctx context.Context, request JobRequest) error {
	if runtime.GOOS != "linux" {
		return errors.New("安装维护任务仅支持 Linux")
	}
	if runtime.GOARCH != "amd64" {
		return fmt.Errorf("当前架构 %s 不受支持，仅支持 x86_64", runtime.GOARCH)
	}
	if os.Geteuid() != 0 {
		return errors.New("管理器需要 root 权限执行安装维护任务")
	}
	if !fileExists(filepath.Join(m.cfg.SystemRoot, "dev", "kvm")) && request.Action == "install" {
		return errors.New("未检测到 /dev/kvm，请先在 BIOS/UEFI 中启用虚拟化")
	}
	for _, command := range []string{"bash", "tar", "systemctl"} {
		if _, err := exec.LookPath(command); err != nil {
			return fmt.Errorf("缺少必要命令: %s", command)
		}
	}
	if request.Action == "install" {
		port := request.Port
		if port == 0 {
			port = 8080
		}
		if err := validatePort(port); err != nil {
			return err
		}
		if err := checkPortAvailable(port); err != nil {
			return err
		}
		if _, err := resolveStorageDirectory(ctx, request.StorageDir); err != nil {
			return err
		}
	}
	return nil
}

func (m *JobManager) downloadArtifact(ctx context.Context, job *managedJob, source artifactSource, forceRefresh bool) (string, error) {
	expectedHash, err := m.fetchArtifactSHA256(ctx, source.URL)
	if err != nil {
		return "", err
	}
	path := filepath.Join(m.cfg.VarDir, "cache", source.CacheKey)
	if !forceRefresh {
		if actual, err := fileSHA256(path); err == nil && strings.EqualFold(actual, expectedHash) {
			m.log(job, "使用已校验缓存: "+source.CacheKey)
			return path, nil
		}
	}
	if forceRefresh {
		m.log(job, "正在重新下载最新发布文件: "+source.CacheKey)
	}
	_ = os.Remove(path)
	temporary := path + ".download"
	_ = os.Remove(temporary)
	request, err := newNoCacheRequest(ctx, http.MethodGet, source.URL)
	if err != nil {
		return "", err
	}
	request.Header.Set("User-Agent", "QVMConsole-Manager/"+ManagerVersion)
	response, err := m.client.Do(request)
	if err != nil {
		return "", fmt.Errorf("下载失败: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("下载返回 HTTP %d", response.StatusCode)
	}
	file, err := os.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	hash := sha256.New()
	_, copyErr := io.Copy(io.MultiWriter(file, hash), io.LimitReader(response.Body, 512<<20))
	closeErr := file.Close()
	if copyErr != nil {
		_ = os.Remove(temporary)
		return "", copyErr
	}
	if closeErr != nil {
		_ = os.Remove(temporary)
		return "", closeErr
	}
	actual := hex.EncodeToString(hash.Sum(nil))
	if !strings.EqualFold(actual, expectedHash) {
		_ = os.Remove(temporary)
		return "", fmt.Errorf("发布文件在下载过程中已变化，请重新执行更新")
	}
	if err := os.Rename(temporary, path); err != nil {
		_ = os.Remove(temporary)
		return "", err
	}
	m.log(job, "下载并校验完成: "+source.CacheKey)
	return path, nil
}

func (m *JobManager) fetchArtifactSHA256(ctx context.Context, rawURL string) (string, error) {
	metadataCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	request, err := newNoCacheRequest(metadataCtx, http.MethodHead, rawURL)
	if err != nil {
		return "", err
	}
	request.Header.Set("User-Agent", "QVMConsole-Manager/"+ManagerVersion)
	response, err := m.client.Do(request)
	if err != nil {
		return "", fmt.Errorf("获取最新发布元数据失败: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("获取最新发布元数据返回 HTTP %d", response.StatusCode)
	}
	checksum, err := parseArtifactETag(response.Header.Get("ETag"))
	if err != nil {
		return "", err
	}
	return checksum, nil
}

func newNoCacheRequest(ctx context.Context, method, rawURL string) (*http.Request, error) {
	request, err := http.NewRequestWithContext(ctx, method, rawURL, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Cache-Control", "no-cache")
	request.Header.Set("Pragma", "no-cache")
	return request, nil
}

func parseArtifactETag(value string) (string, error) {
	value = strings.TrimSpace(value)
	value = strings.TrimPrefix(value, "W/")
	value = strings.Trim(value, "\"")
	const prefix = "sha256-"
	if !strings.HasPrefix(strings.ToLower(value), prefix) {
		return "", errors.New("发布端未提供 SHA256 ETag，已拒绝下载")
	}
	checksum := value[len(prefix):]
	if len(checksum) != sha256.Size*2 {
		return "", errors.New("发布端提供的 SHA256 ETag 格式无效")
	}
	if _, err := hex.DecodeString(checksum); err != nil {
		return "", errors.New("发布端提供的 SHA256 ETag 格式无效")
	}
	return strings.ToLower(checksum), nil
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func validateScriptPolicy(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	content := strings.ToLower(string(data))
	if !strings.Contains(content, "#!/") || !strings.Contains(content, "--non-interactive") {
		return errors.New("安装脚本结构校验失败")
	}
	for _, forbidden := range []string{"apt upgrade", "apt-get upgrade", "dist-upgrade", "full-upgrade"} {
		if strings.Contains(content, forbidden) {
			return fmt.Errorf("安装脚本包含被禁止的系统升级操作: %s", forbidden)
		}
	}
	return nil
}

func validateScriptStorageCLI(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if !strings.Contains(string(data), "--storage-dir)") {
		return errors.New("安装脚本不支持用户存储空间参数，请等待发布端更新后重试")
	}
	return nil
}

func validateReleaseArchive(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	gz, err := gzip.NewReader(file)
	if err != nil {
		return fmt.Errorf("发行包不是有效的 gzip: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	foundBinary := false
	foundWeb := false
	entries := 0
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("读取发行包失败: %w", err)
		}
		entries++
		if entries > 100000 {
			return errors.New("发行包文件数量异常")
		}
		name := filepath.Clean(filepath.FromSlash(header.Name))
		if filepath.IsAbs(name) || name == ".." || strings.HasPrefix(name, ".."+string(filepath.Separator)) {
			return fmt.Errorf("发行包包含非法路径: %s", header.Name)
		}
		base := filepath.Base(name)
		if base == "kvm-console" && header.Typeflag == tar.TypeReg && header.Mode&0o111 != 0 {
			foundBinary = true
		}
		if strings.Contains(filepath.ToSlash(name), "/web-dist/") || strings.HasSuffix(filepath.ToSlash(name), "/web-dist") {
			foundWeb = true
		}
	}
	if !foundBinary || !foundWeb {
		return errors.New("发行包缺少可执行 kvm-console 或 web-dist")
	}
	return nil
}

func (m *JobManager) runCommand(ctx context.Context, job *managedJob, input io.Reader, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), "LANG=en_US.UTF-8", "LC_ALL=en_US.UTF-8")
	cmd.Stdin = input
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		return err
	}
	scanner := bufio.NewScanner(pipe)
	buffer := make([]byte, 64*1024)
	scanner.Buffer(buffer, 1024*1024)
	for scanner.Scan() {
		m.log(job, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		_ = cmd.Process.Kill()
		return err
	}
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("命令执行失败: %w", err)
	}
	return nil
}

func jobActionName(action string) string {
	switch action {
	case "install":
		return "首次安装"
	case "update":
		return "更新"
	case "switch":
		return "版本切换"
	case "repair":
		return "修复"
	case "uninstall":
		return "卸载"
	default:
		return action
	}
}

func (m *JobManager) ClearCache() error {
	if m.Active() != nil {
		return errors.New("任务运行期间不能清理缓存")
	}
	cache := filepath.Join(m.cfg.VarDir, "cache")
	if err := os.RemoveAll(cache); err != nil {
		return err
	}
	return os.MkdirAll(cache, 0o700)
}

func (m *JobManager) LogPath(id string) (string, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	job, ok := m.jobs[id]
	if !ok {
		return "", false
	}
	return job.logPath, true
}

func (m *JobManager) saveIndex() {
	data, err := json.MarshalIndent(m.Recent(), "", "  ")
	if err != nil {
		return
	}
	path := filepath.Join(m.cfg.VarDir, "state", "jobs.json")
	temporary := path + ".tmp"
	if os.WriteFile(temporary, data, 0o600) == nil {
		_ = os.Rename(temporary, path)
	}
}

func (m *JobManager) loadIndex() {
	data, err := os.ReadFile(filepath.Join(m.cfg.VarDir, "state", "jobs.json"))
	if err != nil {
		return
	}
	var snapshots []JobSnapshot
	if json.Unmarshal(data, &snapshots) != nil {
		return
	}
	now := time.Now()
	for _, snapshot := range snapshots {
		if snapshot.Status == "queued" || snapshot.Status == "running" {
			snapshot.Status = "failed"
			snapshot.Error = "管理器在任务执行期间重新启动，原任务已终止"
			snapshot.Message = "任务因管理器重启而终止"
			snapshot.FinishedAt = &now
		}
		copy := snapshot
		m.jobs[copy.ID] = &managedJob{
			JobSnapshot: copy,
			logPath:     filepath.Join(m.cfg.VarDir, "jobs", copy.ID+".log"),
			subscribers: make(map[chan string]struct{}),
		}
	}
}
