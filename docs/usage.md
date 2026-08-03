# QVMConsole 飞牛管理器使用说明

## 功能与边界

QVMConsole 管理器是独立的飞牛 fnOS FPK。FPK 只包含管理器，不包含 QVMConsole 安装脚本、发行压缩包、后端二进制或前端文件。

- 管理器安装、升级不会停止、覆盖或删除 `/opt/kvm-console`。从应用中心卸载管理器时，默认也会保留；只有明确选择联动卸载才会删除。
- QVMConsole 发行文件只会在管理员发起安装、更新或版本切换时下载到管理器缓存。
- 首期仅支持 x86_64 飞牛设备。
- 管理器入口只对飞牛管理员显示，API 通过统一网关和 Unix Socket 提供。

## 安装 FPK

1. 在飞牛应用中心选择手动安装。
2. 上传 `qvmconsole-manager-1.0.29-x86_64.fpk`。
3. 安装完成后，从飞牛桌面打开“QVMConsole 管理器”。
4. 确认概览页中的 KVM、libvirt 和 Open vSwitch 状态。

也可以在设备终端使用：

```bash
appcenter-cli install-fpk /path/to/qvmconsole-manager-1.0.29-x86_64.fpk --volume 1
appcenter-cli start qvmconsole-manager
```

包含生命周期脚本变更的发布包必须在飞牛或其他 Linux 环境执行 `bash scripts/build-fpk.sh` 生成。Windows `build.ps1` 用于本地候选构建，不能替代 Linux 构建的 POSIX 文件权限。

管理器使用 root 权限是因为它需要调用 systemd、安装缺失依赖、维护 `/opt/kvm-console` 和修改 QVMConsole 用户数据库。

## 首次安装 QVMConsole

1. 打开“安装与维护”。
2. 选择“开源版”或“赞助版”。
3. 设置服务端口，范围为 1024～65535。
4. 选择“用户存储空间”。默认使用根目录，也可以选择已挂载、可写的本地存储卷。
5. 阅读“安装前须知”，完成重要数据备份，并确认接受飞牛兼容性限制及系统变更风险。
6. 确认允许执行 `apt update` 和安装缺失依赖。
7. 点击安装并查看实时日志。任务执行期间，日志视图会始终自动滚动到最新输出。

### 用户存储空间

1.0.29 起，首次安装会列出根目录以及已挂载、可写的本地 `ext4`、`xfs` 或 `btrfs` 文件系统。列表显示设备、挂载点和可用容量，远程文件系统、只读文件系统和 QVMConsole 自身的配额挂载点不会显示。

所选挂载点会通过受校验的 `--storage-dir` 参数传给上游安装脚本，用户存储镜像创建为 `<挂载点>/kvm-user-storage.img`；配额挂载点仍为 `/var/lib/kvm-user-storage`。任务入队和脚本执行前都会重新扫描挂载状态，已卸载或变为只读的存储空间会使安装安全终止。

若检测到已有用户存储镜像，上游脚本会继续复用现有路径，不会根据本次选择迁移或覆盖数据。

### 安装前须知

- QVMConsole 不调用飞牛官方虚拟机功能，而是独立基于 Linux libvirt 对接，以提供更底层的虚拟机能力。该方案通常可在发行版 Linux 上正常运行，但在飞牛系统上可能出现异常。
- 安装会修改部分系统设置，并安装或调整相关依赖。继续前请完成重要数据备份，并确认能够接受由此产生的风险。
- 飞牛内置的 libvirtd 与 QEMU 版本较旧，不在 QVMConsole 的兼容范围内。因此，虚拟机创建、网络、启动和管理等功能可能存在问题。QVMConsole 后续不会针对此版本做兼容，需要飞牛官方升级相关组件。

管理器会依次执行环境检查、获取发布端当前 SHA256 元数据、下载并校验安装脚本和发行包、检查 tar 路径与必需文件、执行无人值守安装和 `/api/public/version` 健康检查。

管理器禁止上游脚本执行 `apt upgrade`、`apt-get upgrade`、`dist-upgrade` 和 `full-upgrade`。

## 更新与版本切换

- 已安装 QVMConsole 后，“更新到最新版”始终可用，并按当前渠道更新。
- 更新、首次安装和版本切换都会忽略本地下载缓存，重新获取发布端元数据、脚本和发行包。
- 选择另一个渠道后，可使用“切换到…”在开源版和赞助版之间原地切换。
- 1.0.17 修复了应用中心更新时误要求输入卸载确认词的问题。更新不会触发 QVMConsole 联动卸载。

