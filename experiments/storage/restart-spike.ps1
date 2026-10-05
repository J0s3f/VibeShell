#Requires -Version 7.2

<#
.SYNOPSIS
    Proves that world state on the /state volume survives a container replacement,
    and that a backup taken with VACUUM INTO restores into a fresh container.

.DESCRIPTION
    Two gates cannot be proven inside one process, because the claim is about what
    survives the process and the container itself:

    * gate 3, durability. A committed row has to still be there when a different
      container reads the same volume, including after a commit whose process ended
      without closing the database.
    * gate 8, backup and restore. A snapshot has to restore into a container that
      never saw the database it replaces.

    This script runs each phase in its own process and puts a container replacement
    between the pairs, using this checkout's scripts/dev.ps1, so the named volumes
    persist while the container does not. The full transcript is appended to
    experiments/storage/receipts/restart-transcript.txt inside the checkout.

    It stops the container when it finishes, leaving the volumes in place.

.EXAMPLE
    pwsh -File experiments/storage/restart-spike.ps1
#>
[CmdletBinding()]
param()

Set-StrictMode -Version Latest

# podman writes progress to stderr, so every native failure is checked by exit code
# rather than by turning error output into a terminating exception.
$ErrorActionPreference = 'Continue'
$PSNativeCommandErrorActionPreference = $false

$RepositoryRoot = Split-Path (Split-Path $PSScriptRoot -Parent) -Parent
$DevScript = Join-Path $RepositoryRoot 'scripts/dev.ps1'
$PhaseScript = '/workspace/experiments/storage/phases.sh'

# Each step names the SPIKE_PHASE value to run, whether the container is replaced
# before it runs, and the exit status the phase is expected to produce. A
# replacement means a new container process reading a volume the previous container
# left behind.
$Steps = @(
    @{
        Label = 'commit a row, then end the process without closing the database'
        Phase = 'write-unclean'
        Tests = 'TestDurableStateAcrossContainerReplacement'
        ReplaceContainer = $false
        # The phase ends the process abruptly, and a Go test binary can only do that
        # with a non-zero status, so the orchestrator expects 1 here.
        ExpectedExit = 1
    },
    @{
        Label = 'read it back in a new container'
        Phase = 'read'
        Tests = 'TestDurableStateAcrossContainerReplacement'
        ReplaceContainer = $true
        ExpectedExit = 0
    },
    @{
        Label = 'build a world and copy it with VACUUM INTO'
        Phase = 'backup-write'
        Tests = 'TestBackupAndRestore'
        ReplaceContainer = $false
        ExpectedExit = 0
    },
    @{
        Label = 'restore the copy and replace the live database'
        Phase = 'restore'
        Tests = 'TestBackupAndRestore'
        ReplaceContainer = $true
        ExpectedExit = 0
    },
    @{
        Label = 'read the replaced database in a third container'
        Phase = 'restore-verify'
        Tests = 'TestBackupAndRestore'
        ReplaceContainer = $true
        ExpectedExit = 0
    }
)

# The receipts the run has to produce. They are removed before the run so that a
# stale file from an earlier run cannot satisfy the check at the end.
$ExpectedReceipts = @(
    'durability-before-restart.txt',
    'durability-after-restart.txt',
    'backup-before-restore.txt',
    'backup-after-restore.txt',
    'backup-verify-after-restore.txt',
    'restart-transcript.txt'
)

function Invoke-Dev {
    <#
    .SYNOPSIS
        Runs scripts/dev.ps1 with one action and fails on its exit code.
    #>
    param(
        [Parameter(Mandatory)][string]$Action,
        [string[]]$Command
    )

    # The command vector is passed as the named -Command parameter rather than
    # splatted, because a splatted array cannot bind to a named parameter.
    if ($Command) {
        & $DevScript $Action -Command $Command
    }
    else {
        & $DevScript $Action
    }
    if ($LASTEXITCODE -ne 0) {
        throw "scripts/dev.ps1 $Action failed with exit code $LASTEXITCODE."
    }
}

function Invoke-Phase {
    param(
        [Parameter(Mandatory)][string]$Label,
        [Parameter(Mandatory)][string]$Phase,
        [Parameter(Mandatory)][string]$Tests,
        [Parameter(Mandatory)][int]$ExpectedExit
    )

    Write-Host ''
    Write-Host "==> $Label (SPIKE_PHASE=$Phase, expected exit $ExpectedExit)"
    Invoke-Dev -Action 'exec' -Command @('sh', $PhaseScript, $Phase, $Tests, "$ExpectedExit")
}

foreach ($receipt in $ExpectedReceipts) {
    $path = Join-Path $PSScriptRoot "receipts/$receipt"
    if (Test-Path -LiteralPath $path) {
        Remove-Item -LiteralPath $path -Force
    }
}

try {
    Invoke-Dev -Action 'start'

    foreach ($step in $Steps) {
        if ($step.ReplaceContainer) {
            Write-Host ''
            Write-Host '==> Replace the container, keeping the state volume'
            Invoke-Dev -Action 'stop'
            Invoke-Dev -Action 'start'
        }
        Invoke-Phase -Label $step.Label -Phase $step.Phase -Tests $step.Tests -ExpectedExit $step.ExpectedExit
    }

    Write-Host ''
    Invoke-Dev -Action 'stop'
}
catch {
    Write-Host "Restart spike failed: $_"
    exit 1
}

Write-Host ''
Write-Host 'Receipts:'
foreach ($receipt in $ExpectedReceipts) {
    $path = Join-Path $PSScriptRoot "receipts/$receipt"
    if (-not (Test-Path -LiteralPath $path)) {
        Write-Host "  MISSING $path"
        exit 1
    }
    Write-Host "  $path"
}