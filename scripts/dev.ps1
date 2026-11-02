#requires -Version 7.0
param(
  [string]$BackendConfig = $(if ($env:BACKEND_CONFIG) { $env:BACKEND_CONFIG } else { 'backend/config.yaml' }),
  [int]$BackendPort = $(if ($env:BACKEND_PORT) { [int]$env:BACKEND_PORT } else { 17878 }),
  [string]$BackendOrigin = $env:BACKEND_ORIGIN,
  [string]$FrontendHost = $(if ($env:FRONTEND_HOST) { $env:FRONTEND_HOST } else { '0.0.0.0' }),
  [int]$FrontendPort = $(if ($env:FRONTEND_PORT) { [int]$env:FRONTEND_PORT } else { 5173 }),
  [string]$FrontendPublicShareOrigin = $(if ($env:FRONTEND_PUBLIC_SHARE_ORIGIN) { $env:FRONTEND_PUBLIC_SHARE_ORIGIN } else { $env:VITE_PUBLIC_SHARE_ORIGIN }),
  [string]$FrontendTransferOrigin = $(if ($env:FRONTEND_TRANSFER_ORIGIN) { $env:FRONTEND_TRANSFER_ORIGIN } else { $env:VITE_TRANSFER_ORIGIN }),
  [int]$InstallTimeoutSeconds = $(if ($env:INSTALL_TIMEOUT_SECONDS) { [int]$env:INSTALL_TIMEOUT_SECONDS } else { 300 }),
  [int]$BuildTimeoutSeconds = $(if ($env:BUILD_TIMEOUT_SECONDS) { [int]$env:BUILD_TIMEOUT_SECONDS } else { 180 }),
  [int]$BackendReadyTimeoutSeconds = $(if ($env:BACKEND_READY_TIMEOUT_SECONDS) { [int]$env:BACKEND_READY_TIMEOUT_SECONDS } else { 30 }),
  [int]$FrontendReadyTimeoutSeconds = $(if ($env:FRONTEND_READY_TIMEOUT_SECONDS) { [int]$env:FRONTEND_READY_TIMEOUT_SECONDS } else { 30 })
)

