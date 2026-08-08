# 故障排查

## 管理器入口打不开

1. 在应用中心确认“QVMConsole 管理器”处于运行状态。
2. 查看 `${TRIM_PKGVAR}/manager.log`。
3. 确认 `${TRIM_APPDEST}/app.sock` 存在。
4. 确认桌面入口为 `/app/qvmconsole-manager`，并使用飞牛管理员账号访问。

## 应用中心更新提示输入卸载确认词

这是 1.0.16 及更早构建中的生命周期脚本问题：更新流程错误触发了仅供卸载使用的确认校验。该问题已在 1.0.17 修复，后续应用中心更新会跳过卸载确认和联动清理。

首次恢复时，请在应用中心使用“手动安装”选择 1.0.27 或更高版本的 FPK 覆盖安装；不要卸载现有管理器，也不要输入卸载确认词。该操作不会删除 `/opt/kvm-console`、虚拟机磁盘或现有 QVMConsole 数据。

## 下载或校验失败

- 检查设备 DNS、HTTPS 出站连接和系统时间。
- 使用“清理缓存”后重新执行。
- 若错误显示 SHA256 不匹配，说明上游文件内容已改变，需要更新管理器渠道目录后重新发布 FPK。

## 写操作提示请求来源校验失败

1. 确认管理器版本为 1.0.2 或更高版本。
2. 从飞牛桌面入口重新打开管理器，避免使用绕过统一网关的 Socket 或内部地址。
3. 刷新页面以重新获取 CSRF Token。

1.0.2 使用 CSRF Token 与管理器前端专用请求头共同证明请求来源，同时兼容 `Sec-Fetch-Site`、`Forwarded`、`X-Forwarded-Host` 和 `X-Original-Host`。入口 HTML 使用禁用缓存策略，更新后应关闭旧窗口再从飞牛桌面重新打开。

## 安装任务失败

任务面板会保存完整日志。优先检查：

- `/dev/kvm` 是否存在；
- 端口是否被占用；
- apt 软件源是否可用；
- libvirt 或 Open vSwitch 服务是否正常；
- `/opt/kvm-console` 所在分区剩余空间。

## 用户存储空间未显示或安装前失效

- 只有已挂载、可写的本地 `ext4`、`xfs` 或 `btrfs` 文件系统会显示；网络挂载和只读挂载不会进入列表。
- 先在飞牛存储管理中完成磁盘初始化和挂载，再点击用户存储空间右侧的刷新按钮。
- 如果任务提示“选择的用户存储空间已不可用”，说明选择后挂载状态发生变化。重新挂载该存储空间、刷新列表并再次安装。
- 根目录选项始终对应 `/var/lib/kvm-user-storage.img`，用于保持旧版默认行为。

## 安装后重启进入 emergency mode

1.0.29 及更早版本的上游安装脚本会把用户存储镜像作为强制 loop 挂载写入 `/etc/fstab`。镜像位于 `/volN` 时，systemd 可能在飞牛存储池就绪前尝试挂载；旧条目缺少 `nofail`，挂载失败会阻断系统启动。

升级到 1.0.30 会自动迁移活动条目以及为临时恢复而手动注释的旧条目。修复后的配置应包含：

```text
nofail,x-systemd.automount,x-systemd.mount-timeout=30s
```

外置存储卷还会包含对应的 `x-systemd.requires-mounts-for=/volN`。可以使用以下命令检查，输出中不应再存在缺少 `nofail` 的活动条目：

```bash
grep '[[:space:]]/var/lib/kvm-user-storage[[:space:]]' /etc/fstab
```

如果设备已经进入紧急模式且 root 账号被锁定，可从 GRUB 临时追加 `init=/bin/bash`，将根文件系统重新挂载为可写后注释旧条目，再正常重启并覆盖安装 1.0.30。不要删除 `kvm-user-storage.img`，其中保存用户存储数据。

更新、切换和修复失败后，日志中应出现“正在恢复备份”及恢复结果。不要在回滚过程中手动覆盖 `/opt/kvm-console`。

## 创建 Linux 虚拟机提示 guestfs_launch failed

1. 在管理器中执行“修复配置”。
2. 1.0.3 起，修复任务会把宿主机实际的 `libstdc++.so.6` 加入 supermin hostfiles，清理 appliance 缓存并运行 `libguestfs-test-tool`。
3. 任务日志出现“libguestfs appliance 自检通过”后，重新创建虚拟机。

