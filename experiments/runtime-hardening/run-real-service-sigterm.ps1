#!/usr/bin/env pwsh
# Re-runnable qualification of the REAL VibeShell service binary under the
# hardened runtime flags (documents/adr/0008 re-check).
#
# The earlier hardening spike (run-hardening-tests.ps1) tested SIGTERM through a
# shell trap because the runtime ENTRYPOINT was still the cmd/vibeshell stub.
# The composed service now exists, so this script builds the runtime image from
# the current containers/Containerfile and drives the real binary end to end:
#
#   1. start `vibeshell run` with a minimal valid config mounted read-only,
#      under the hardened flags, with a named volume for writable state;
#   2. wait for the "vibeshell ready" log line;
#   3. open a real SSH session against the published port;
#   4. send SIGTERM through `podman stop --time 5`;
#   5. assert the process exited cleanly (0, never 137) and the real service's
#      own shutdown ran (not the container runtime's SIGKILL escalation);
#   6. reopen the database and assert the open session was durably ended.
#
# Usage (from the repository root):
#   & .\experiments\runtime-hardening\run-real-service-sigterm.ps1
#
# Exit code 0 means every verdict-bearing check passed. The receipt is written
# to receipts/real-service-sigterm-receipt.txt and echoed to the console.

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$IMAGE = 'localhost/vibeshell-runtime:real'
$ServiceUID = '10001'
$PublishedPort = 2222
$StopTimeoutSeconds = 5
# The service config caps the shutdown wait so a handler that cannot finish is
# reported rather than blocking the stop; 4 s is inside the 5 s podman timeout.
$ShutdownGraceMs = 4000

$ReceiptPath = Join-Path $PSScriptRoot 'receipts/real-service-sigterm-receipt.txt'

$runId = Get-Random
$Name = "vibeshell-real-$runId"
$Volume = "vibeshell-real-state-$runId"
$StageDir = Join-Path ([System.IO.Path]::GetTempPath()) "vibeshell-real-sigterm-$runId"
New-Item -ItemType Directory -Force -Path $StageDir | Out-Null
$ConfigPath = Join-Path $StageDir 'vibeshell.json'

$receipt = [System.Collections.Generic.List[string]]::new()

function Write-Receipt {
    param([string] $Text = '', [ConsoleColor] $Color = [ConsoleColor]::Gray)
    Write-Host $Text -ForegroundColor $Color
    $receipt.Add($Text)
}

function Write-Heading { param([string] $Text); Write-Receipt ''; Write-Receipt "=== $Text ===" Cyan }

$script:PASS = 0
$script:FAIL = 0

function Add-Check {
    param(
        [Parameter(Mandatory)] [string] $Id,
        [Parameter(Mandatory)] [string] $Title,
        [Parameter(Mandatory)] [string] $Command,
        [Parameter(Mandatory)] [string] $Observed,
        [Parameter(Mandatory)] [string] $Expected,
        [Parameter(Mandatory)] [bool] $Ok
    )
    Write-Receipt "[$Id] $Title"
    Write-Receipt "     command : $Command"
    Write-Receipt "     observed: $Observed"
    Write-Receipt "     expected: $Expected"
    if ($Ok) { Write-Receipt '     verdict : PASS' Green; $script:PASS++ }
    else { Write-Receipt '     verdict : FAIL' Red; $script:FAIL++ }
}

function Write-Observation {
    param([string] $Title, [string] $Observed, [string] $Note = '')
    Write-Receipt "- $Title"
    Write-Receipt "     observed: $Observed"
    if ($Note -ne '') { Write-Receipt "     note    : $Note" }
}

