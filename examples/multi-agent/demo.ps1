# Multi-agent demo (roadmap §23): one machine, three agent sessions.
# PowerShell port of demo.sh. Works without Unity installed (command tools
# stand in for Unity workloads).
$ErrorActionPreference = "Stop"

$Arh = if ($env:ARH) { $env:ARH } else { "arh" }
$State = New-Item -ItemType Directory -Path (Join-Path $env:TEMP ("arh-demo-" + [guid]::NewGuid().ToString("N").Substring(0,8)))
$env:ARH_STATE_DIR = $State.FullName
$Config = Join-Path $State "config.yaml"

$sh = "C:\Windows\System32\cmd.exe"
@"
runtime:
  state_dir: $($State.FullName -replace '\\','\\')
  max_sessions: 8
tools:
  gameplay-compiler:
    type: command
    executable: $sh
  shader-validator:
    type: command
    executable: $sh
  android-builder:
    type: command
    executable: $sh
resources:
  gpu:
    mode: exclusive
  unity-license:
    mode: exclusive
  build-slot:
    mode: capacity
    capacity: 2
environment:
  isolate_home: true
  isolate_tmp: true
"@ | Set-Content -Path $Config -Encoding UTF8
$env:ARH_CONFIG = $Config

Write-Host "== state dir: $($State.FullName)"
Write-Host "== starting daemon"
& $Arh daemon start -d | Out-Null

try {
  Write-Host ""
  Write-Host "== Agent A: gameplay code -> EditMode tests"
  $outA = & $Arh session create --name agent-a
  $SessA = ($outA | Select-String '^session (\S+) created').Matches[0].Groups[1].Value
  & $Arh lease acquire --session $SessA --resource unity-license | Out-Null
  & $Arh exec gameplay-compiler.run --session $SessA -- 'args=/c echo [agent-a] compiling & timeout /t 1 >nul & echo [agent-a] tests: 12 passed' | Out-Null

  Write-Host "== Agent B: shader -> GPU lease -> PlayMode validation"
  $outB = & $Arh session create --name agent-b
  $SessB = ($outB | Select-String '^session (\S+) created').Matches[0].Groups[1].Value
  & $Arh lease acquire --session $SessB --resource gpu --wait | Out-Null
  & $Arh exec shader-validator.run --session $SessB -- 'args=/c echo [agent-b] validating shaders on GPU & timeout /t 1 >nul' | Out-Null

  Write-Host "== Agent C: android plugin -> player build"
  $outC = & $Arh session create --name agent-c
  $SessC = ($outC | Select-String '^session (\S+) created').Matches[0].Groups[1].Value
  & $Arh exec android-builder.run --session $SessC -- 'args=/c echo [agent-c] building Android player & timeout /t 1 >nul & echo [agent-c] build ok' | Out-Null

  Write-Host ""
  Write-Host "== status"
  & $Arh status
  Write-Host ""
  Write-Host "== leases"
  & $Arh lease list
  Write-Host ""
  Write-Host "== runs"
  & $Arh run list

  Write-Host ""
  Write-Host "== teardown"
  foreach ($s in @($SessA, $SessB, $SessC)) {
    & $Arh session kill $s --reason "demo done" | Out-Null
  }
  & $Arh session list
  & $Arh lease list
  & $Arh port list
  Write-Host "3 agents | 1 machine | 1 shared toolchain | 0 runtime collisions"
} finally {
  & $Arh daemon stop 2>$null | Out-Null
}