飞牛系统可能提供未登记在 Debian 包文件清单中的运行库。supermin 默认只复制包清单文件，这会导致 appliance 启动后找不到实际动态库；管理器使用独立的 `packages-qvmconsole` 和 `hostfiles-qvmconsole` 文件处理该差异。

## 新建 UEFI 虚拟机一直显示 Guest has not initialized the display

飞牛当前提供的 libvirt 9.0 不保留较新 XML 中的 `nvram format='qcow2'` 属性。QVMConsole 创建的 NVRAM 实际为 qcow2，但域定义会把它按 raw 固件使用，QEMU 因而在 PCI 和系统盘初始化前卡住。这不是 VNC 网络故障，典型现象包括：

- NVRAM 文件约为 `917504` 字节，`qemu-img info -U` 显示格式为 `qcow2`；
- `virsh dumpxml` 中的 `<nvram>` 没有 `format='qcow2'`；
- `virsh domblkstat <VM> vda` 的读取次数一直为 0。

1.0.4 起，管理器会安装 `/etc/libvirt/hooks/qemu.d/50-qvmconsole-nvram-compat` 和 `/etc/libvirt/hooks/qvmconsole-nvram-helper`。辅助程序放在 libvirtd 的 AppArmor 允许执行目录内。libvirt 在虚拟机启动前遇到“XML 按 raw 使用、文件实际为 qcow2”的组合时，会先保留原有 UEFI 变量内容并原子转换为 raw；XML 已正确声明 qcow2 或文件本来就是 raw 时不会处理。

新建虚拟机会在第一次启动前自动处理，不需要修改 QVMConsole 源码。已经用错误格式启动并卡住的旧虚拟机，其变量区可能已有异常内容，应先备份域 XML 和 NVRAM，再使用 libvirt 的 NVRAM 重置功能单独处理。兼容程序安装在系统 libvirt 钩子目录中，卸载管理器不会中断后续新建虚拟机的兼容能力。

## UEFI 启动提示 swtpm socket Permission denied

Windows UEFI 虚拟机可能带有 TPM 2.0 模拟器。若启动错误包含：

```text
Failed to connect to '/run/libvirt/qemu/swtpm/...-swtpm.sock': Permission denied
```

请升级到 1.0.26 并执行一次“修复配置”。1.0.24-1.0.25 曾向整个域写入 `model='dac' relabel='no'`，虽然可以阻止 libvirt 改动飞牛卷磁盘属主，但也会阻止 libvirt 把 `tss:tss 0600` 的 swtpm socket 调整给 QEMU。1.0.26 使用独立的磁盘用户组、mode 与 ACL 修复，不再需要关闭域级 DAC relabel；兼容层会在新建、重新定义或启动旧域时移除由管理器写入的禁用标签。该修复不会关闭 AppArmor，也不会放宽 `/run/libvirt` 的全局权限。

## 启动虚拟机提示 qcow2 Permission denied

飞牛存储卷可能把 `/volN` 及用户目录设置为 `000`，再通过 ACL 控制访问。QVMConsole 即使已经把 qcow2 文件的属主改为 `libvirt-qemu`，QEMU 仍可能因为父目录没有遍历权限而无法打开磁盘。典型错误包含：

```text
Could not open '/vol1/.../vm.qcow2': Permission denied
```

升级到 1.0.27 后执行一次“修复配置”。管理器会更新 libvirt 启动钩子、QVMConsole 专用的 `virsh` 适配器和 `qemu-img` 适配器，并从 QVMConsole 存储池配置、`KVM_CLONE_DIR` 与现有 `/volN/vm-disks` 目录发现磁盘位置。1.0.21 会提前为磁盘目录链补充遍历 ACL，并设置默认 ACL，使新建 qcow2 在创建时直接继承 QEMU 权限，不再依赖启动钩子的执行时序。1.0.22 会把 `/volN` 规则写入飞牛实际包含的 QEMU AppArmor local abstraction；此前版本虽然生成了 drop-in 文件，但飞牛的 profile 没有包含该目录，规则不会生效。1.0.23 还会处理磁盘目录属主就是 QEMU、但属主权限为 `000` 的飞牛 ACL 组合：保留目录组，把属主规范为 root，并让 QEMU 通过命名 ACL 访问，避免 POSIX ACL 因先命中属主条目而跳过同 UID 命名条目。1.0.25 会保留磁盘文件属主，把文件组同步为 QEMU 账号的主组，并在 ACL 设置完成后同步最小 mode：只读文件合并 `0440`，可写磁盘合并 `0660`。这是因为飞牛 `trima` 挂载上的 `setfacl` 能更新 ACL 扩展属性，但普通文件仍按文件组与 mode 校验；文件为 `root:root 000` 时，即使命名 ACL 显示有效，QEMU 仍会收到 `Permission denied`。1.0.26 在保留上述磁盘修复的同时恢复 libvirt DAC 运行时权限管理。1.0.27 会截获 QVMConsole 的 `qemu-img create`，在命令成功后立即为新磁盘同步 QEMU 主组、命名 ACL 和 `0660` 最小 mode，再由上游执行 libvirt RPC 热插；该流程不依赖虚拟机启动钩子。兼容层保留已有权限，不向 other 开放，也不会执行 `chmod 777`。