操作前会备份以下内容：

- `/opt/kvm-console` 中的数据库、配置、二进制和前端；
- `/etc/systemd/system/kvm-console.service`。

脚本退出异常或健康检查失败时，管理器会停止目标服务、恢复备份、执行 `systemctl daemon-reload` 并重新启动原服务。最多保留最近三份系统级备份。

## 服务管理

“服务管理”页支持：

- 启动、停止和重启 `kvm-console.service`；
- 修改服务端口并同步 `.env` 和 UFW；
- 查看最近 500 行或下载最多 2000 行 systemd 日志；
- 在新窗口打开 QVMConsole。

QVMConsole 本体带有 `X-Frame-Options: DENY`，因此只内嵌管理器，QVMConsole 本体使用新窗口打开。

修改端口需要输入 `CHANGE PORT`。新端口健康检查失败时，管理器会恢复原 `.env`、UFW 规则和服务状态。

## 用户管理

用户数据库初始化后，“用户管理”页提供 `qvmc-manage.sh` 对应功能：

- 查看所有未删除用户；
- 清除指定用户的 TOTP；
- 重置默认管理员为 `admin / admin123`，同时清除该账号邮箱和 TOTP；
- 修改指定管理员密码，同时保留邮箱和 TOTP；
- 修改服务端口。

敏感操作均要求输入指定确认词。管理员新密码不得少于 12 位，并通过 HIBP k-Anonymity 泄露检测；密码只在请求内存中使用，不写入管理日志。修改前会使用 SQLite `VACUUM INTO` 创建数据库在线备份，最多保留十份。

## 修复与卸载

“修复配置”会调用上游修复模式重建 `.env`，执行前自动创建回滚备份。1.0.4 起还会安装飞牛 libvirt 9.0 的 UEFI NVRAM 格式兼容层，新建虚拟机启动前会自动修正不兼容的 NVRAM 文件。

1.0.17 起，libvirt 启动钩子还会处理飞牛存储卷的 ACL 差异。1.0.18 起，QVMConsole 服务使用的 `virsh start` 适配器也会在启动前同步执行同一套修复，以兼容飞牛 libvirt 未触发钩子的启动链路。1.0.20 起，管理器会为 `/volN` 补充 AppArmor 访问上限规则，并在兼容 helper 更新后重载 libvirt。1.0.21 起，修复配置会提前为 QVMConsole 存储池磁盘目录设置遍历 ACL 和默认 ACL，使新建 qcow2 在落盘时直接继承 QEMU 权限。1.0.22 起，规则还会写入飞牛实际包含的 `/etc/apparmor.d/local/abstractions/libvirt-qemu`，避免规则文件存在但 QEMU profile 未加载。1.0.23 起，如果磁盘目录属主就是 QEMU，管理器会保留原组并把属主规范为 root，再通过命名 ACL 授予 QEMU 目录访问权限，避免属主 `000` 使同 UID 命名 ACL 失效。1.0.24 曾为 QVMConsole 域禁用 libvirt DAC relabel；1.0.25 已通过 QEMU 主组、命名 ACL 和最小 mode 独立解决 `trima` 磁盘访问。1.0.26 会移除管理器写入的 DAC 禁用标签，恢复 libvirt 对 swtpm 等运行时资源的权限管理，同时保留磁盘兼容修复。1.0.27 会在 `qemu-img create` 成功后、libvirt RPC 热插之前立即修复新磁盘权限，并为 `virsh attach-device` 增加同类兜底。other 权限不会开放，也不会让 QEMU 以 root 身份运行。

1.0.5 起，管理器会检测飞牛 `network_service` 对外部 OVS 网桥的清理行为，并自动启用 libvirt NAT 兼容层：

- 将 QVMConsole 网络后端持久化为 `libvirt`，停止冲突的 OVS DHCP 服务；
- 优先复用系统现有的 libvirt NAT 网络，不存在时才创建独立网络；
- 每 15 秒核对 QVMConsole 的 VPC 绑定，为缺少实体网卡的虚拟机补充标准 VirtIO 桥接网卡；
- 将已有 OVS 持久网卡迁移到稳定的 Linux 网桥；
- 运行态热插失败时保留持久配置，并在管理器概览页提示需要重启的虚拟机，不主动中断虚拟机。

1.0.6 起还会为 QVMConsole 服务安装独立的网络命令适配层。QVMConsole 内部对 `br-ovs` 的存在性、流表和端口校验会映射到兼容网络，`virsh` 收到的 OVS 网卡 XML 会在提交给 libvirt 前转换为标准 Linux 桥网卡。该适配只作用于 `kvm-console.service`，不会替换系统命令，也不会影响飞牛自身网络服务。

