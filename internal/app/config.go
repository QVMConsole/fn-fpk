package app

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
)

const (
	ManagerVersion       = "1.0.30"
	defaultGatewayPrefix = "/app/qvmconsole-manager"
	defaultInstallDir    = "/opt/kvm-console"
	defaultServiceName   = "kvm-console.service"
)

type Config struct {
	SocketPath    string
	GatewayPrefix string
	VarDir        string
	InstallDir    string
	ServiceName   string
	SystemRoot    string
	DevMode       bool
	DevAddress    string
	Catalog       ReleaseCatalog
}

func LoadConfig() (Config, error) {
	varDir := envOr("QVMC_MANAGER_VAR", filepath.Join(".", ".data"))
	catalog, err := loadReleaseCatalog()
	if err != nil {
		return Config{}, err
	}
	cfg := Config{
		SocketPath:    os.Getenv("QVMC_MANAGER_SOCKET"),
		GatewayPrefix: envOr("QVMC_MANAGER_GATEWAY_PREFIX", defaultGatewayPrefix),
		VarDir:        varDir,
		InstallDir:    envOr("QVMC_INSTALL_DIR", defaultInstallDir),
		ServiceName:   envOr("QVMC_SERVICE_NAME", defaultServiceName),
		SystemRoot:    envOr("QVMC_SYSTEM_ROOT", string(filepath.Separator)),
		DevMode:       os.Getenv("QVMC_MANAGER_DEV") == "1",
		DevAddress:    envOr("QVMC_MANAGER_DEV_ADDRESS", "127.0.0.1:18990"),
		Catalog:       catalog,
	}

	if cfg.SocketPath == "" && !cfg.DevMode {
		return Config{}, errors.New("缺少 QVMC_MANAGER_SOCKET")
	}
	if runtime.GOOS == "windows" && cfg.SocketPath != "" && !cfg.DevMode {
		return Config{}, errors.New("Windows 环境仅支持开发模式")
	}
	for _, name := range []string{"cache", "jobs", "backups", "state"} {
		if err := os.MkdirAll(filepath.Join(cfg.VarDir, name), 0o700); err != nil {
			return Config{}, err
		}
	}
	return cfg, nil
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func (c Config) EnvPath() string {
	return filepath.Join(c.InstallDir, ".env")
}

func (c Config) DatabasePath() string {
	if value := readEnvValue(c.EnvPath(), "KVM_DB_PATH"); value != "" {
		return value
	}
	return filepath.Join(c.InstallDir, "data", "kvm_console.db")
}

func (c Config) ServiceFile() string {
	return filepath.Join(c.SystemRoot, "etc", "systemd", "system", c.ServiceName)
}
