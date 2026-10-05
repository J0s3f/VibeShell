#!/usr/bin/env pwsh
# Re-runnable VibeShell runtime hardening qualification.
#
# Usage (from the repository root):
#   & .\experiments\runtime-hardening\run-hardening-tests.ps1
#
# The orchestrator owns every verdict. Each check prints the exact command it
# ran and the value it observed, so a reviewer can repeat it by hand. Checks
# whose result cannot be attributed to the hardening flags are recorded as
# observations, never as PASS.
#
# Exit code 0 means every verdict-bearing check passed.

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$IMAGE = 'localhost/vibeshell-runtime:hardening'
$ServiceUID = '10001'
$ExpectedPidLimit = '100'
$ExpectedMemoryMaxBytes = '536870912'
$PublishedPort = '2222'
$StopTimeoutSeconds = 3

$ReceiptPath = Join-Path $PSScriptRoot 'receipts/hardening-receipt.txt'
$InventoryScript = Join-Path $PSScriptRoot 'test-hardening.sh'

$runId = Get-Random
$HardenedName = "vibeshell-hardening-$runId"
# Identical to $HardenedName except for the omitted --cap-drop=ALL. Every
# capability verdict compares the two, so a result that is really caused by the
# non-root user or by rootless user namespaces cannot pass.
$ControlName = "vibeshell-control-$runId"

$receipt = [System.Collections.Generic.List[string]]::new()

function Write-Receipt {
    param(
        [string] $Text = '',
        [ConsoleColor] $Color = [ConsoleColor]::Gray
    )
    Write-Host $Text -ForegroundColor $Color
    $receipt.Add($Text)
}

function Write-Heading {
    param([string] $Text)
    Write-Receipt ''
    Write-Receipt "=== $Text ===" Cyan
}

$script:PASS = 0
$script:FAIL = 0
$script:NOTCHECKED = 0

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
    if ($Ok) {
        Write-Receipt "     verdict : PASS" Green
        $script:PASS++
    } else {
        Write-Receipt "     verdict : FAIL" Red
        $script:FAIL++
    }
}

function Add-NotChecked {
    param(
        [Parameter(Mandatory)] [string] $Id,
        [Parameter(Mandatory)] [string] $Title,
        [Parameter(Mandatory)] [string] $Reason
    )
    Write-Receipt "[$Id] $Title"
    Write-Receipt "     reason  : $Reason"
    Write-Receipt "     verdict : NOT CHECKED" Yellow
    $script:NOTCHECKED++
}

function Add-Observation {
    param(
        [Parameter(Mandatory)] [string] $Title,
        [Parameter(Mandatory)] [string] $Observed,
        [string] $Note = ''
    )
    Write-Receipt "- $Title"
    Write-Receipt "     observed: $Observed"
    if ($Note -ne '') {
        Write-Receipt "     note    : $Note"
    }
}

function Invoke-InContainer {
    <#
      Runs a shell script inside a container and reports both the merged
      output and the container process exit code. $LASTEXITCODE is read
      immediately after the native call so later PowerShell work cannot
      clobber it.
    #>
    param(
        [Parameter(Mandatory)] [string] $Container,
        [Parameter(Mandatory)] [string] $ShellScript,
        [string] $User = ''
    )
    $userArgs = @()
    if ($User -ne '') { $userArgs = @('--user', $User) }
    $captured = & podman exec @userArgs $Container sh -c $ShellScript 2>&1
    $code = $LASTEXITCODE
    [pscustomobject]@{
        Output   = (($captured | Out-String).TrimEnd())
        ExitCode = $code
    }
}