1.0.7 起，桥接模式不再创建会被飞牛清理的自定义 OVS 网桥。管理器根据 QVMConsole 保存的“桥名 → 物理上联网卡”记录，将桥接网卡转换为 libvirt macvtap `direct/bridge` 接口；宿主机 IP 继续保留在飞牛管理的物理网卡上，来宾通过独立 MAC 接入同一物理网络。macvtap 的常规限制仍然存在：来宾可访问外部局域网，但默认不能直接访问同一宿主机的 IP。

1.0.9 起，混合使用 VPC 与桥接直通网卡时，QVMConsole 的运行态视图会保留桥接逻辑名称，同时将直通网卡标记为 `direct`。这可避免启动后应用第一个 VPC 绑定时误把同一虚拟机的其他桥接网卡识别为“正在切换回 VPC”。

1.0.14 起，兼容层还会扫描全部 libvirt 持久域定义。即使旧虚拟机缺少 `vpc_vm_bindings` 数据库记录，只要网卡仍引用已被飞牛清理的 `br-ovs`，也会转换为稳定的 libvirt 网桥。新建虚拟机在执行 `virsh start` 前同步转换，不再依赖 15 秒后台轮询；后台只处理磁盘存在且已稳定至少 60 秒的旧域，避免与创建失败后的资源清理竞争。转换会保留原有 MAC、网卡型号和 PCI 地址；运行中的虚拟机只更新持久配置，并在概览页提示重启。

1.0.15 起，桥接直通映射会检查物理上联网卡是否已是飞牛 OVS 的从端口。如果 `ovs-vsctl port-to-br` 返回系统 OVS 网桥，macvtap 会改用该 OVS 内部接口作为父接口，避免直接挂在物理从端口时出现 `Device or resource busy`。已有虚拟机的持久域 XML 会自动迁移到解析后的父接口。

卸载 QVMConsole 时：

- 默认只删除服务、二进制和前端，保留数据库与 `.env`；
- 勾选清理数据后会删除 `/opt/kvm-console`；
- 虚拟机磁盘、模板、libvirt 定义和用户存储镜像始终保留；
- 需要输入 `UNINSTALL` 完成二次确认。

默认卸载后再次安装时，管理器会在执行上游脚本前备份保留的数据库与 `.env`，安装完成后覆盖恢复并重启服务，避免上游首次安装流程重写已有配置。

### 卸载管理器时联动清理

从应用中心卸载管理器 FPK 时，卸载向导提供“卸载范围”单选项，默认选择“仅卸载管理器，保留 QVMConsole 和兼容修补”。选择该项时，卸载 FPK 不会停止或删除 QVMConsole。

卸载确认框必须输入 `UNINSTALL QVMCONSOLE`。选择“同时卸载 QVMConsole 和兼容修补”时，联动清理会在管理器 FPK 删除前执行；确认词不正确会直接取消整个卸载流程。该操作会：

- 停止并删除 QVMConsole 和 OVS DHCP 辅助服务；
- 删除 `/opt/kvm-console`，其中包括数据库与 `.env`；
- 删除管理器创建的网络命令兼容服务覆盖、NVRAM 钩子和 libguestfs 初始化文件；
- 删除管理器创建的 `qvmconsole-fnos` libvirt 网络。

虚拟机磁盘、模板和 libvirt 域定义不会删除。若有虚拟机正在使用 `qvmconsole-fnos` 网络，请先关闭它们，以免网络在清理期间中断。

## 下载校验

管理器在每次获取发布文件前，都会使用 HTTPS 请求发布端的 `ETag`。只有 `ETag` 明确声明 SHA256 时才会继续下载，并在下载完成后再次计算 SHA256 比对。更新任务会附带禁用缓存请求头并删除本地同名缓存，确保执行的是该次请求获取的发布文件。若文件在获取元数据后又发生变化，任务会停止并要求重新执行更新。

默认发布地址可通过以下环境变量在启动管理器前配置：

- `QVMC_EXECUTOR_URL`：无人值守安装脚本地址；
- `QVMC_OPEN_PACKAGE_URL`：开源版发行包地址；
- `QVMC_SPONSOR_PACKAGE_URL`：赞助版发行包地址。

这三个地址必须是无账号信息的 HTTPS 地址。管理器不会跳过脚本结构校验或发行包 SHA256 校验。
