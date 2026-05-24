param(
  [string]$BackendConfig = $(if ($env:BACKEND_CONFIG) { $env:BACKEND_CONFIG } else { 'backend/config.yaml' }),
  [int]$BackendPort = $(if ($env:BACKEND_PORT) { [int]$env:BACKEND_PORT } else { 17878 }),
  [string]$BackendOrigin = $(if ($env:BACKEND_ORIGIN) { $env:BACKEND_ORIGIN } else { '' }),
  [string]$FrontendHost = $(if ($env:FRONTEND_HOST) { $env:FRONTEND_HOST } else { '0.0.0.0' }),
  [int]$FrontendPort = $(if ($env:FRONTEND_PORT) { [int]$env:FRONTEND_PORT } else { 5173 })
)

$ErrorActionPreference = 'Stop'

$RootDir = (Resolve-Path (Join-Path $PSScriptRoot '..')).Path
if ([string]::IsNullOrWhiteSpace($BackendOrigin)) {
  $BackendOrigin = "http://localhost:$BackendPort"
}

if ([System.IO.Path]::IsPathRooted($BackendConfig)) {
  $BackendConfigPath = $BackendConfig
} else {
  $BackendConfigPath = Join-Path $RootDir $BackendConfig
}

function Require-Command {
  param([string]$Name, [string]$InstallHint)
  if (-not (Get-Command $Name -ErrorAction SilentlyContinue)) {
    Write-Error "未找到 $Name，请先安装 $InstallHint。"
  }
}

function Join-CmdArguments {
  param([string[]]$Arguments)
  # Start-Process 直接启动 bun 时可能拿到 .cmd 或脚本包装器，cmd.exe /c 需要自行保护参数空格。
  return ($Arguments | ForEach-Object {
    if ($_ -match '[\s"&|<>^]') {
      '"' + ($_ -replace '"', '\"') + '"'
    } else {
      $_
    }
  }) -join ' '
}

Require-Command -Name 'go' -InstallHint 'Go'
Require-Command -Name 'bun' -InstallHint 'Bun'

if (-not (Test-Path -LiteralPath $BackendConfigPath -PathType Leaf)) {
  $TargetDir = Split-Path -Parent $BackendConfigPath
  if ($TargetDir -and -not (Test-Path -LiteralPath $TargetDir)) {
    New-Item -ItemType Directory -Path $TargetDir | Out-Null
  }
  Copy-Item -LiteralPath (Join-Path $RootDir 'backend/config.example.yaml') -Destination $BackendConfigPath
  Write-Host "已复制 backend/config.example.yaml 到 $BackendConfig。"
  Write-Host '请先编辑配置文件，至少替换：'
  Write-Host '  - auth.totp_secret'
  Write-Host '  - auth.admin.username'
  Write-Host '  - auth.admin.password_sha256'
  Write-Host '  - storage.dirs'
  Write-Host ''
  Write-Host '配置完成后再次运行：pwsh -File scripts/dev.ps1'
  exit 1
}

$FrontendDir = Join-Path $RootDir 'frontend'
if (-not (Test-Path -LiteralPath (Join-Path $FrontendDir 'node_modules') -PathType Container)) {
  Write-Host '首次运行：安装前端依赖...'
  Push-Location $FrontendDir
  try {
    bun install
  } finally {
    Pop-Location
  }
}

$BackendProcess = $null
$FrontendProcess = $null
$OldBackendOrigin = $env:BACKEND_ORIGIN
$OldViteBackendOrigin = $env:VITE_BACKEND_ORIGIN

try {
  Write-Host "启动后端：$BackendConfig"
  Write-Host "前端代理目标：$BackendOrigin"
  $BackendProcess = Start-Process -FilePath 'go' `
    -ArgumentList @('run', './cmd/server', '-config', $BackendConfigPath) `
    -WorkingDirectory (Join-Path $RootDir 'backend') `
    -NoNewWindow `
    -PassThru

  $env:BACKEND_ORIGIN = $BackendOrigin
  $env:VITE_BACKEND_ORIGIN = $BackendOrigin

  Write-Host "启动前端：http://localhost:$FrontendPort"
  $FrontendCommand = 'bun ' + (Join-CmdArguments @('run', 'dev', '--', '--host', $FrontendHost, '--port', [string]$FrontendPort))
  # Windows 上 bun 常以 bun.cmd/shim 形式出现在 PATH 中，Start-Process 不能总是把它当原生 Win32 程序启动。
  # 通过 cmd.exe 启动可复用命令解析规则，避免 “%1 不是有效的 Win32” 错误。
  $FrontendProcess = Start-Process -FilePath 'cmd.exe' `
    -ArgumentList @('/d', '/s', '/c', $FrontendCommand) `
    -WorkingDirectory $FrontendDir `
    -NoNewWindow `
    -PassThru

  while ($true) {
    Start-Sleep -Seconds 1
    if ($BackendProcess.HasExited -or $FrontendProcess.HasExited) {
      break
    }
  }

  if ($BackendProcess.HasExited) {
    exit $BackendProcess.ExitCode
  }
  if ($FrontendProcess.HasExited) {
    exit $FrontendProcess.ExitCode
  }
} finally {
  if ($BackendProcess -and -not $BackendProcess.HasExited) {
    Stop-Process -Id $BackendProcess.Id -Force -ErrorAction SilentlyContinue
  }
  if ($FrontendProcess -and -not $FrontendProcess.HasExited) {
    Stop-Process -Id $FrontendProcess.Id -Force -ErrorAction SilentlyContinue
  }
  $env:BACKEND_ORIGIN = $OldBackendOrigin
  $env:VITE_BACKEND_ORIGIN = $OldViteBackendOrigin
}