# The minimal valid public-mode configuration: one provider/product/route/tier
# (tiers are required), no accounts and no prompts group, so no secret or prompt
# file is needed. State (host key + database) lives on the writable volume.
function New-MinimalConfig {
    param([string] $Path)
    $json = @'
{
  "version": 1,
  "identity": {"system_name": "VibeOS", "shell_name": "VibeShell", "hostname": "vibeshell.test"},
  "ssh": {"listen_address": "0.0.0.0", "listen_port": 2222, "host_key_file": "/var/lib/vibeshell/host_key", "handshake_timeout_ms": 10000},
  "auth": {"mode": "public"},
  "sharing": {"enabled": false},
  "providers": [{"name": "opencode", "products": [{"name": "console", "base_url": "https://opencode.example.invalid", "protocols": ["chat"], "default_protocol": "chat"}]}],
  "routes": [{"id": "rte_0123456789ABCDEFGHJKMNPQRS", "provider": "opencode", "product": "console", "model": "runtime-real", "protocol": "chat"}],
  "tiers": [{"name": "runtime-real", "routes": ["rte_0123456789ABCDEFGHJKMNPQRS"]}],
  "persistence": {"database_path": "/var/lib/vibeshell/world.db", "durability": "FULL"},
  "operations": {"log_level": "info", "shutdown_grace_ms": __GRACE__}
}
'@
    $json = $json.Replace('__GRACE__', [string]$ShutdownGraceMs)
    Set-Content -Path $Path -Value $json -NoNewline
}

