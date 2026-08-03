# 构建与运行依赖

## 构建环境

| 依赖 | 当前版本 | 用途 |
| --- | --- | --- |
| Go | 1.25.4 | 构建 Linux/amd64 静态后端 |
| Node.js | 24.16.0 | 构建 React 管理页面 |
| npm | 11.6.1 | 安装前端依赖 |
| fnpack | 1.2.3 | 生成飞牛 FPK |
| React | 18.3.1 | 管理页面 |
| Semi UI / Icons | 2.102.0 | 飞牛管理界面组件 |
| Vite | 8.2.0 | 前端构建 |
| TypeScript | 5.9.3 | 前端类型检查 |
| modernc.org/sqlite | 1.55.0 | 无 CGO 的 SQLite 用户管理 |
| golang.org/x/crypto | 0.54.0 | bcrypt 密码哈希 |

运行 `powershell -ExecutionPolicy Bypass -File scripts/build.ps1` 可完成前端、后端和 Windows 候选 FPK 构建。

正式发布时，将构建结果同步到 Linux 或飞牛设备，再运行：

```bash
bash scripts/build-fpk.sh
```

该脚本会先校验 manifest 与 Linux 管理器二进制报告的版本完全一致，再将普通文件设为 0644、目录及生命周期脚本和 Linux ELF 设为 0755，最后生成正式 FPK。版本不一致时必须先在本地运行 `scripts/build.ps1` 并等待二进制同步完成，防止新版本 FPK 携带旧代码。

## 管理器运行依赖

管理器后端静态链接，不要求飞牛设备安装 Node.js、Python、sqlite3 或 Python bcrypt。

设备需具备以下系统能力：

- x86_64 Linux 与飞牛 fnOS 1.2.0 及以上；
- systemd、bash、tar、acl（提供 `setfacl`）；
- `/dev/kvm`；
- 可访问 QVMConsole 下载地址和 HIBP 密码泄露检测服务。

首次安装 QVMConsole 时，上游安装脚本可能通过 apt 安装缺失的 libvirt、Open vSwitch、QEMU、UFW 等组件。管理器允许安装缺失依赖及刷新软件索引，但禁止执行系统整体升级。

飞牛自带 libvirt 9.0 对 qcow2 格式 UEFI NVRAM 的 XML 支持不完整，飞牛存储卷还可能使用 `000` 模式配合 ACL 隔离用户目录。管理器通过 libvirt qemu 生命周期钩子做 NVRAM 格式兼容，并使用 `setfacl`、QEMU 主组和最小 mode 为域实际引用的磁盘文件授予访问权限。系统启用 AppArmor 时，管理器还会允许 `virt-aa-helper` 和 libvirt QEMU 配置识别 `/volN` 存储根，并把 QEMU 规则同时写入飞牛实际包含的 local abstraction 与兼容性 drop-in。libvirt 的 DAC 运行时权限管理保持启用，以便正确管理 swtpm 等动态套接字。管理器不会升级或替换飞牛的 libvirt、QEMU、AppArmor、swtpm 和 OVMF 软件包。

## FPK 运行目录

- 管理器 Unix Socket：`${TRIM_APPDEST}/app.sock`
- 管理器日志：`${TRIM_PKGVAR}/manager.log`
- 任务日志：`${TRIM_PKGVAR}/jobs/`
- 下载缓存：`${TRIM_PKGVAR}/cache/`
- 回滚备份：`${TRIM_PKGVAR}/backups/`
- 渠道与任务状态：`${TRIM_PKGVAR}/state/`
- QVMConsole 目标目录：`/opt/kvm-console`
