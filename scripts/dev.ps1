param(
  [string]$BackendConfig = $(if ($env:BACKEND_CONFIG) { $env:BACKEND_CONFIG } else { 'backend/config.yaml' }),
  [int]$BackendPort = $(if ($env:BACKEND_PORT) { [int]$env:BACKEND_PORT } else { 17878 }),
  [string]$BackendOrigin = $(if ($env:BACKEND_ORIGIN) { $env:BACKEND_ORIGIN } else { '' }),
  [string]$FrontendHost = $(if ($env:FRONTEND_HOST) { $env:FRONTEND_HOST } else { '0.0.0.0' }),
  [int]$FrontendPort = $(if ($env:FRONTEND_PORT) { [int]$env:FRONTEND_PORT } else { 5173 }),
  [string]$FrontendPublicShareOrigin = $(if ($env:FRONTEND_PUBLIC_SHARE_ORIGIN) { $env:FRONTEND_PUBLIC_SHARE_ORIGIN } elseif ($env:VITE_PUBLIC_SHARE_ORIGIN) { $env:VITE_PUBLIC_SHARE_ORIGIN } else { '' }),
  [string]$FrontendTransferOrigin = $(if ($env:FRONTEND_TRANSFER_ORIGIN) { $env:FRONTEND_TRANSFER_ORIGIN } elseif ($env:VITE_TRANSFER_ORIGIN) { $env:VITE_TRANSFER_ORIGIN } else { '' })
)

$ErrorActionPreference = 'Stop'

$RootDir = (Resolve-Path (Join-Path $PSScriptRoot '..')).Path
if ([string]::IsNullOrWhiteSpace($BackendOrigin)) {
  # 使用 127.0.0.1 避免 Windows/Node 将 localhost 优先解析为 ::1，导致 Vite 代理连不上只监听 IPv4 的后端。
  $BackendOrigin = "http://127.0.0.1:$BackendPort"
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

function Wait-BackendReady {
  param([string]$Origin, [object]$Process, [string]$StdoutLog, [string]$StderrLog)
  $HealthUrl = "$Origin/api/health"
  Write-Host "等待后端就绪：$HealthUrl"
  for ($i = 0; $i -lt 60; $i++) {
    if ($Process -and $Process.HasExited) {
      Show-BackendFailure -Message '后端启动失败。' -Process $Process -StdoutLog $StdoutLog -StderrLog $StderrLog
      exit $Process.ExitCode
    }
    try {
      $Response = Invoke-WebRequest -Uri $HealthUrl -UseBasicParsing -TimeoutSec 1
      if ($Response.StatusCode -eq 200) {
        return
      }
    } catch {
      Start-Sleep -Milliseconds 500
    }
  }
  Show-BackendFailure -Message '后端未在预期时间内就绪。' -Process $Process -StdoutLog $StdoutLog -StderrLog $StderrLog
  exit 1
}

function Show-LogTail {
  param([string]$Path, [string]$Title)
  if ($Path -and (Test-Path -LiteralPath $Path)) {
    $Content = Get-Content -LiteralPath $Path -Tail 80 -ErrorAction SilentlyContinue
    if ($Content) {
      Write-Host ""
      Write-Host $Title
      $Content | ForEach-Object { Write-Host "  $_" }
    }
  }
}

function Show-BackendFailure {
  param([string]$Message, [object]$Process, [string]$StdoutLog, [string]$StderrLog)
  Write-Host ""
  Write-Host $Message -ForegroundColor Red
  if ($Process -and $Process.HasExited) {
    Write-Host "退出码：$($Process.ExitCode)"
  }
  Show-LogTail -Path $StderrLog -Title '后端错误日志：'
  Show-LogTail -Path $StdoutLog -Title '后端输出日志：'
  Write-Host ""
  Write-Host '常见处理方式：'
  Write-Host "  1. 如果提示 YAML 格式错误，请检查 backend/config.yaml 对应行附近的缩进。"
  Write-Host "  2. file_picker 应与 storage 同级；不要把 roots/max_page_size/deny_names 缩进到 storage.shares 下面。"
  Write-Host "  3. 如果提示端口监听失败，请确认 $BackendOrigin 没有被其他进程占用，或用 -BackendPort 修改端口。"
  Write-Host "  4. 如果提示数据库无法打开，请确认 backend/data 目录可写。"
  Write-Host "日志文件："
  Write-Host "  stdout: $StdoutLog"
  Write-Host "  stderr: $StderrLog"
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
$BackendStdoutLog = Join-Path ([System.IO.Path]::GetTempPath()) ("file-trans-backend-stdout-{0}.log" -f ([guid]::NewGuid().ToString('N')))
$BackendStderrLog = Join-Path ([System.IO.Path]::GetTempPath()) ("file-trans-backend-stderr-{0}.log" -f ([guid]::NewGuid().ToString('N')))
$OldBackendOrigin = $env:BACKEND_ORIGIN
$OldViteBackendOrigin = $env:VITE_BACKEND_ORIGIN
$OldPublicShareOrigin = $env:VITE_PUBLIC_SHARE_ORIGIN
$OldTransferOrigin = $env:VITE_TRANSFER_ORIGIN
$OldFrontendPort = $env:VITE_FRONTEND_PORT
$OldTransferPort = $env:VITE_TRANSFER_PORT

try {
  Write-Host "启动后端：$BackendConfig"
  Write-Host "前端代理目标：$BackendOrigin"
  $BackendProcess = Start-Process -FilePath 'go' `
    -ArgumentList @('run', './cmd/server', '-config', $BackendConfigPath) `
    -WorkingDirectory (Join-Path $RootDir 'backend') `
    -RedirectStandardOutput $BackendStdoutLog `
    -RedirectStandardError $BackendStderrLog `
    -NoNewWindow `
    -PassThru

  Wait-BackendReady -Origin $BackendOrigin -Process $BackendProcess -StdoutLog $BackendStdoutLog -StderrLog $BackendStderrLog

  $env:BACKEND_ORIGIN = $BackendOrigin
  $env:VITE_BACKEND_ORIGIN = $BackendOrigin
  $env:VITE_PUBLIC_SHARE_ORIGIN = $FrontendPublicShareOrigin
  $env:VITE_TRANSFER_ORIGIN = $FrontendTransferOrigin
  $env:VITE_FRONTEND_PORT = [string]$FrontendPort
  $env:VITE_TRANSFER_PORT = [string]$BackendPort

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
    if ($BackendProcess.ExitCode -ne 0) {
      Show-BackendFailure -Message '后端进程已退出。' -Process $BackendProcess -StdoutLog $BackendStdoutLog -StderrLog $BackendStderrLog
    }
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
  $env:VITE_PUBLIC_SHARE_ORIGIN = $OldPublicShareOrigin
  $env:VITE_TRANSFER_ORIGIN = $OldTransferOrigin
  $env:VITE_FRONTEND_PORT = $OldFrontendPort
  $env:VITE_TRANSFER_PORT = $OldTransferPort
}