$ErrorActionPreference = 'Stop'
$RootDir = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '..'))
if (-not $IsWindows) { throw 'dev.ps1 需要 Windows 和 PowerShell 7；其他平台请使用 dev.sh。' }
foreach ($Timeout in @($InstallTimeoutSeconds, $BuildTimeoutSeconds, $BackendReadyTimeoutSeconds, $FrontendReadyTimeoutSeconds)) {
  if ($Timeout -le 0) { throw '启动阶段超时必须为正整数（秒）。' }
}
if (-not $BackendOrigin) { $BackendOrigin = "http://127.0.0.1:$BackendPort" }
$BackendConfigPath = [IO.Path]::GetFullPath($BackendConfig, $RootDir)
$SettingsPath = Join-Path (Split-Path -Parent $BackendConfigPath) 'settings.yaml'
if (-not (Test-Path -LiteralPath $BackendConfigPath -PathType Leaf)) {
  New-Item -ItemType Directory -Force -Path (Split-Path -Parent $BackendConfigPath) | Out-Null
  Copy-Item -LiteralPath (Join-Path $RootDir 'backend/config.example.yaml') -Destination $BackendConfigPath
  if (-not (Test-Path -LiteralPath $SettingsPath)) {
    Copy-Item -LiteralPath (Join-Path $RootDir 'backend/settings.example.yaml') -Destination $SettingsPath
  }
  Write-Host "已生成管理员配置：$BackendConfigPath"
  Write-Host "服务设置（保留已有文件）：$SettingsPath"
  Write-Host '请设置 admin.username/password_hash，并检查 settings.yaml。生成密码块：'
  Write-Host ("  python `"{0}`" --format bootstrap" -f (Join-Path $PSScriptRoot 'hash-admin-password.py'))
  Write-Host '配置完成后，请使用本次相同的启动参数重新运行。'
  exit 1
}
$Go = (Get-Command go -CommandType Application -ErrorAction Stop | Select-Object -First 1).Source
$Bun = (Get-Command bun -CommandType Application -ErrorAction Stop | Select-Object -First 1).Source
$ArtifactRoot = if ($env:TMPDIR) { [IO.Path]::GetFullPath($env:TMPDIR, $RootDir) } else { Join-Path $RootDir '.opencode/dev' }
$RunDir = Join-Path $ArtifactRoot ('file-trans-dev-' + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Force -Path $RunDir | Out-Null
Write-Host "诊断及构建目录：$RunDir"
$Owners = [Collections.Generic.List[object]]::new()
$OldEnvironment = @{}
foreach ($Name in @('TMPDIR', 'TMP', 'TEMP', 'BACKEND_ORIGIN', 'VITE_BACKEND_ORIGIN', 'VITE_PUBLIC_SHARE_ORIGIN', 'VITE_TRANSFER_ORIGIN', 'VITE_FRONTEND_PORT', 'VITE_TRANSFER_PORT')) {
  $OldEnvironment[$Name] = [Environment]::GetEnvironmentVariable($Name, 'Process')
}

function Start-OwnedProcess {
  param([string]$Name, [string]$Command, [string[]]$Arguments, [string]$Directory)
  $Info = [Diagnostics.ProcessStartInfo]::new()
  $Info.WorkingDirectory = $Directory
  $Info.UseShellExecute = $false
  $Info.RedirectStandardOutput = $true
  $Info.RedirectStandardError = $true
  $Info.RedirectStandardInput = $true
  # This runner cannot launch a native tool until the parent assigns its private job.
  # It also delegates .cmd shim quoting to PowerShell instead of composing cmd /c text.
  $Info.FileName = Join-Path $PSHOME 'pwsh.exe'
  $Runner = @'
$ErrorActionPreference = 'Stop'
$Spec = [Console]::ReadLine() | ConvertFrom-Json
if ($Spec.backendPidPath) {
  # The native server shares the launcher's console and receives its Ctrl+C directly.
  $Info = [Diagnostics.ProcessStartInfo]::new()
  $Info.FileName = $Spec.command
  $Info.UseShellExecute = $false
  foreach ($Arg in $Spec.arguments) { $Info.ArgumentList.Add($Arg) }
  $Server = [Diagnostics.Process]::Start($Info)
  [IO.File]::WriteAllText($Spec.backendPidPath, [string]$Server.Id)
  $Server.WaitForExit()
  exit $Server.ExitCode
}
$NativeArgs = [string[]]$Spec.arguments
& $Spec.command @NativeArgs
exit $LASTEXITCODE
'@
  foreach ($Arg in @('-NoLogo', '-NoProfile', '-NonInteractive', '-Command', $Runner)) {
    $Info.ArgumentList.Add($Arg)
  }
  $Job = [FileTransitDevJob]::new()
  $Process = [Diagnostics.Process]::new()
  $Process.StartInfo = $Info
  $Owner = [pscustomobject]@{ Name = $Name; Process = $Process; Job = $Job; Out = $null; Err = $null; Stdout = $null; Stderr = $null; Assigned = $false; Saved = $false }
  $Owners.Add($Owner)
  [void]$Process.Start()
  $Owner.Stdout = [IO.File]::Create((Join-Path $RunDir "$Name.stdout.log"))
  $Owner.Stderr = [IO.File]::Create((Join-Path $RunDir "$Name.stderr.log"))
  $Owner.Out = $Process.StandardOutput.BaseStream.CopyToAsync($Owner.Stdout)
  $Owner.Err = $Process.StandardError.BaseStream.CopyToAsync($Owner.Stderr)
  $Job.Assign($Process.Handle)
  $Owner.Assigned = $true
  $Spec = @{ command = $Command; arguments = $Arguments }
  if ($Name -eq 'backend') {
    $Spec.backendPidPath = Join-Path $RunDir 'backend.pid'
  }
  $Process.StandardInput.WriteLine(($Spec | ConvertTo-Json -Compress))
  $Process.StandardInput.Close()
  return $Owner
}

function Save-ProcessLog {
  param($Owner)
  if (-not $Owner.Saved -and $Owner.Out) {
    [void]$Owner.Out.GetAwaiter().GetResult()
    [void]$Owner.Err.GetAwaiter().GetResult()
    $Owner.Stdout.Dispose()
    $Owner.Stderr.Dispose()
    $Owner.Saved = $true
  }
}

function Invoke-Phase {
  param([string]$Name, [int]$Timeout, [string]$Command, [string[]]$Arguments, [string]$Directory)
  Write-Host "[$Name] 开始（最长 ${Timeout}s）"
  $Owner = Start-OwnedProcess $Name $Command $Arguments $Directory
  if (-not $Owner.Process.WaitForExit($Timeout * 1000)) {
    Write-Host "[$Name] 超时；诊断目录：$RunDir"
    exit 124
  }
  $Code = $Owner.Process.ExitCode
  $Owner.Job.Dispose()
  Save-ProcessLog $Owner
  if ($Code -ne 0) {
    Write-Host "[$Name] 失败，退出码：$Code"
    Get-Content -LiteralPath (Join-Path $RunDir "$Name.stdout.log"), (Join-Path $RunDir "$Name.stderr.log") -Tail 80 | Write-Host
    exit $Code
  }
  Write-Host "[$Name] 完成"
}

function Test-Services {
  foreach ($Owner in @($script:BackendProcess, $script:FrontendProcess)) {
    if ($Owner -and $Owner.Process.HasExited) {
      $Code = $Owner.Process.ExitCode
      Write-Host "[运行] 服务意外退出（$($Owner.Name)），原退出码：$Code；诊断目录：$RunDir"
      if ($Code -eq 0) { $Code = 1 }
      exit $Code
    }
  }
}

function Wait-Ready {
  param([string]$Name, [string]$Url, [int]$Timeout)
  Write-Host "[$Name 就绪] 等待 $Url（最长 ${Timeout}s）"
  $Clock = [Diagnostics.Stopwatch]::StartNew()
  while ($Clock.Elapsed.TotalSeconds -lt $Timeout) {
    Test-Services
    # Only the readiness probe has a timeout, never a transfer or the service runtime.
    try { $Response = $Client.GetAsync($Url).GetAwaiter().GetResult() } catch [Net.Http.HttpRequestException] { $Response = $null } catch [OperationCanceledException] { $Response = $null }
    if ($Response) {
      $Ready = $Response.IsSuccessStatusCode
      $Response.Dispose()
      if ($Ready) { Test-Services; Write-Host "[$Name 就绪] 完成"; return }
    }
    Start-Sleep -Milliseconds 200
  }
  Write-Host "[$Name 就绪] 超时；诊断目录：$RunDir"
  exit 124
}

$BackendProcess = $null
$FrontendProcess = $null
$RuntimeReady = $false
$Handler = [Net.Http.HttpClientHandler]::new()
$Handler.UseProxy = $false
$Client = [Net.Http.HttpClient]::new($Handler)
$Client.Timeout = [TimeSpan]::FromSeconds(1)
try {
  $env:TMPDIR = $RunDir
  $env:TMP = $RunDir
  $env:TEMP = $RunDir
  if (-not ('FileTransitDevJob' -as [type])) { Add-Type -Path (Join-Path $PSScriptRoot 'dev-job.cs') }
  $FrontendDir = Join-Path $RootDir 'frontend'
  if (-not (Test-Path -LiteralPath (Join-Path $FrontendDir 'node_modules') -PathType Container)) {
    Invoke-Phase install $InstallTimeoutSeconds $Bun @('install') $FrontendDir
  } else { Write-Host '[install] 已有 node_modules，跳过安装' }
  $Binary = Join-Path $RunDir 'server.exe'
  Invoke-Phase build $BuildTimeoutSeconds $Go @('build', '-o', $Binary, './cmd/server') (Join-Path $RootDir 'backend')
  foreach ($Url in @("$BackendOrigin/api/health", "http://127.0.0.1:$FrontendPort/")) {
    try { $Response = $Client.GetAsync($Url).GetAwaiter().GetResult() } catch [Net.Http.HttpRequestException] { $Response = $null } catch [OperationCanceledException] { $Response = $null }
    if ($Response) { $Response.Dispose(); throw "[启动] 地址已被占用：$Url；请修改端口或自行停止旧服务。" }
  }
  Write-Host "[后端启动] 配置：$BackendConfigPath；代理目标：$BackendOrigin"
  $BackendProcess = Start-OwnedProcess backend $Binary @('-config', $BackendConfigPath, '-dev', '-dev-frontend-port', [string]$FrontendPort) (Join-Path $RootDir 'backend')
  Wait-Ready 后端 "$BackendOrigin/api/health" $BackendReadyTimeoutSeconds
  $env:BACKEND_ORIGIN = $BackendOrigin
  $env:VITE_BACKEND_ORIGIN = $BackendOrigin
  $env:VITE_PUBLIC_SHARE_ORIGIN = $FrontendPublicShareOrigin
  $env:VITE_TRANSFER_ORIGIN = $FrontendTransferOrigin
  $env:VITE_FRONTEND_PORT = [string]$FrontendPort
  $env:VITE_TRANSFER_PORT = [string]$BackendPort
  Write-Host "[前端启动] http://localhost:$FrontendPort"
  $FrontendProcess = Start-OwnedProcess frontend $Bun @('run', 'dev', '--', '--host', $FrontendHost, '--port', [string]$FrontendPort, '--strictPort') $FrontendDir
  Wait-Ready 前端 "http://127.0.0.1:$FrontendPort/" $FrontendReadyTimeoutSeconds
  $ServerPid = [int][IO.File]::ReadAllText((Join-Path $RunDir 'backend.pid'))
  $RuntimeReady = $true
  Write-Host '[运行] 前后端已就绪。按一次 Ctrl+C 开始停止，等待当前传输完成。'
  while ($true) { Test-Services; Start-Sleep -Milliseconds 200 }
} finally {
  if ($RuntimeReady) {
    # Wait on the native server, not its runner (a console event may stop the runner first).
    $NativeServer = Get-Process -Id $ServerPid -ErrorAction SilentlyContinue
    if ($NativeServer) {
      Write-Host '[停止] 等待本次后端退出，无固定截止时间；不会强杀后端。'
      Write-Host '本控制台 Ctrl+C 直接通知 Go 排空；若已按过则只需等待。若因前端退出进入此处，请按一次 Ctrl+C。'
      $NativeServer.WaitForExit()
      $NativeServer.Dispose()
    }
  }
  # Only close the backend job after drain, or for a startup failure before runtime.
  foreach ($Owner in $Owners) {
    $Owner.Job.Dispose()
    if ($Owner.Out -and -not $Owner.Assigned -and -not $Owner.Process.HasExited) { $Owner.Process.Kill($true) }
    if ($Owner.Out) { $Owner.Process.WaitForExit(); Save-ProcessLog $Owner }
    $Owner.Process.Dispose()
  }
  $Client.Dispose()
  foreach ($Name in $OldEnvironment.Keys) { [Environment]::SetEnvironmentVariable($Name, $OldEnvironment[$Name], 'Process') }
}
