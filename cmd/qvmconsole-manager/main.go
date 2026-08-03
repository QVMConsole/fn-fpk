package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"time"

	"qvmconsole-manager/internal/app"
)

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "version" || os.Args[1] == "--version") {
		fmt.Println(app.ManagerVersion)
		return
	}
	if len(os.Args) > 1 && (os.Args[1] == "qemu-hook" || os.Args[1] == "nvram-hook") {
		os.Exit(app.RunQEMUHook(os.Args[2:], os.Stdin, os.Stdout, os.Stderr))
	}
	if len(os.Args) > 1 && os.Args[1] == "storage-path" {
		os.Exit(app.RunStoragePathCompatibility(os.Args[2:], os.Stdout, os.Stderr))
	}
	if len(os.Args) > 1 && os.Args[1] == "storage-xml" {
		os.Exit(app.RunStorageXMLCompatibility(os.Stdin, os.Stdout, os.Stderr))
	}

	cfg, err := app.LoadConfig()
	if err != nil {
		log.Fatalf("加载配置失败: %v", err)
	}
	if len(os.Args) > 1 && os.Args[1] == "network-compat" {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		state, compatErr := app.EnsureNetworkCompatibility(ctx, cfg)
		_ = json.NewEncoder(os.Stdout).Encode(state)
		if compatErr != nil {
			log.Printf("配置飞牛网络兼容层失败: %v", compatErr)
			os.Exit(1)
		}
		return
	}
	compatCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	if err := app.EnsureLibvirtCompatibility(compatCtx, cfg); err != nil {
		log.Printf("初始化飞牛 libvirt 兼容组件失败: %v", err)
	}
	cancel()

	monitorCtx, stopMonitor := context.WithCancel(context.Background())
	defer stopMonitor()
	app.StartNetworkCompatibilityMonitor(monitorCtx, cfg, log.Printf)

	server, err := app.NewServer(cfg)
	if err != nil {
		log.Fatalf("初始化管理器失败: %v", err)
	}

	if err := server.Run(); err != nil {
		log.Printf("管理器退出: %v", err)
		os.Exit(1)
	}
}
