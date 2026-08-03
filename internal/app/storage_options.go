package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"runtime"
	"sort"
	"strconv"
	"strings"
)

const (
	defaultUserStorageDir = "/var/lib"
	userStorageMount      = "/var/lib/kvm-user-storage"
)

type StorageOption struct {
	Path           string `json:"path"`
	Source         string `json:"source,omitempty"`
	Filesystem     string `json:"filesystem,omitempty"`
	TotalBytes     uint64 `json:"totalBytes,omitempty"`
	AvailableBytes uint64 `json:"availableBytes,omitempty"`
	Default        bool   `json:"default"`
}

type findmntDocument struct {
	Filesystems []findmntFilesystem `json:"filesystems"`
}

type findmntFilesystem struct {
	Source  string          `json:"source"`
	Target  string          `json:"target"`
	FSType  string          `json:"fstype"`
	Options string          `json:"options"`
	Size    json.RawMessage `json:"size"`
	Avail   json.RawMessage `json:"avail"`
}

func listStorageOptions(ctx context.Context) ([]StorageOption, error) {
	if runtime.GOOS != "linux" {
		return []StorageOption{{Path: defaultUserStorageDir, Default: true}}, nil
	}

	output, err := commandOutput(ctx, "findmnt", "--json", "--list", "--bytes", "--types", "ext4,xfs,btrfs", "--output", "SOURCE,TARGET,FSTYPE,OPTIONS,SIZE,AVAIL")
	if err != nil {
		return nil, fmt.Errorf("读取本地存储空间失败: %s", output)
	}
	rootTarget, err := commandOutput(ctx, "findmnt", "--noheadings", "--raw", "--output", "TARGET", "--target", defaultUserStorageDir)
	if err != nil || rootTarget == "" {
		return nil, errors.New("无法确定根目录所在的存储空间")
	}
	return parseStorageOptions([]byte(output), path.Clean(rootTarget))
}

func parseStorageOptions(data []byte, rootTarget string) ([]StorageOption, error) {
	var document findmntDocument
	if err := json.Unmarshal(data, &document); err != nil {
		return nil, fmt.Errorf("解析本地存储空间失败: %w", err)
	}

	defaultOption := StorageOption{Path: defaultUserStorageDir, Default: true}
	options := make([]StorageOption, 0, len(document.Filesystems)+1)
	seen := make(map[string]struct{}, len(document.Filesystems))
	for _, filesystem := range document.Filesystems {
		target := path.Clean(strings.TrimSpace(filesystem.Target))
		if target == rootTarget {
			defaultOption.Source = filesystem.Source
			defaultOption.Filesystem = filesystem.FSType
			defaultOption.TotalBytes = parseFindmntBytes(filesystem.Size)
			defaultOption.AvailableBytes = parseFindmntBytes(filesystem.Avail)
			continue
		}
		if !path.IsAbs(target) || target == userStorageMount || !strings.HasPrefix(filesystem.Source, "/dev/") {
			continue
		}
		if !supportedStorageFilesystem(filesystem.FSType) || mountOptionPresent(filesystem.Options, "ro") {
			continue
		}
		if _, exists := seen[target]; exists {
			continue
		}
		seen[target] = struct{}{}
		options = append(options, StorageOption{
			Path:           target,
			Source:         filesystem.Source,
			Filesystem:     filesystem.FSType,
			TotalBytes:     parseFindmntBytes(filesystem.Size),
			AvailableBytes: parseFindmntBytes(filesystem.Avail),
		})
	}

	sort.Slice(options, func(i, j int) bool { return options[i].Path < options[j].Path })
	return append([]StorageOption{defaultOption}, options...), nil
}

func normalizeStorageDirectory(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return defaultUserStorageDir, nil
	}
	if len(value) > 4096 || strings.ContainsAny(value, "\x00\r\n") || !path.IsAbs(value) {
		return "", errors.New("用户存储空间路径无效")
	}
	cleaned := path.Clean(value)
	if cleaned == "/" || cleaned == userStorageMount {
		return "", errors.New("不能使用该目录存放用户存储镜像")
	}
	return cleaned, nil
}

func resolveStorageDirectory(ctx context.Context, value string) (StorageOption, error) {
	requested, err := normalizeStorageDirectory(value)
	if err != nil {
		return StorageOption{}, err
	}
	options, err := listStorageOptions(ctx)
	if err != nil {
		return StorageOption{}, err
	}
	for _, option := range options {
		if option.Path == requested {
			return option, nil
		}
	}
	return StorageOption{}, fmt.Errorf("选择的用户存储空间已不可用: %s", requested)
}

func supportedStorageFilesystem(value string) bool {
	switch value {
	case "ext4", "xfs", "btrfs":
		return true
	default:
		return false
	}
}

func mountOptionPresent(options, expected string) bool {
	for _, option := range strings.Split(options, ",") {
		if strings.TrimSpace(option) == expected {
			return true
		}
	}
	return false
}

func parseFindmntBytes(value json.RawMessage) uint64 {
	text := strings.Trim(strings.TrimSpace(string(value)), `"`)
	parsed, err := strconv.ParseUint(text, 10, 64)
	if err != nil {
		return 0
	}
	return parsed
}
