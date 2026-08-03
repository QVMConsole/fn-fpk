param(
    [string]$FnpackPath = "",
    [switch]$SkipFrontend
)

$ErrorActionPreference = "Stop"
$Root = Split-Path -Parent $PSScriptRoot
$Web = Join-Path $Root "web"
$Package = Join-Path $Root "package\qvmconsole-manager"
$ServerDir = Join-Path $Package "app\server"
$Dist = Join-Path $Root "dist"
$Tools = Join-Path $Root ".tools"
$manifestVersion = Get-Content (Join-Path $Package "manifest") | Where-Object { $_ -match '^version=' } | Select-Object -First 1
if (-not $manifestVersion) {
    throw "manifest 缺少版本号"
}
$Version = ($manifestVersion -split '=', 2)[1].Trim()
$OutputName = "qvmconsole-manager-${Version}-x86_64.fpk"

if (-not $SkipFrontend) {
    Push-Location $Web
    try {
        npm ci
        npm run build
    } finally {
        Pop-Location
    }
}

New-Item -ItemType Directory -Force -Path $ServerDir, $Dist | Out-Null
$env:GOOS = "linux"
$env:GOARCH = "amd64"
$env:CGO_ENABLED = "0"
go build -trimpath -ldflags "-s -w" -o (Join-Path $ServerDir "qvmconsole-manager") .\cmd\qvmconsole-manager

$forbidden = Get-ChildItem -Path $Package -Recurse -File | Where-Object {
    $_.Name -eq "install.sh" -or $_.Name -like "kvm-console*.tar.gz" -or $_.Name -eq "kvm-console"
}
if ($forbidden) {
    throw "FPK 目录包含被禁止的 QVMConsole 安装文件: $($forbidden.FullName -join ', ')"
}

if (-not $FnpackPath) {
    $command = Get-Command fnpack -ErrorAction SilentlyContinue
    if ($command) {
        $FnpackPath = $command.Source
    } else {
        New-Item -ItemType Directory -Force -Path $Tools | Out-Null
        $FnpackPath = Join-Path $Tools "fnpack.exe"
        if (-not (Test-Path -LiteralPath $FnpackPath)) {
            Invoke-WebRequest -UseBasicParsing -Uri "https://static2.fnnas.com/fnpack/fnpack-1.2.3-windows-amd64" -OutFile $FnpackPath
        }
    }
}

Push-Location $Root
try {
    & $FnpackPath build --directory $Package
    if ($LASTEXITCODE -ne 0) {
        throw "fnpack 构建失败，退出码: $LASTEXITCODE"
    }
} finally {
    Pop-Location
}

$candidate = Join-Path $Root "qvmconsole-manager.fpk"
if (-not (Test-Path -LiteralPath $candidate)) {
    throw "fnpack 未生成 FPK 文件"
}
$target = Join-Path $Dist $OutputName
Copy-Item -LiteralPath $candidate -Destination $target -Force
$hash = (Get-FileHash -LiteralPath $target -Algorithm SHA256).Hash.ToLowerInvariant()
Set-Content -LiteralPath "$target.sha256" -Value "$hash  $OutputName" -Encoding ascii
Write-Host "构建完成: $target"
Write-Host "SHA256: $hash"
