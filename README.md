# QVMConsole Manager for fnOS

独立的飞牛 fnOS QVMConsole 图形化管理器。FPK 不内置 QVMConsole 安装包，运行时由管理员选择开源版或赞助版并进行固定 SHA256 校验。

使用和维护说明参见 [`docs/usage.md`](docs/usage.md)，依赖说明参见 [`docs/dependencies.md`](docs/dependencies.md)。

## 构建

```powershell
powershell -ExecutionPolicy Bypass -File .\scripts\build.ps1
```

同步到飞牛或其他 Linux 环境后生成带标准 POSIX 权限的正式包：

```bash
bash scripts/build-fpk.sh
```

输出：

```text
dist/qvmconsole-manager-1.0.32-x86_64.fpk
dist/qvmconsole-manager-1.0.32-x86_64.fpk.sha256
```
