package app

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

func createSystemBackup(cfg Config, label string) (string, error) {
	backupDir := filepath.Join(cfg.VarDir, "backups")
	if err := os.MkdirAll(backupDir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(backupDir, fmt.Sprintf("%s-%s.tar.gz", time.Now().Format("20060102-150405"), label))
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	gz := gzip.NewWriter(file)
	tw := tar.NewWriter(gz)

	type backupItem struct {
		path string
		name string
	}
	items := []backupItem{
		{path: filepath.Join(cfg.SystemRoot, "etc", "fstab"), name: systemArchiveName(cfg, filepath.Join(cfg.SystemRoot, "etc", "fstab"))},
		{path: cfg.EnvPath(), name: systemArchiveName(cfg, cfg.EnvPath())},
		{path: filepath.Join(cfg.InstallDir, "kvm-console"), name: systemArchiveName(cfg, filepath.Join(cfg.InstallDir, "kvm-console"))},
		{path: filepath.Join(cfg.InstallDir, "kvm-console-native"), name: systemArchiveName(cfg, filepath.Join(cfg.InstallDir, "kvm-console-native"))},
		{path: filepath.Join(cfg.InstallDir, "web-dist"), name: systemArchiveName(cfg, filepath.Join(cfg.InstallDir, "web-dist"))},
		{path: cfg.ServiceFile(), name: systemArchiveName(cfg, cfg.ServiceFile())},
	}
	databasePath := cfg.DatabasePath()
	for _, path := range []string{databasePath, databasePath + "-wal", databasePath + "-shm"} {
		items = append(items, backupItem{path: path, name: systemArchiveName(cfg, path)})
	}
	written := false
	for _, item := range items {
		if !fileExists(item.path) {
			continue
		}
		if err := addBackupPath(tw, item.path, item.name); err != nil {
			_ = tw.Close()
			_ = gz.Close()
			_ = file.Close()
			_ = os.Remove(path)
			return "", err
		}
		written = true
	}
	if err := tw.Close(); err != nil {
		return "", err
	}
	if err := gz.Close(); err != nil {
		return "", err
	}
	if err := file.Close(); err != nil {
		return "", err
	}
	if !written {
		_ = os.Remove(path)
		return "", nil
	}
	pruneBackups(backupDir, 3)
	return path, nil
}

func systemArchiveName(cfg Config, path string) string {
	relative, err := filepath.Rel(filepath.Clean(cfg.SystemRoot), filepath.Clean(path))
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return filepath.ToSlash(filepath.Join("opt", "kvm-console", filepath.Base(path)))
	}
	return filepath.ToSlash(relative)
}

func addBackupPath(tw *tar.Writer, source, archiveName string) error {
	return filepath.Walk(source, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSocket != 0 {
			return nil
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		name := filepath.ToSlash(filepath.Join(archiveName, relative))
		if relative == "." {
			name = filepath.ToSlash(archiveName)
		}
		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		header.Name = name
		if info.Mode()&os.ModeSymlink != 0 {
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			header.Linkname = link
		}
		if err := tw.WriteHeader(header); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(tw, file)
		closeErr := file.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	})
}

func restoreSystemBackup(cfg Config, path string) error {
	if path == "" {
		return errors.New("没有可用备份")
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	gz, err := gzip.NewReader(file)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)

	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		clean := filepath.Clean(filepath.FromSlash(header.Name))
		if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return fmt.Errorf("备份包含非法路径: %s", header.Name)
		}
		target := filepath.Join(cfg.SystemRoot, clean)
		rootClean := filepath.Clean(cfg.SystemRoot)
		targetClean := filepath.Clean(target)
		relativeToRoot, relErr := filepath.Rel(rootClean, targetClean)
		if relErr != nil || relativeToRoot == ".." || strings.HasPrefix(relativeToRoot, ".."+string(filepath.Separator)) || filepath.IsAbs(relativeToRoot) {
			return fmt.Errorf("备份恢复路径越界: %s", header.Name)
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(header.Mode)); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(header.Mode))
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(out, tr)
			closeErr := out.Close()
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
		case tar.TypeSymlink:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			_ = os.Remove(target)
			if err := os.Symlink(header.Linkname, target); err != nil {
				return err
			}
		}
	}
	return nil
}

func pruneBackups(dir string, keep int) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	var files []string
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".tar.gz") {
			files = append(files, filepath.Join(dir, entry.Name()))
		}
	}
	sort.Strings(files)
	for len(files) > keep {
		_ = os.Remove(files[0])
		files = files[1:]
	}
}
