<#
.SYNOPSIS
  End to end on a Windows machine: install muster from locally assembled npm
  packages, start it, enrol a client, and drive a session through the
  subprocess driver (#307, part of #303).

.DESCRIPTION
  Run by CI's `windows` job, after scripts/gate-windows.sh. It is the only place
  the PIECES are exercised together: the release build for windows, the npm
  packages assembled from it (--all, because windows is built but not
  published), the launcher, enrolment, `doctor`, the service started through
  its scheduled task, the HTTP API, and the process tree the subprocess driver
  leaves behind.

  The "opencode" it drives is a stand-in: the opencode package's own test binary
  (`go test -c`), which answers as the runtime when invoked as one. No opencode
  install is needed or used.

  It prints one PASS line per assertion, and on the first failure dumps the
  service log and exits non-zero. It needs PowerShell 7 (pwsh), which the
  windows-latest runner has.

  Two things it does that need a word:

    * The service is started through the scheduled task when the machine lets
      it, and as a direct process when it does not; the output says which
      ("START PATH: ..."). A runner without an interactive desktop may refuse a
      task registered for an interactive token, and that must not read as a
      muster failure. Everything after the start is identical either way.
    * It changes the USER environment (a task cannot carry its own environment,
      so FLEET_* have to be there) and registers a task. Both are removed at the
      end, whatever happened. Because that would be destructive on a person's
      machine, it refuses to run outside CI unless -Force is given.
#>
[CmdletBinding()]
param(
  # A prerelease-shaped synthetic tag, so nothing here can be mistaken for a
  # published version and `muster --version` has something exact to compare.
  [string]$Tag = 'v0.0.1-e2e.1',
  [switch]$Force
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$PSNativeCommandUseErrorActionPreference = $false

if (-not $env:CI -and -not $Force) {
  throw 'e2e-windows.ps1 changes the user environment and registers a scheduled task; it runs in CI. Pass -Force to run it on this machine.'
}

$repo = Split-Path -Parent $PSScriptRoot
$work = Join-Path ([IO.Path]::GetTempPath()) ('muster-e2e-' + [guid]::NewGuid().ToString('n').Substring(0, 8))
$null = New-Item -ItemType Directory -Force $work
$taskName = 'muster-e2e'
$machine = 'e2ewin'
$script:passed = 0
$script:logFile = $null
$script:userVars = @()
$script:token = $null
$script:base = $null

function Pass([string]$what) { $script:passed++; Write-Host "PASS  $what" }
function Check([bool]$cond, [string]$what) {
  if (-not $cond) { throw "FAILED: $what" }
  Pass $what
}
function Say([string]$what) { Write-Host "== $what" }

# Run a native command; return its combined output, throw on a non-zero exit.
function Run([string]$exe, [string[]]$arguments, [string]$cwd = $repo) {
  Push-Location $cwd
  try {
    $out = (& $exe @arguments 2>&1 | Out-String)
    if ($LASTEXITCODE -ne 0) { throw "$exe $($arguments -join ' ') exited $LASTEXITCODE`n$out" }
    return $out
  } finally { Pop-Location }
}

function BashPath([string]$p) { return ($p -replace '\\', '/') }

function FindBash {
  # Git for Windows' bash, never the WSL launcher in System32, which would run
  # the script against a different filesystem.
  foreach ($root in @($env:ProgramFiles, ${env:ProgramFiles(x86)})) {
    if (-not $root) { continue }
    $c = Join-Path $root 'Git\bin\bash.exe'
    if (Test-Path $c) { return $c }
  }
  throw 'Git for Windows bash was not found'
}

# Both scopes: the process (for the commands this script runs) and the user
# environment (what a task started as this user is given).
function Set-UserVar([string]$name, [string]$value) {
  [Environment]::SetEnvironmentVariable($name, $value, 'Process')
  [Environment]::SetEnvironmentVariable($name, $value, 'User')
  $script:userVars += $name
}

function Api([string]$method, [string]$path, $body = $null, [string]$key = $null) {
  $headers = @{ Authorization = "Bearer $script:token" }
  if ($key) { $headers['Idempotency-Key'] = $key }
  $params = @{
    Uri = "$script:base$path"; Method = $method; Headers = $headers
    SkipHttpErrorCheck = $true; TimeoutSec = 30
  }
  if ($null -ne $body) {
    $params['Body'] = ($body | ConvertTo-Json -Compress -Depth 8)
    $params['ContentType'] = 'application/json'
  }
  $r = Invoke-WebRequest @params
  $json = $null
  if ($r.Content) { try { $json = $r.Content | ConvertFrom-Json } catch { } }
  return [pscustomobject]@{ Status = [int]$r.StatusCode; Json = $json; Raw = [string]$r.Content }
}

function FakeProcesses { return @(Get-Process -Name 'fake-opencode' -ErrorAction SilentlyContinue) }
# A count, not an array: a function's output is unrolled, so a single process
# would come back as the process itself, which has no Count under strict mode.
function FakeCount { return @(Get-Process -Name 'fake-opencode' -ErrorAction SilentlyContinue).Count }

function WaitFor([scriptblock]$cond, [int]$seconds) {
  $deadline = (Get-Date).AddSeconds($seconds)
  while ((Get-Date) -lt $deadline) {
    if (& $cond) { return $true }
    Start-Sleep -Milliseconds 300
  }
  return $false
}

function ServiceUp {
  try {
    $r = Api 'GET' '/v1/health'
    return ($r.Status -eq 200)
  } catch { return $false }
}

function ServiceProcesses([string]$exe) {
  return @(Get-Process -Name 'muster' -ErrorAction SilentlyContinue | Where-Object {
      try { $_.Path -ieq $exe } catch { $false }
    })
}

function Cleanup([string]$exe) {
  try { Stop-ScheduledTask -TaskName $taskName -ErrorAction SilentlyContinue } catch { }
  try { Unregister-ScheduledTask -TaskName $taskName -Confirm:$false -ErrorAction SilentlyContinue } catch { }
  if ($exe) { ServiceProcesses $exe | Stop-Process -Force -ErrorAction SilentlyContinue }
  FakeProcesses | Stop-Process -Force -ErrorAction SilentlyContinue
  foreach ($n in $script:userVars) { [Environment]::SetEnvironmentVariable($n, $null, 'User') }
}

$muster = $null
try {
  # ---- 1. build what a release builds, and assemble it --------------------
  $dist = Join-Path $work 'dist'
  $pkgs = Join-Path $work 'pkgs'
  $tgz = Join-Path $work 'tgz'
  $prefix = Join-Path $work 'npm-prefix'
  $null = New-Item -ItemType Directory -Force $tgz, $prefix

  Say "scripts/build-release.sh $Tag"
  Run (FindBash) @('scripts/build-release.sh', (BashPath $dist), $Tag) | Write-Host
  Check (Test-Path (Join-Path $dist "muster-$Tag-windows-amd64.exe")) 'the release build produced the windows-amd64 binary'
  Check (Test-Path (Join-Path $dist "muster-$Tag-windows-arm64.exe")) 'the release build produced the windows-arm64 binary'

  Say 'scripts/assemble-npm.mjs --all'
  Run 'node' @('scripts/assemble-npm.mjs', '--all', $dist, $Tag, $pkgs) | Write-Host
  Check (Test-Path (Join-Path $pkgs 'muster-win32-x64\bin\muster.exe')) 'the win32-x64 package carries bin\muster.exe'

  # ---- 2. install from the packed tarballs --------------------------------
  Say 'npm pack + npm install -g'
  Run 'npm' @('pack', (Join-Path $pkgs 'muster-win32-x64'), (Join-Path $pkgs 'muster'), '--pack-destination', $tgz) | Write-Host
  # Named from the version, not globbed: a Windows filename filter has no
  # character classes, and the launcher's name is a prefix of the platform's.
  $ver = $Tag.TrimStart('v')
  $platformTgz = Join-Path $tgz "futurelastic-muster-win32-x64-$ver.tgz"
  $launcherTgz = Join-Path $tgz "futurelastic-muster-$ver.tgz"
  Check ((Test-Path $platformTgz) -and (Test-Path $launcherTgz)) 'both tarballs were packed'
  # The launcher lists every platform as an optional dependency at this exact
  # version, and none of them is on the registry (it is a synthetic tag): npm
  # skips an optional dependency it cannot find, but the first attempt fails
  # loudly on some versions, so the second form is the fallback.
  try {
    Run 'npm' @('install', '-g', '--prefix', $prefix, '--ignore-scripts', '--no-audit', '--no-fund', $platformTgz, $launcherTgz) | Write-Host
  } catch {
    Write-Host "npm install failed once; retrying without optional dependencies: $($_.Exception.Message)"
    Run 'npm' @('install', '-g', '--prefix', $prefix, '--ignore-scripts', '--no-audit', '--no-fund', '--omit=optional', $platformTgz, $launcherTgz) | Write-Host
  }
  $muster = Join-Path $prefix 'node_modules\@futurelastic\muster-win32-x64\bin\muster.exe'
  Check (Test-Path $muster) 'the platform package installed under the global prefix'

  # ---- 3. --version: the launcher, then the binary itself -----------------
  $shim = Join-Path $prefix 'muster.cmd'
  Check (Test-Path $shim) 'npm created the muster command'
  $viaLauncher = (Run $shim @('--version')).Trim()
  Check ($viaLauncher -match ('^muster ' + [regex]::Escape($Tag) + ' ')) "muster --version through the launcher names the tag ($viaLauncher)"
  $direct = (Run $muster @('--version')).Trim()
  Check ($direct -match ('^muster ' + [regex]::Escape($Tag) + ' ')) 'muster.exe --version names the tag'

  # ---- 4. the stand-in runtime --------------------------------------------
  Say 'go test -c ./internal/drivers/opencode (the stand-in opencode)'
  $fake = Join-Path $work 'fake-opencode.exe'
  Run 'go' @('test', '-c', '-o', $fake, './internal/drivers/opencode') | Write-Host
  Check (Test-Path $fake) 'the stand-in runtime was built'

  # ---- 5. enrol a client principal ----------------------------------------
  # Paths are the layout docs/install.md describes, under a scratch root.
  $appdata = Join-Path $work 'AppData\Roaming\muster'
  $local = Join-Path $work 'AppData\Local\muster'
  $null = New-Item -ItemType Directory -Force $appdata, $local
  $config = Join-Path $appdata 'config.json'
  $stateDir = Join-Path $local 'state'
  $script:logFile = Join-Path $local 'muster.log'
  # UTF-8 WITHOUT a byte-order mark: Windows PowerShell's `Set-Content -Encoding
  # UTF8` writes one, and the JSON reader does not accept it.
  [IO.File]::WriteAllText($config, '{}', [Text.UTF8Encoding]::new($false))
  $grants = 'read,create,send,interrupt,close,rename,discard,keys'
  Say 'muster principal add'
  Run $muster @('principal', 'add', 'e2e', "--grants=$grants", "--config=$config") | Write-Host
  $tokenFile = Join-Path $appdata 'e2e.token'
  Check (Test-Path $tokenFile) 'a token file was written'
  $script:token = (Get-Content -Raw $tokenFile).Trim()
  Check ($script:token.Length -ge 32) 'the token is not empty'
  Check (-not ([IO.File]::ReadAllBytes($tokenFile)[0] -eq 0xEF)) 'the token file has no byte-order mark'

  # ---- 6. the service environment -----------------------------------------
  foreach ($e in @(Get-ChildItem Env: | Where-Object { $_.Name -like 'FLEET_*' })) {
    [Environment]::SetEnvironmentVariable($e.Name, $null, 'Process')
  }
  $l = [Net.Sockets.TcpListener]::new([Net.IPAddress]::Loopback, 0)
  $l.Start(); $port = $l.LocalEndpoint.Port; $l.Stop()
  $script:base = "http://127.0.0.1:$port"
  Set-UserVar 'FLEET_MACHINE' $machine
  Set-UserVar 'FLEET_CONFIG' $config
  Set-UserVar 'FLEET_STATE_DIR' $stateDir
  Set-UserVar 'FLEET_ADDR' "127.0.0.1:$port"
  Set-UserVar 'FLEET_RUNTIME_OPENCODE' '1'
  Set-UserVar 'FLEET_OPENCODE_BIN' $fake
  Set-UserVar 'FLEET_LOG_FILE' $script:logFile
  # The task is registered under a name of this script's own, not the default.
  Set-UserVar 'FLEET_TASK_NAME' $taskName
  # FLEET_RUNTIME is deliberately NOT set: on Windows the default is "none", and
  # that default is part of what is being proved.

  # ---- 7. the scheduled task, as docs/install.md registers it -------------
  Say 'Register-ScheduledTask'
  $taskRegistered = $false
  try {
    $who = "$env:USERDOMAIN\$env:USERNAME"
    $action = New-ScheduledTaskAction -Execute $muster -Argument 'serve'
    $trigger = New-ScheduledTaskTrigger -AtLogOn -User $who
    $settings = New-ScheduledTaskSettingsSet -ExecutionTimeLimit ([TimeSpan]::Zero) `
      -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -MultipleInstances IgnoreNew `
      -RestartCount 3 -RestartInterval (New-TimeSpan -Minutes 1)
    $principal = New-ScheduledTaskPrincipal -UserId $who -LogonType Interactive -RunLevel Limited
    $null = Register-ScheduledTask -TaskName $taskName -Action $action -Trigger $trigger -Settings $settings -Principal $principal -Force
    $taskRegistered = $true
    Pass 'the scheduled task registered'
  } catch {
    Write-Host "NOTE  the task could not be registered on this machine: $($_.Exception.Message)"
  }

  # ---- 8. doctor ------------------------------------------------------------
  Say 'muster doctor'
  Push-Location $work
  try {
    $docJson = (& $muster doctor --json 2>$null | Out-String)
    $docExit = $LASTEXITCODE
  } finally { Pop-Location }
  $doc = $docJson | ConvertFrom-Json
  foreach ($r in $doc.rows) { Write-Host ('  {0,-8} {1,-26} {2}' -f $r.status.ToUpper(), $r.id, $r.summary) }
  $failed = @($doc.rows | Where-Object { $_.status -eq 'fail' })
  if ($taskRegistered) {
    Check ($docExit -eq 0) 'doctor exits 0'
    $taskRow = @($doc.rows | Where-Object { $_.id -eq 'service.task' })
    Check ($taskRow.Count -eq 1 -and $taskRow[0].status -eq 'pass') 'doctor reads the scheduled task as correct'
  } else {
    # Without a task the one row that is allowed to fail is service.task.
    $others = @($failed | Where-Object { $_.id -ne 'service.task' })
    Check ($others.Count -eq 0) 'doctor reports no failure other than the missing task'
  }
  $rtRow = @($doc.rows | Where-Object { $_.id -eq 'runtime' })
  Check ($rtRow.Count -eq 1 -and $rtRow[0].status -eq 'pass' -and $rtRow[0].summary -match 'none') 'doctor reads the default runtime as none'

  # ---- 9. start the service -------------------------------------------------
  Say 'start the service'
  $started = $false
  if ($taskRegistered) {
    try {
      Start-ScheduledTask -TaskName $taskName
      $started = WaitFor { ServiceUp } 45
      if ($started) { Write-Host 'START PATH: scheduled task' }
      else {
        Write-Host 'NOTE  the task did not bring the service up within 45s; falling back to a direct process'
        Stop-ScheduledTask -TaskName $taskName -ErrorAction SilentlyContinue
        ServiceProcesses $muster | Stop-Process -Force -ErrorAction SilentlyContinue
      }
    } catch {
      Write-Host "NOTE  starting the task failed ($($_.Exception.Message)); falling back to a direct process"
    }
  }
  if (-not $started) {
    $null = Start-Process -FilePath $muster -ArgumentList 'serve' -WindowStyle Hidden -PassThru
    $started = WaitFor { ServiceUp } 45
    if ($started) { Write-Host 'START PATH: direct process (the scheduled task was not usable on this machine)' }
  }
  Check $started 'the service answers GET /v1/health'

  # ---- 10. what is running here ---------------------------------------------
  Say 'GET /v1/runtimes'
  $rt = Api 'GET' '/v1/runtimes'
  Check ($rt.Status -eq 200) 'GET /v1/runtimes answers 200'
  $items = @($rt.Json.items)
  $localRows = @($items | Where-Object { $_.machine -eq $machine })
  Check ($localRows.Count -eq 1) "exactly one local runtime row (got $($localRows.Count): $(($localRows | ForEach-Object { $_.runtime }) -join ', '))"
  Check ($localRows[0].runtime -eq 'opencode') 'the local runtime is opencode'
  Check ($localRows[0].status -eq 'available') 'opencode is available'
  $capNames = @($localRows[0].capabilities.PSObject.Properties.Name)
  Check ($capNames -notcontains 'sandbox') 'no sandbox capability is declared'
  Check (@($items | Where-Object { $_.runtime -match 'tmux|terminal|claude' }).Count -eq 0) 'no terminal-runtime row is listed'

  # ---- 11. create, read, close ----------------------------------------------
  Say 'create / get / delete a session'
  $cwd = Join-Path $work 'session-cwd'
  $null = New-Item -ItemType Directory -Force $cwd
  $create = Api 'POST' "/v1/machines/$machine/sessions" @{ runtime = 'opencode'; cwd = $cwd; name = 'e2e' } ([guid]::NewGuid().ToString())
  Check ($create.Status -eq 201) "create answers 201 (got $($create.Status): $($create.Raw))"
  $id = [string]$create.Json.id
  Check ($id.Length -gt 0) "the session has an id ($id)"
  Check ((FakeCount) -ge 1) 'the session has a runtime process (so "none left" below means something)'

  $get = Api 'GET' "/v1/machines/$machine/sessions/$id"
  Check ($get.Status -eq 200) "get answers 200 (got $($get.Status): $($get.Raw))"
  Check ([string]$get.Json.id -eq $id) 'get returns the same session id'

  $del = Api 'DELETE' "/v1/machines/$machine/sessions/$id"
  Check ($del.Status -ge 200 -and $del.Status -lt 300) "delete answers 2xx (got $($del.Status): $($del.Raw))"
  $gone = WaitFor { (FakeCount) -eq 0 } 20
  Check $gone 'no runtime process is left after the session is closed'

  # ---- 12. stop the service; nothing may outlive it --------------------------
  # A session is left OPEN across the stop on purpose: the job-object cleanup
  # is what is being proved, and a closed session leaves nothing to clean.
  $open = Api 'POST' "/v1/machines/$machine/sessions" @{ runtime = 'opencode'; cwd = $cwd; name = 'e2e-open' } ([guid]::NewGuid().ToString())
  Check ($open.Status -eq 201 -and (FakeCount) -ge 1) 'a second session is open when the service stops'
  Say 'stop the service'
  try { Stop-ScheduledTask -TaskName $taskName -ErrorAction SilentlyContinue } catch { }
  ServiceProcesses $muster | Stop-Process -Force
  $stopped = WaitFor { -not (ServiceUp) } 20
  Check $stopped 'the service has stopped answering'
  $clean = WaitFor { (FakeCount) -eq 0 } 20
  Check $clean 'no runtime process outlives the service'

  Write-Host ''
  Write-Host "e2e-windows: $script:passed checks passed"
} catch {
  Write-Host ''
  Write-Host "e2e-windows FAILED after $script:passed passing checks: $($_.Exception.Message)"
  if ($script:logFile -and (Test-Path $script:logFile)) {
    Write-Host '---- service log ----'
    Get-Content $script:logFile | Write-Host
    Write-Host '---- end of service log ----'
  }
  Cleanup $muster
  exit 1
}
Cleanup $muster
exit 0