$CAP_NAMES = @(
    'CAP_CHOWN', 'CAP_DAC_OVERRIDE', 'CAP_DAC_READ_SEARCH', 'CAP_FOWNER', 'CAP_FSETID',
    'CAP_KILL', 'CAP_SETGID', 'CAP_SETUID', 'CAP_SETPCAP', 'CAP_LINUX_IMMUTABLE',
    'CAP_NET_BIND_SERVICE', 'CAP_NET_BROADCAST', 'CAP_NET_ADMIN', 'CAP_NET_RAW',
    'CAP_IPC_LOCK', 'CAP_IPC_OWNER', 'CAP_SYS_MODULE', 'CAP_SYS_RAWIO', 'CAP_SYS_CHROOT',
    'CAP_SYS_PTRACE', 'CAP_SYS_PACCT', 'CAP_SYS_ADMIN', 'CAP_SYS_BOOT', 'CAP_SYS_NICE',
    'CAP_SYS_RESOURCE', 'CAP_SYS_TIME', 'CAP_SYS_TTY_CONFIG', 'CAP_MKNOD', 'CAP_LEASE',
    'CAP_AUDIT_WRITE', 'CAP_AUDIT_CONTROL', 'CAP_SETFCAP', 'CAP_MAC_OVERRIDE',
    'CAP_MAC_ADMIN', 'CAP_SYSLOG', 'CAP_WAKE_ALARM', 'CAP_BLOCK_SUSPEND', 'CAP_AUDIT_READ',
    'CAP_PERFMON', 'CAP_BPF', 'CAP_CHECKPOINT_RESTORE'
)

function Convert-CapMask {
    param([string] $Mask)
    $value = [Convert]::ToUInt64($Mask, 16)
    $names = @()
    for ($bit = 0; $bit -lt $CAP_NAMES.Count; $bit++) {
        if (($value -band ([uint64]1 -shl $bit)) -ne 0) { $names += $CAP_NAMES[$bit] }
    }
    if ($names.Count -eq 0) { return '<empty>' }
    return ($names -join ',')
}