创建过程中已经显示“已清理资源”的虚拟机，其域定义和系统盘已被 QVMConsole 删除，需要重新创建。未被清理的关机虚拟机可直接重新开机。可在设备终端检查：

```bash
namei -l /vol1/path/to/vm.qcow2
getfacl -cp /vol1 /vol1/path/to /vol1/path/to/vm.qcow2
getfacl -cpn /vol1/vm-disks
grep -F 'qvmconsole-manager fnOS storage access' /etc/apparmor.d/local/usr.lib.libvirt.virt-aa-helper /etc/apparmor.d/local/abstractions/libvirt-qemu /etc/apparmor.d/abstractions/libvirt-qemu.d/qvmconsole-manager-storage
```

如果任务日志提示缺少 `setfacl`，请先安装 `acl` 软件包再执行“修复配置”。

### 运行中的虚拟机添加磁盘失败

若错误来自 `blockdev-add`，且路径指向刚创建的 `vm名称-vdb.qcow2`，说明上游通过 libvirt RPC 热插磁盘，没有触发虚拟机启动钩子。1.0.27 会让 QVMConsole 服务优先使用专用 `qemu-img` 适配器，在镜像创建成功后、RPC 热插前同步权限。升级后必须执行一次“修复配置”，使 systemd 重新加载适配器路径并重启 QVMConsole 服务；不需要关闭或重建原虚拟机。

## 新增网卡后虚拟机中只有 lo

飞牛的 `network_service` 会清理未纳入系统网络管理的外部 OVS 网桥。QVMConsole 的 VPC 接口可能只写入逻辑绑定，或者刚建立的 OVS 网桥和 `vnet` 端口随后被飞牛删除，来宾系统最终只能看到回环接口。

1.0.5 起，管理器启动、安装、更新和修复时会自动启用 libvirt NAT 网络兼容层，并持续核对 VPC 逻辑绑定与 libvirt 实体网卡。概览页显示“飞牛网络兼容：已启用”即表示兼容层生效。

如果添加网卡时出现“配置 VPC 交换机带宽失败: OVS 网桥 br-ovs 不存在”，应升级到 1.0.6。该版本会为 QVMConsole 服务单独映射 OVS 校验命令与 `virsh` 网卡 XML，避免上游在 libvirt 兼容模式下继续要求真实 `br-ovs`。系统级 `/usr/bin/ovs-vsctl`、`/usr/bin/virsh` 不会被覆盖。

如果启动虚拟机时报“Cannot get interface MTU on 'br-ovs': No such device”，应升级到 1.0.14，并在管理器中执行“修复配置”。该错误表示 libvirt 持久域 XML 仍引用已被飞牛网络服务清理的 `br-ovs`。旧虚拟机可能没有对应的 `vpc_vm_bindings` 记录，早期兼容层因而无法发现；新建虚拟机也可能在后台轮询修复前立即启动。1.0.14 会扫描稳定的旧域，并在每次 `virsh start` 前同步转换新域，把遗留 OVS 网卡改为当前 libvirt NAT 网桥，不需要重建虚拟机或修改 QVMConsole 源码。

如果桥接网卡启动时报“Unable to add bridge `<桥名>` port `vnetX`: Operation not supported”，应升级到 1.0.8。飞牛会清理 QVMConsole 创建的自定义 OVS 网桥；管理器会读取桥接记录中的物理上联网卡，并按 VPC 绑定顺序分别保留 NAT 网卡、转换桥接网卡为 libvirt macvtap 接口，不再要求逻辑桥名对应真实宿主机设备。宿主机管理 IP 不会迁移，避免网络服务清理网桥时造成管理连接中断。