# StartAsyncSession launches the Windows OpenSSH client as a background job and
# leaves its stdin open so the shell channel stays established (EOF would end
# the session before the test sends SIGTERM). It returns the job; the caller
# must stop it after the assertion.
function Start-AsyncSession {
    param([int] $Port)
    return Start-Job -ScriptBlock {
        param($p)
        # The upstream block writes one command and then sleeps; the pipeline
        # stays open, so ssh stdin never reaches EOF while the job runs.
        & { 'echo runtime-real-probe'; Start-Sleep -Seconds 120 } |
            ssh -tt -o StrictHostKeyChecking=no -o UserKnownHostsFile=NUL `
                -o ConnectTimeout=5 -p $p alice@127.0.0.1 2>&1
    } -ArgumentList $Port
}

# ---------------------------------------------------------------------------
# Set-up
# ---------------------------------------------------------------------------

New-Item -ItemType Directory -Force -Path (Split-Path $ReceiptPath) | Out-Null
Set-Content -Path $ReceiptPath -Value ''

Write-Receipt 'VibeShell real-service SIGTERM qualification (rootless Podman)'
Write-Receipt "started (UTC) : $((Get-Date).ToUniversalTime().ToString('yyyy-MM-ddTHH:mm:ssZ'))"
Write-Receipt "worktree      : $((git rev-parse --show-toplevel 2>$null) ?? 'unknown')"
Write-Receipt "branch/commit : $((git rev-parse --abbrev-ref HEAD 2>$null) ?? 'unknown') $((git rev-parse --short HEAD 2>$null) ?? 'unknown')"
Write-Receipt "image         : $IMAGE"

$rootless = (& podman info --format '{{.Host.Security.Rootless}}' 2>$null)
Write-Receipt "rootless      : $rootless"

New-MinimalConfig -Path $ConfigPath

try {
    Write-Heading 'build'
    & podman image exists $IMAGE
    if ($LASTEXITCODE -ne 0) {
        Write-Receipt "image missing; building target runtime"
        & podman build --file containers/Containerfile --target runtime --tag $IMAGE .
        if ($LASTEXITCODE -ne 0) { throw 'podman build failed' }
    }
    $imageId = (& podman image inspect $IMAGE --format '{{.Id}}')
    $imageStopSignal = (& podman image inspect $IMAGE --format '{{.Config.StopSignal}}')
    Write-Receipt "image id      : $imageId"
    Write-Receipt "image Stopsignal: $imageStopSignal"

    # A fresh named volume per run keeps state fully isolated from other agents.
    & podman volume create $Volume | Out-Null
    if ($LASTEXITCODE -ne 0) { throw 'failed to create state volume' }

    $runArgs = @(
        '--user', $ServiceUID,
        '--read-only',
        '--tmpfs', '/tmp',
        '--cap-drop=ALL',
        '--security-opt', 'no-new-privileges',
        '--memory=512m', '--pids-limit=100',
        '--publish', "127.0.0.1::$PublishedPort",
        '-v', "${ConfigPath}:/etc/vibeshell/vibeshell.json:ro,Z",
        '-v', "${Volume}:/var/lib/vibeshell"
    )

    Write-Heading 'start real service under hardened flags'
    Write-Receipt "podman run -d --name $Name $($runArgs -join ' ') $IMAGE -config /etc/vibeshell/vibeshell.json"
    & podman run -d --name $Name @runArgs $IMAGE -config /etc/vibeshell/vibeshell.json | Out-Null
    if ($LASTEXITCODE -ne 0) { throw 'failed to start the service container' }

    # Wait for readiness: the service logs "vibeshell ready" only after the
    # listener is accepting, so a session opened after that cannot race startup.
    $ready = $false
    for ($attempt = 0; $attempt -lt 40 -and -not $ready; $attempt++) {
        $logs = (& podman logs $Name 2>&1 | Out-String)
        if ($logs -match 'vibeshell ready') { $ready = $true } else { Start-Sleep -Milliseconds 250 }
    }

    $hostPort = $null
    $portsJson = (& podman inspect $Name --format '{{json .NetworkSettings.Ports}}')
    try {
        $ports = $portsJson | ConvertFrom-Json -AsHashtable
        if ($ports.ContainsKey("$PublishedPort/tcp")) { $hostPort = [int]$ports["$PublishedPort/tcp"][0].HostPort }
    } catch { $hostPort = $null }

    Add-Check -Id '1' -Title 'real service starts and reaches readiness under the hardened flags' `
        -Command "podman logs $Name" `
        -Observed "ready=$ready; published 127.0.0.1:$hostPort -> $PublishedPort/tcp; logs: $(($logs -replace "`r?`n", ' | ').Trim())" `
        -Expected 'the "vibeshell ready" log line appears and the SSH port is published on loopback' `
        -Ok ($ready -and $null -ne $hostPort)

    # Open one real SSH session and give it a moment to register a turn, so a
    # session.end really has something to close out (check 3).
    Write-Heading 'open a real SSH session'
    $sessionJob = $null
    $sessionOpened = $false
    if ($ready -and $null -ne $hostPort) {
        $sessionJob = Start-AsyncSession -Port $hostPort
        Start-Sleep -Seconds 4
        $sessionOpened = ($sessionJob.State -eq 'Running')
    }
    Write-Receipt "session job state: $(if ($sessionJob) { $sessionJob.State } else { 'not started' })"

    # Inspect the database while the service is up: at least one session exists
    # and none is ended yet. `admin sessions list` opens the same database and
    # applies no destructive migration.
    $preList = ''
    if ($ready) {
        $preList = (& podman run --rm --entrypoint /usr/local/bin/vibeshell `
            -v "${ConfigPath}:/etc/vibeshell/vibeshell.json:ro,Z" `
            -v "${Volume}:/var/lib/vibeshell" `
            $IMAGE admin sessions list -config /etc/vibeshell/vibeshell.json 2>&1 | Out-String)
    }

    Add-Check -Id '2' -Title 'an open SSH session is recorded before shutdown' `
        -Command "podman run --rm ... $IMAGE admin sessions list" `
        -Observed ($preList -replace "`r?`n", ' | ').Trim() `
        -Expected 'one session row with ended=false' `
        -Ok ($sessionOpened -and ($preList -match 'false'))

    # -----------------------------------------------------------------------
    Write-Heading 'SIGTERM via podman stop'
    $stopSw = [System.Diagnostics.Stopwatch]::StartNew()
    & podman stop --time $StopTimeoutSeconds $Name | Out-Null
    $stopSw.Stop()
    $exitCode = (& podman inspect $Name --format '{{.State.ExitCode}}')
    $postLogs = (& podman logs $Name 2>&1 | Out-String)
    $shutdownBegan = $postLogs -match '"shutdown":"begin"'
    $shutdownComplete = $postLogs -match '"shutdown":"complete"'

    Add-Check -Id '3' -Title 'the real binary handles SIGTERM and exits cleanly (not SIGKILL 137)' `
        -Command "podman stop --time $StopTimeoutSeconds $Name; podman inspect $Name --format '{{.State.ExitCode}}'" `
        -Observed "exit code $exitCode; stop returned in $([math]::Round($stopSw.Elapsed.TotalSeconds,2))s; logs: $(($postLogs -replace "`r?`n", ' | ').Trim())" `
        -Expected ('exit code 0 and both "shutdown":"begin" and "shutdown":"complete" in the log; ' +
                   "137 would mean SIGTERM was ignored and the $StopTimeoutSeconds s timeout escalated to SIGKILL") `
        -Ok ($exitCode -eq '0' -and $shutdownBegan -and $shutdownComplete)

    # -----------------------------------------------------------------------
    Write-Heading 'durable session end'
    # The database survives on the named volume after the container stops, so
    # the session.end written during shutdown can be observed from a new
    # process. A session with ended=true proves the handler finished and the
    # event store was flushed before it closed.
    $postList = (& podman run --rm --entrypoint /usr/local/bin/vibeshell `
        -v "${ConfigPath}:/etc/vibeshell/vibeshell.json:ro,Z" `
        -v "${Volume}:/var/lib/vibeshell" `
        $IMAGE admin sessions list -config /etc/vibeshell/vibeshell.json 2>&1 | Out-String)

    $lines = @(($postList -split "`r?`n") | Where-Object { $_ -match '^ses_' })
    $notEnded = @($lines | Where-Object { $_ -notmatch 'true\s+false\s*$' })
    $allEnded = ($lines.Count -gt 0) -and ($notEnded.Count -eq 0)
    Add-Check -Id '4' -Title 'the session open at SIGTERM is durably ended (session.end recorded)' `
        -Command "podman run --rm ... $IMAGE admin sessions list" `
        -Observed ($postList -replace "`r?`n", ' | ').Trim() `
        -Expected 'every listed session has ended=true' `
        -Ok $allEnded

    if ($sessionJob) { Stop-Job $sessionJob; Remove-Job $sessionJob -Force }

    # -----------------------------------------------------------------------
    Write-Heading 'observations (no verdict)'
    $seccomp = (& podman run --rm --entrypoint sh $IMAGE -c 'grep -E "^Seccomp" /proc/self/status' 2>&1 | Out-String)
    Write-Observation -Title 'entrypoint and STOPSIGNAL' `
        -Observed "image Stopsignal=$imageStopSignal; the real process installs signal.NotifyContext(SIGINT,SIGTERM), so the kernel default action does not apply to PID 1" `
        -Note 'check 3 is the attributable test: a handler-less PID 1 is SIGKILLed (137) after the timeout.'

    Write-Heading 'summary'
    Write-Receipt "PASS : $script:PASS"
    Write-Receipt "FAIL : $script:FAIL"
    Write-Receipt "finished (UTC): $((Get-Date).ToUniversalTime().ToString('yyyy-MM-ddTHH:mm:ssZ'))"
    if ($script:FAIL -eq 0) { Write-Receipt "result: all $script:PASS verdict-bearing checks passed." Green }
    else { Write-Receipt "result: $script:FAIL check(s) failed." Red }
}
finally {
    Write-Heading 'cleanup'
    Get-Job | Where-Object { $_.State -ne 'Completed' } | Stop-Job -PassThru | Remove-Job -Force 2>$null
    & podman rm -f $Name 2>&1 | Out-Null
    & podman volume rm -f $Volume 2>&1 | Out-Null
    Remove-Item -Recurse -Force $StageDir -ErrorAction SilentlyContinue
    Write-Receipt "removed container $Name, volume $Volume, and staging dir"
    Set-Content -Path $ReceiptPath -Value (($receipt -join "`n") + "`n")
    Write-Receipt "receipt written to $ReceiptPath"
}

if ($script:FAIL -eq 0) { exit 0 } else { exit 1 }