function Get-CapField {
    param([string] $Container, [string] $Field, [string] $User = '')
    $result = Invoke-InContainer -Container $Container -User $User `
        -ShellScript ("grep ""^${Field}:"" /proc/self/status | sed 's/^.*:[[:space:]]*//'")
    if ($result.ExitCode -ne 0) { return "<unreadable>" }
    return $result.Output.Trim()
}

# ---------------------------------------------------------------------------
# Set-up
# ---------------------------------------------------------------------------

New-Item -ItemType Directory -Force -Path (Split-Path $ReceiptPath) | Out-Null
Set-Content -Path $ReceiptPath -Value ''

Write-Receipt "VibeShell runtime hardening qualification (rootless Podman)"
Write-Receipt "started (UTC) : $((Get-Date).ToUniversalTime().ToString('yyyy-MM-ddTHH:mm:ssZ'))"
Write-Receipt "worktree      : $((git rev-parse --show-toplevel 2>$null) ?? 'unknown')"
Write-Receipt "branch/commit : $((git rev-parse --abbrev-ref HEAD 2>$null) ?? 'unknown') $((git rev-parse --short HEAD 2>$null) ?? 'unknown')"
Write-Receipt "image         : $IMAGE"

$clientVersion = (& podman version --format '{{.Client.Version}}' 2>$null)
$serverVersion = (& podman version --format '{{.Server.Version}}' 2>$null)
$rootless = (& podman info --format '{{.Host.Security.Rootless}}' 2>$null)
$networkBackend = (& podman info --format '{{.Host.NetworkBackend}}' 2>$null)
Write-Receipt "podman client : $clientVersion"
Write-Receipt "podman server : $serverVersion"
Write-Receipt "rootless      : $rootless"
Write-Receipt "net backend   : $networkBackend"

# `podman image exists` prints nothing and reports through its exit code, so the
# test must read $LASTEXITCODE. The earlier revision tested the command's output
# instead and therefore rebuilt the image on every run.
& podman image exists $IMAGE
if ($LASTEXITCODE -ne 0) {
    Write-Receipt "image missing; building"
    & podman build --file containers/Containerfile --target runtime --tag $IMAGE .
    if ($LASTEXITCODE -ne 0) { throw "podman build failed" }
}

$imageId = (& podman image inspect $IMAGE --format '{{.Id}}')
$imageDigest = (& podman image inspect $IMAGE --format '{{.Digest}}')
$imageStopSignal = (& podman image inspect $IMAGE --format '{{.Config.StopSignal}}')
$imageExposed = (& podman image inspect $IMAGE --format '{{json .Config.ExposedPorts}}')
Write-Receipt "image id      : $imageId"
Write-Receipt "image digest  : $imageDigest"
Write-Receipt "image Stopsignal: $imageStopSignal"
Write-Receipt "image EXPOSEs : $imageExposed"

# Flags shared by the hardened and control containers. The entrypoint is added
# per container so a shell-based probe never inherits `--entrypoint sleep`.
$commonRunArgs = @(
    '--user', $ServiceUID,
    '--read-only',
    '--tmpfs', '/tmp',
    '--security-opt', 'no-new-privileges',
    '--memory=512m',
    "--pids-limit=$ExpectedPidLimit",
    '--publish', "127.0.0.1::$PublishedPort"
)
$hardenedArgs = $commonRunArgs + @('--cap-drop=ALL', '--entrypoint', 'sleep')
# Same hardening flags as $hardenedArgs but with a shell entrypoint, for the
# stop-signal probes whose whole point is to install and receive a handler.
$shellHardenedArgs = $commonRunArgs + @('--cap-drop=ALL', '--entrypoint', 'sh')
# Identical to $hardenedArgs except for the omitted --cap-drop=ALL, so every
# capability difference the run observes is attributable to that one flag.
$controlArgs = $commonRunArgs + @('--entrypoint', 'sleep')

Write-Heading 'containers'
Write-Receipt "hardened: podman run -d --name $HardenedName $($hardenedArgs -join ' ') $IMAGE 300"
& podman run -d --name $HardenedName @hardenedArgs $IMAGE 300 | Out-Null
if ($LASTEXITCODE -ne 0) { throw "failed to start hardened container" }

Write-Receipt "control : podman run -d --name $ControlName $($controlArgs -join ' ') $IMAGE 300"
Write-Receipt "          (identical to the hardened container except --cap-drop=ALL)"
& podman run -d --name $ControlName @controlArgs $IMAGE 300 | Out-Null
if ($LASTEXITCODE -ne 0) { throw "failed to start control container" }

$hardenedState = (& podman inspect $HardenedName --format '{{.State.Status}}')
$controlState = (& podman inspect $ControlName --format '{{.State.Status}}')
Write-Receipt "hardened state: $hardenedState"
Write-Receipt "control  state: $controlState"

try {
    # -----------------------------------------------------------------------
    Write-Heading 'checks'

    # [1] Non-root uid
    $uid = Invoke-InContainer -Container $HardenedName -ShellScript 'id -u'
    Add-Check -Id '1' -Title 'service process is not root' `
        -Command "podman exec $HardenedName sh -c 'id -u'" `
        -Observed "$($uid.Output.Trim()) (exit $($uid.ExitCode))" `
        -Expected $ServiceUID `
        -Ok ($uid.ExitCode -eq 0 -and $uid.Output.Trim() -eq $ServiceUID)

    # [2] Read-only root filesystem
    $roWrite = Invoke-InContainer -Container $HardenedName -ShellScript 'touch /vibeshell-ro-probe 2>&1; echo "touch_rc=$?"'
    $roMount = Invoke-InContainer -Container $HardenedName -ShellScript 'grep -E "^[a-z0-9]+ / " /proc/mounts | cut -d" " -f1,2,4 | cut -d, -f1'
    $roOk = $roWrite.Output -match 'touch_rc=[1-9]' -and $roMount.Output.Trim() -eq 'overlay / ro'
    Add-Check -Id '2' -Title 'root filesystem is read-only' `
        -Command "podman exec $HardenedName sh -c 'touch /vibeshell-ro-probe'" `
        -Observed "$($roWrite.Output.Trim()) | root mount (fstype target first-option): $($roMount.Output.Trim())" `
        -Expected 'touch_rc=<non-zero> and "overlay / ro"' `
        -Ok $roOk

    # [3] Capability set is empty, and the drop is attributable
    #     CapEff is 0 for any non-root process, so an empty CapEff alone cannot
    #     show that --cap-drop=ALL did anything. CapBnd is the inheritable
    #     bounding set the kernel keeps regardless of uid, so the comparison
    #     below measures the flag itself. CAP_CHOWN is used as the attributable
    #     operation because it is a plain inode-ownership check that succeeds
    #     for root in a rootless container.
    $capScript = 'grep -E "^(CapInh|CapPrm|CapEff|CapBnd|CapAmb):" /proc/self/status'
    $hardBnd = Get-CapField -Container $HardenedName -Field 'CapBnd'
    $hardEff = Get-CapField -Container $HardenedName -Field 'CapEff'
    $ctrlBnd = Get-CapField -Container $ControlName -Field 'CapBnd'
    # The container's configured user is 10001, for which CapEff is always 0.
    # Measure the control's effective set as uid 0, the only uid there that can
    # exercise a capability.
    $ctrlEffRoot = Get-CapField -Container $ControlName -Field 'CapEff' -User '0'

    $chownScript = 'touch /tmp/capprobe 2>/dev/null; chown 1:1 /tmp/capprobe 2>/dev/null && echo CHOWN_OK || echo CHOWN_DENIED'
    $chownHardened = Invoke-InContainer -Container $HardenedName -User '0' -ShellScript $chownScript
    $chownControl = Invoke-InContainer -Container $ControlName -User '0' -ShellScript $chownScript

    $capsEmpty = ($hardEff -eq '0000000000000000' -and $hardBnd -eq '0000000000000000')
    $capsDiffer = ($ctrlBnd -ne '0000000000000000' -and $ctrlBnd -ne $hardBnd)
    $capsAttributable = ($chownControl.Output.Trim() -eq 'CHOWN_OK' -and $chownHardened.Output.Trim() -eq 'CHOWN_DENIED')
    Add-Check -Id '3' -Title 'capability set is empty and --cap-drop=ALL is attributable' `
        -Command "podman exec $HardenedName sh -c '$capScript' ; podman exec -u 0 <container> sh -c '$chownScript'" `
        -Observed ("hardened CapEff=$hardEff CapBnd=$hardBnd [$(Convert-CapMask $hardBnd)] | " +
                   "control CapEff=$ctrlEffRoot CapBnd=$ctrlBnd [$(Convert-CapMask $ctrlBnd)] | " +
                   "chown as uid 0: hardened=$($chownHardened.Output.Trim()) control=$($chownControl.Output.Trim())") `
        -Expected 'hardened CapEff=0 and CapBnd=0; control CapBnd non-zero; chown allowed in control, denied when hardened' `
        -Ok ($capsEmpty -and $capsDiffer -and $capsAttributable)

    # [4] PID limit
    $pids = Invoke-InContainer -Container $HardenedName -ShellScript 'cat /sys/fs/cgroup/pids.max'
    Add-Check -Id '4' -Title 'PID limit is enforced' `
        -Command "podman exec $HardenedName sh -c 'cat /sys/fs/cgroup/pids.max'" `
        -Observed "$($pids.Output.Trim()) (exit $($pids.ExitCode))" `
        -Expected $ExpectedPidLimit `
        -Ok ($pids.ExitCode -eq 0 -and $pids.Output.Trim() -eq $ExpectedPidLimit)

    # [5] Memory limit
    $mem = Invoke-InContainer -Container $HardenedName -ShellScript 'cat /sys/fs/cgroup/memory.max'
    Add-Check -Id '5' -Title 'memory limit is enforced' `
        -Command "podman exec $HardenedName sh -c 'cat /sys/fs/cgroup/memory.max'" `
        -Observed "$($mem.Output.Trim()) bytes (exit $($mem.ExitCode))" `
        -Expected "$ExpectedMemoryMaxBytes bytes (512 MiB)" `
        -Ok ($mem.ExitCode -eq 0 -and $mem.Output.Trim() -eq $ExpectedMemoryMaxBytes)

    # [6] Only 2222 is published, and only on loopback
    $portsJson = (& podman inspect $HardenedName --format '{{json .NetworkSettings.Ports}}')
    $exposedJson = (& podman image inspect $IMAGE --format '{{json .Config.ExposedPorts}}')
    $portsOk = $false
    $portsObserved = "$portsJson"
    try {
        $ports = $portsJson | ConvertFrom-Json -AsHashtable
        $exposed = $exposedJson | ConvertFrom-Json -AsHashtable
        $exposedOk = ($exposed.Count -eq 1 -and $exposed.ContainsKey("$PublishedPort/tcp"))
        $portsOk = $exposedOk -and $ports.Count -eq 1 -and $ports.ContainsKey("$PublishedPort/tcp") -and
                   $ports["$PublishedPort/tcp"].Count -eq 1 -and
                   $ports["$PublishedPort/tcp"][0].HostIp -eq '127.0.0.1'
        $portsObserved = "$portsJson (image EXPOSEs: $exposedJson)"
    } catch {
        $portsObserved = "$portsJson (unparseable: $_)"
    }
    Add-Check -Id '6' -Title 'only the SSH port is published, on loopback' `
        -Command "podman inspect $HardenedName --format '{{json .NetworkSettings.Ports}}'" `
        -Observed $portsObserved `
        -Expected "one entry $PublishedPort/tcp bound to 127.0.0.1; image EXPOSEs only $PublishedPort/tcp" `
        -Ok $portsOk

    # [7] no-new-privileges reached the kernel
    $nnp = Get-CapField -Container $HardenedName -Field 'NoNewPrivs'
    Add-Check -Id '7' -Title 'no-new-privileges is set for the container process' `
        -Command "podman exec $HardenedName sh -c 'grep ""^NoNewPrivs:"" /proc/self/status'" `
        -Observed "NoNewPrivs=$nnp" `
        -Expected 'NoNewPrivs=1' `
        -Ok ($nnp -eq '1')

    # [8] No host filesystem is mounted in
    #     Every legitimate source in this image is an in-container filesystem
    #     type, so a host bind mount shows up as an unexpected type.
    $allowedFsTypes = @('tmpfs', 'overlay', 'proc', 'sysfs', 'cgroup2', 'devpts', 'mqueue', 'none', 'shm', 'devtmpfs')
    $mountsRaw = Invoke-InContainer -Container $HardenedName -ShellScript 'cat /proc/mounts'
    $unexpected = @()
    foreach ($line in $mountsRaw.Output -split "`n") {
        $fields = $line.Trim() -split '\s+'
        if ($fields.Count -lt 3) { continue }
        if ($allowedFsTypes -notcontains $fields[2]) {
            $unexpected += "$($fields[2]) $($fields[1]) <- $($fields[0])"
        }
    }
    Add-Check -Id '8' -Title 'no host filesystem, directory or engine socket is mounted' `
        -Command "podman exec $HardenedName sh -c 'cat /proc/mounts'" `
        -Observed "$($unexpected.Count) mount(s) with a filesystem type outside the container-local allowlist ($($allowedFsTypes -join ',')); $(($mountsRaw.Output -split "`n" | Where-Object { $_ }).Count) mounts total" `
        -Expected '0 mounts from a host filesystem' `
        -Ok ($unexpected.Count -eq 0)
    foreach ($entry in $unexpected) { Write-Receipt "     offending: $entry" Red }

    # [9] A process that installs a SIGTERM handler runs that handler
    $handlerScript = 'trap "echo HANDLER_RAN; exit 0" TERM; echo READY; kill -TERM $$; wait; echo REACHED_AFTER_SIGNAL'
    $handler = Invoke-InContainer -Container $HardenedName -ShellScript $handlerScript
    $handlerOk = $handler.ExitCode -eq 0 -and $handler.Output -match 'HANDLER_RAN' -and $handler.Output -notmatch 'REACHED_AFTER_SIGNAL'
    Add-Check -Id '9' -Title 'SIGTERM is delivered and a handler that installed one runs' `
        -Command "podman exec $HardenedName sh -c '$handlerScript'" `
        -Observed "exit $($handler.ExitCode); output: $($handler.Output -replace "`n", ' | ')" `
        -Expected 'exit 0, output contains HANDLER_RAN and does not reach REACHED_AFTER_SIGNAL' `
        -Ok $handlerOk

    # [10] `podman stop` reaches the handler instead of escalating to SIGKILL
    $stopName = "vibeshell-stopsignal-$runId"
    $stopHandlerScript = 'trap "echo SIGTERM_HANDLED; exit 0" TERM; echo READY; sleep 300 & wait'
    Write-Receipt ("stop-signal container: podman run -d --name $stopName " +
                   "$($shellHardenedArgs -join ' ') $IMAGE -c '$stopHandlerScript'")
    & podman run -d --name $stopName @shellHardenedArgs $IMAGE -c $stopHandlerScript | Out-Null
    if ($LASTEXITCODE -ne 0) { throw 'failed to start stop-signal container' }

    $ready = $false
    for ($attempt = 0; $attempt -lt 20 -and -not $ready; $attempt++) {
        $logs = (& podman logs $stopName 2>&1 | Out-String)
        if ($logs -match 'READY') { $ready = $true } else { Start-Sleep -Milliseconds 250 }
    }
    if ($ready) {
        & podman stop --time $StopTimeoutSeconds $stopName | Out-Null
        $stopState = (& podman inspect $stopName --format '{{.State.ExitCode}}')
        $stopLogs = (& podman logs $stopName 2>&1 | Out-String)
        $stopOk = $stopState -eq '0' -and $stopLogs -match 'SIGTERM_HANDLED'
        $stopObserved = "container exit code $stopState; logs: $(($stopLogs -replace "`r?`n", ' | ').Trim())"
        $stopExpected = "exit code 0 and SIGTERM_HANDLED in the log; 137 would mean SIGTERM was ignored and the $StopTimeoutSeconds s timeout escalated to SIGKILL"
        $stopCommand = "podman stop --time $StopTimeoutSeconds $stopName"
    } else {
        $stopOk = $false
        $stopObserved = 'container never printed READY; stop-signal check not exercised'
        $stopExpected = 'READY, then exit code 0'
        $stopCommand = "podman stop --time $StopTimeoutSeconds $stopName"
    }
    Add-Check -Id '10' -Title 'podman stop delivers STOPSIGNAL and the handler runs before the SIGKILL timeout' `
        -Command $stopCommand `
        -Observed $stopObserved `
        -Expected $stopExpected `
        -Ok $stopOk
    & podman rm -f $stopName | Out-Null

    # [11] Why the service's own stop behaviour was unverified at spike time
    $nohandlerScript = 'trap "" TERM; echo READY; sleep 300 & wait'
    $nohandlerName = "vibeshell-nohandler-$runId"
    & podman run -d --name $nohandlerName @shellHardenedArgs $IMAGE -c $nohandlerScript | Out-Null
    if ($LASTEXITCODE -eq 0) {
        Start-Sleep -Milliseconds 750
        & podman stop --time $StopTimeoutSeconds $nohandlerName | Out-Null
        $nohandlerCode = (& podman inspect $nohandlerName --format '{{.State.ExitCode}}')
        & podman rm -f $nohandlerName | Out-Null
        $noHandlerObserved = "exit code $nohandlerCode"
    } else {
        $noHandlerObserved = 'control container failed to start'
    }
    Add-NotChecked -Id '11' -Title 'SIGTERM handling of the real vibeshell service binary' `
        -Reason "Superseded. At the time of this run the containers/Containerfile ENTRYPOINT was the cmd/vibeshell stub, which prints a version line and exits 0 and never installs a signal handler. Observed placeholder behaviour without a handler: $noHandlerObserved. The real binary's SIGTERM handling is now checked by experiments/runtime-hardening/run-real-service-sigterm.ps1 against the composed service (4 PASS / 0 FAIL): clean exit 0 and durable session.end. This legacy check is retained for the stub-era receipt only."

    # -----------------------------------------------------------------------
    Write-Heading 'observations (no verdict)'

    $rawSocketScript = 'perl -e ''use Socket; socket(T, AF_INET, SOCK_STREAM, 0) and print "tcp_socket=ok\n"; socket(R, AF_INET, SOCK_RAW, IPPROTO_ICMP) or die "raw_socket_errno=$!\n"; print "raw_socket=ok\n"'''
    $rawHardened = Invoke-InContainer -Container $HardenedName -User $ServiceUID -ShellScript $rawSocketScript
    $rawControl = Invoke-InContainer -Container $ControlName -User '0' -ShellScript $rawSocketScript
    Add-Observation -Title 'raw-socket probe is not attributable on this host' `
        -Observed "hardened as uid $ServiceUID (CapEff=$hardEff): $($rawHardened.Output -replace "`n", ' | '); control as uid 0 (CapEff=$ctrlEffRoot, CapBnd includes CAP_NET_RAW): $($rawControl.Output -replace "`n", ' | ')" `
        -Note 'The control container has effective capabilities as uid 0 (its chown succeeded in check [3]) and still gets the same "Protocol not supported" errno, so the raw ICMP socket is refused by the network namespace rather than by a capability check. This probe therefore cannot pass or fail the capability check; check [3] uses CapBnd plus CAP_CHOWN instead.'

    $seccomp = Get-CapField -Container $HardenedName -Field 'Seccomp'
    $seccompFilters = Get-CapField -Container $HardenedName -Field 'Seccomp_filters'
    Add-Observation -Title 'seccomp filter state' -Observed "Seccomp=$seccomp Seccomp_filters=$seccompFilters" `
        -Note 'Seccomp=2 is SECCOMP_MODE_FILTER. Which syscalls the filter blocks was not audited.'

    $stubName = "vibeshell-stub-$runId"
    $stubOut = & podman run --name $stubName --user $ServiceUID --read-only --tmpfs /tmp --cap-drop=ALL --security-opt no-new-privileges $IMAGE 2>&1 | Out-String
    $stubCode = $LASTEXITCODE
    $stubState = (& podman inspect $stubName --format '{{.State.ExitCode}}' 2>$null)
    & podman rm -f $stubName | Out-Null
    Add-Observation -Title 'image entrypoint behaviour' -Observed "output '$($stubOut.Trim())', exit code $stubCode, container exit $stubState" `
        -Note 'The service stub returns immediately, so nothing about a long-running service could be measured.'

    $dockerCommand = Get-Command docker -ErrorAction SilentlyContinue
    if ($null -ne $dockerCommand) {
        Add-Observation -Title 'Docker' -Observed ((& docker version 2>&1 | Out-String).Trim())
    } else {
        Add-Observation -Title 'Docker compatibility' -Observed 'docker is not installed on this host; the check could not be run' `
            -Note 'UNVERIFIED. The Containerfile is a plain multi-stage Dockerfile and every flag used is accepted by Docker, but nothing here demonstrates that.'
    }

    # -----------------------------------------------------------------------
    Write-Heading 'in-container inventory (experiments/runtime-hardening/test-hardening.sh)'
    Write-Receipt "command: Get-Content test-hardening.sh | podman exec -i $HardenedName sh"
    $inventory = Get-Content -Raw $InventoryScript | & podman exec -i $HardenedName sh 2>&1 | Out-String
    foreach ($line in ($inventory -split "`r?`n")) { Write-Receipt "  $($line.TrimEnd())" }

    # -----------------------------------------------------------------------
    Write-Heading 'summary'
    Write-Receipt "PASS        : $script:PASS"
    Write-Receipt "FAIL        : $script:FAIL"
    Write-Receipt "NOT CHECKED : $script:NOTCHECKED"
    Write-Receipt "finished (UTC): $((Get-Date).ToUniversalTime().ToString('yyyy-MM-ddTHH:mm:ssZ'))"
    if ($script:FAIL -eq 0) {
        Write-Receipt "result: all $script:PASS verdict-bearing checks passed; $script:NOTCHECKED check(s) remain unverified." Green
    } else {
        Write-Receipt "result: $script:FAIL check(s) failed." Red
    }
}
finally {
    Write-Heading 'cleanup'
    & podman rm -f $HardenedName $ControlName 2>&1 | Out-Null
    Write-Receipt "removed $HardenedName and $ControlName"
    Set-Content -Path $ReceiptPath -Value (($receipt -join "`n") + "`n")
    Write-Receipt "receipt written to $ReceiptPath"
}

if ($script:FAIL -eq 0) { exit 0 } else { exit 1 }