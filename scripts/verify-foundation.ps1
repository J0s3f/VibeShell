# Run from the integration checkout. This checks real container/worktree isolation.
[CmdletBinding()]
param()
Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$repo = Split-Path $PSScriptRoot -Parent
$dev = Join-Path $PSScriptRoot 'dev.ps1'
if (-not (Test-Path -LiteralPath $dev)) { throw 'Development helper is missing.' }

function Assert-Success([string]$Step) {
    if ($LASTEXITCODE -ne 0) { throw "$Step failed with exit code $LASTEXITCODE" }
}

& $dev image
Assert-Success 'Image build'
& $dev start
Assert-Success 'Integration container start'
& $dev smoke
Assert-Success 'Go toolchain smoke test'

$suffix = [Guid]::NewGuid().ToString('N').Substring(0, 8)
$agents = @("smoke-a-$suffix", "smoke-b-$suffix")
$worktrees = @()
try {
    foreach ($agent in $agents) {
        & (Join-Path $PSScriptRoot 'new-agent.ps1') -Name $agent
        Assert-Success "Create $agent"
        $path = Join-Path $repo ".worktrees/$agent"
        $worktrees += $path
        # Test the working copy of the tooling before committing it.
        foreach ($folder in @('scripts', 'containers')) {
            Copy-Item -LiteralPath (Join-Path $repo $folder) -Destination $path -Recurse -Force
        }
        & (Join-Path $path 'scripts/dev.ps1') start -PublishSsh
        Assert-Success "Start $agent"
    }

    $first = Join-Path $worktrees[0] 'scripts/dev.ps1'
    $second = Join-Path $worktrees[1] 'scripts/dev.ps1'
    & $first exec -Command @('sh', '-c', 'printf isolated > /state/probe; printf cache > /cache/probe; printf output > /out/probe; printf source > /workspace/.dev/probe; printf temporary > /tmp/probe')
    Assert-Success 'Write isolation probes'
    & $second exec -Command @('sh', '-c', 'test ! -e /state/probe && test ! -e /cache/probe && test ! -e /out/probe && test ! -e /workspace/.dev/probe && test ! -e /tmp/probe')
    Assert-Success 'Verify separate writable storage'
    & $first stop
    Assert-Success 'Stop agent container'
    & $first start -PublishSsh
    Assert-Success 'Restart agent container'
    & $first exec -Command @('sh', '-c', 'test "$(cat /state/probe)" = isolated')
    Assert-Success 'Verify retained state'
    & $second exec -Command @('sh', '-c', 'exit 23')
    if ($LASTEXITCODE -ne 23) { throw 'Container exit code was not preserved.' }
    $a = & $first status -AsJson | ConvertFrom-Json
    $b = & $second status -AsJson | ConvertFrom-Json
    if ($a.container -eq $b.container -or $a.ssh -eq $b.ssh -or -not $a.ssh) {
        throw 'Agent containers or dynamically assigned SSH ports overlap.'
    }
    Write-Output 'PASS: compiler/race detector, separate worktrees/containers/storage/ports, restart, and exit-code propagation.'
}
finally {
    foreach ($path in $worktrees) {
        & (Join-Path $path 'scripts/dev.ps1') stop
    }
    Write-Output 'Smoke resources are retained for inspection. See docs/development.md for cleanup after preserving receipts.'
}