如果桥接网卡启动时报“error creating macvtap interface ... Device or resource busy”，应升级到 1.0.15。该错误通常表示桥接记录中的物理网卡已经加入飞牛管理的 OVS 网桥，内核不允许在 OVS 从端口上直接创建 macvtap。1.0.15 会通过 `ovs-vsctl port-to-br` 解析对应的飞牛 OVS 内部接口，并将现有和后续桥接直通网卡迁移到该接口；无需把物理网卡移出飞牛网络管理，也不会迁移宿主机管理 IP。

如果混合 VPC 与桥接直通网卡的虚拟机已经成功启动、IP 也正常，但页面仍提示“从桥接直通交换机切换回 VPC 需要先关闭虚拟机”，应升级到 1.0.9。该问题是 QVMConsole 在启动后应用 VPC 绑定时遍历了同一虚拟机的所有网卡；管理器会在专用兼容视图中保留桥接逻辑名称，并将其标记为 `direct`，避免其他桥接网卡触发 VPC 网卡的切换校验。

如果带 VLAN 的 VPC 网卡启动时报“启动成功，但应用 VPC 网络失败”，应升级到 1.0.31 并执行一次“修复配置”。飞牛兼容层会把数据库中的 VLAN 绑定写入 `/opt/kvm-console/.fnos-compat/vpc-vlans.tsv`，再由 QVMConsole 服务专用的 `virsh dumpxml` 还原给上游视图。真实 libvirt 网卡仍保持飞牛可稳定保留的 Linux 网桥或 macvtap 配置，不依赖真实 `br-ovs` 或 OVS 端口存在。

如果升级 1.0.31 后仍提示“启动成功，但应用 VPC 网络失败: 桥接直通交换机切换需要先关闭虚拟机”，应升级到 1.0.32 并再次执行“修复配置”。该问题是运行态 `virsh domiflist` 仍暴露了飞牛真实 macvtap/direct 父接口，QVMConsole 未看到桥接直通交换机的逻辑桥名；1.0.32 会生成 `/opt/kvm-console/.fnos-compat/vpc-runtime-bridges.tsv` 并在专用 `domiflist` 视图中还原。

如果概览页提示某台虚拟机需要重启，说明标准 VirtIO 网卡已经写入持久配置，但当前 QEMU 运行态没有可用 PCI 热插槽或热插被旧版 libvirt 拒绝。正常关闭并重新启动该虚拟机后即可加载网卡；管理器不会自动强制关机。

可在设备终端检查：

```bash
grep '^KVM_NETWORK_BACKEND=' /opt/kvm-console/.env
virsh net-list --all
virsh domiflist <虚拟机名称>
```

正常状态下网络后端为 `libvirt`，NAT 网络处于 `active` 且已启用 `autostart`，虚拟机接口来源为对应的 `virbr` 网桥。

## 用户管理提示数据库不存在

QVMConsole 至少成功启动一次后才会创建数据库。默认路径是：

```text
/opt/kvm-console/data/kvm_console.db
```

如果 `.env` 中设置了 `KVM_DB_PATH`，管理器会优先使用该路径。

## 修改端口后访问异常

管理器使用 `/api/public/version` 验证新端口。失败时会自动恢复原端口。可检查：

```bash
systemctl status kvm-console.service
journalctl -u kvm-console.service -n 200 --no-pager
grep '^KVM_PORT=' /opt/kvm-console/.env
```

## 卸载管理器后的 QVMConsole

这是预期行为。卸载向导默认选择保留 QVMConsole，因此卸载管理器后 `kvm-console.service` 会继续保持原状态。如需卸载目标程序，可先在管理页面执行“卸载 QVMConsole”，或在卸载向导中选择“同时卸载 QVMConsole 和兼容修补”。所有卸载都必须输入 `UNINSTALL QVMCONSOLE`。

如果选择联动卸载、输入确认词后仍未清理 QVMConsole，请不要使用 Windows `build.ps1` 产出的候选包进行验证。生命周期脚本需要 POSIX 可执行权限；将代码同步到飞牛或其他 Linux 环境后，执行：

```bash
bash scripts/build-fpk.sh
tar -tvf dist/qvmconsole-manager-1.0.27-x86_64.fpk | grep 'cmd/uninstall_'
```

输出中的 `cmd/uninstall_init` 和 `cmd/uninstall_callback` 必须以 `-rwxr-xr-x` 开头。然后安装该 Linux 构建的 FPK，再执行卸载测试。
