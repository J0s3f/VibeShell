#Requires -Version 7.2

<#
.SYNOPSIS
    Runs VibeShell development tasks inside this checkout's Podman container.

.DESCRIPTION
    Nothing is installed on the host: podman and git are the only host tools
    used, and every compile, test, and experiment runs inside a container.

    Each checkout owns its resources. The prefix comes from the checkout path:
    <repository>/.worktrees/<agent> becomes vibeshell-<agent>, and the
    integration checkout becomes vibeshell-main. Container, module cache, build
    cache, durable state, home, and installed-tool volumes, and the optional
    published SSH port, therefore never overlap between two checkouts.

    The dev image is content addressed by the hash of containers/Containerfile
    and containers/versions.env, so a checkout that pins different images keeps
    using its own toolchain after another checkout updates the pins.

.EXAMPLE
    scripts/dev.ps1 image
    Builds the development toolchain image for this checkout's pins.

.EXAMPLE
    scripts/dev.ps1 start -PublishSsh
    Creates this checkout's container, optionally publishing SSH on a
    dynamically assigned host port bound to 127.0.0.1.

.EXAMPLE
    scripts/dev.ps1 exec -Command sh -c 'go test ./internal/...'
    Runs a command in the container and propagates its exit code.

.EXAMPLE
    scripts/dev.ps1 status -AsJson
    Prints {"container":"vibeshell-main-dev","ssh":null} for tooling.
#>
[CmdletBinding()]
param(
    [Parameter(Position = 0)]
    [ValidateSet('image', 'start', 'stop', 'exec', 'smoke', 'test', 'build', 'status', 'clean')]
    [string]$Action = 'status',

    # Argument vector for exec, forwarded to podman exec unchanged.
    [string[]]$Command,

    # Publish the container's SSH port on a dynamically assigned 127.0.0.1 port.
    [switch]$PublishSsh,

    # Emit status as one JSON line for tooling.
    [switch]$AsJson,

    # Also delete this checkout's named volumes (clean only).
    [switch]$RemoveVolumes
)

Set-StrictMode -Version Latest

# Callers such as scripts/verify-foundation.ps1 set $ErrorActionPreference to
# Stop, and podman writes progress to stderr. Native failures must stay
# non-terminating here so that each step can report its own failure and exit
# with the exit code that produced it.
$ErrorActionPreference = 'Continue'
$PSNativeCommandErrorActionPreference = $false

# ---------------------------------------------------------------------------
# Names and paths
# ---------------------------------------------------------------------------

# Container paths. /workspace is the checkout, /out and /state are container
# directories, and /cache/gomod plus /cache/gobuild receive named volumes.
$WorkspacePath = '/workspace'
$StatePath = '/state'
$SSHContainerPort = 2222

# /root and /usr/local/bin hold state the base image does not: the OpenCode CLI
# and its login live in the home directory, and the CLI's browser-open shim is
# an installed tool. Persisting both keeps that state across container
# recreation (for example, to change the published SSH port).
$HomePath = '/root'
$LocalBinPath = '/usr/local/bin'

function Get-CheckoutPrefix {
    <#
    .SYNOPSIS
        Derives the deterministic resource prefix for one checkout.
    #>
    param([Parameter(Mandatory)][string]$CheckoutPath)

    $full = [System.IO.Path]::GetFullPath($CheckoutPath).TrimEnd([System.IO.Path]::DirectorySeparatorChar)
    $identity = 'main'
    if ((Split-Path (Split-Path $full -Parent) -Leaf) -eq '.worktrees') {
        $identity = Split-Path $full -Leaf
    }

    # Podman accepts lowercase letters, digits, dot, dash, and underscore in
    # resource names, so anything else becomes a dash.
    $slug = $identity.ToLowerInvariant() -replace '[^a-z0-9._-]', '-'
    if ($slug -eq '') {
        throw "Cannot derive container names from the checkout path '$full'."
    }
    return "vibeshell-$slug"
}

function Get-PinnedArguments {
    <#
    .SYNOPSIS
        Reads the pinned build arguments from containers/versions.env.
    #>
    param([Parameter(Mandatory)][string]$RepositoryRoot)

    $versionsFile = Join-Path $RepositoryRoot 'containers/versions.env'
    if (-not (Test-Path -LiteralPath $versionsFile)) {
        throw "Pinned build arguments are missing: $versionsFile"
    }

    $pins = foreach ($line in Get-Content -LiteralPath $versionsFile) {
        $entry = $line.Trim()
        if ($entry -eq '' -or $entry.StartsWith('#')) { continue }
        if ($entry -notmatch '^([A-Z][A-Z0-9_]*)=(.+)$') {
            throw "Unreadable entry '$line' in $versionsFile"
        }
        [pscustomobject]@{ Name = $Matches[1]; Value = $Matches[2] }
    }
    if (-not $pins) {
        throw "No pinned build arguments found in $versionsFile"
    }
    return $pins
}

function Get-DevImageTag {
    <#
    .SYNOPSIS
        Returns the content addressed tag of the development image.
    #>
    param([Parameter(Mandatory)][string]$RepositoryRoot)

    $definitionFiles = @(
        (Join-Path $RepositoryRoot 'containers/Containerfile'),
        (Join-Path $RepositoryRoot 'containers/versions.env')
    )
    $definition = foreach ($definitionFile in $definitionFiles) {
        if (-not (Test-Path -LiteralPath $definitionFile)) {
            throw "Cannot hash the development image definition: $definitionFile is missing."
        }
        # Only the file name and the contents take part in the hash. Including
        # the checkout's absolute path would give every worktree its own tag and
        # rebuild an identical image.
        "$(Split-Path $definitionFile -Leaf)`n$(Get-Content -LiteralPath $definitionFile -Raw)"
    }

    $sha256 = [System.Security.Cryptography.SHA256]::Create()
    $digest = $sha256.ComputeHash([System.Text.Encoding]::UTF8.GetBytes($definition -join ''))
    $short = [System.BitConverter]::ToString($digest).Replace('-', '').ToLowerInvariant().Substring(0, 12)
    return "localhost/vibeshell-dev:$short"
}

function Assert-ContainerfileMatchesPins {
    <#
    .SYNOPSIS
        Fails when a pin and the Containerfile ARG default disagree.
    #>
    param([Parameter(Mandatory)][string]$RepositoryRoot)

    $containerfile = Join-Path $RepositoryRoot 'containers/Containerfile'
    if (-not (Test-Path -LiteralPath $containerfile)) {
        throw "Containerfile is missing: $containerfile"
    }

    $definition = Get-Content -LiteralPath $containerfile -Raw
    foreach ($pin in Get-PinnedArguments -RepositoryRoot $RepositoryRoot) {
        $expected = '(?m)^\s*ARG\s+' + [regex]::Escape($pin.Name) + '\s*=\s*' + [regex]::Escape($pin.Value) + '\s*$'
        if ($definition -notmatch $expected) {
            throw "containers/Containerfile does not default ARG $($pin.Name) to the value pinned in containers/versions.env. Update both files together."
        }
    }
}

# ---------------------------------------------------------------------------
# Container queries
# ---------------------------------------------------------------------------

function Test-ContainerImageExists {
    param([Parameter(Mandatory)][string]$Name)

    & podman image exists $Name *> $null
    return ($LASTEXITCODE -eq 0)
}

function Test-ContainerExists {
    param([Parameter(Mandatory)][string]$Name)

    & podman container exists $Name *> $null
    return ($LASTEXITCODE -eq 0)
}

function Test-VolumeExists {
    param([Parameter(Mandatory)][string]$Name)

    & podman volume exists $Name *> $null
    return ($LASTEXITCODE -eq 0)
}

function Initialize-DevelopmentVolumes {
    <#
    .SYNOPSIS
        Creates this checkout's named volumes, labelled with their owner.
    #>
    # Podman creates a named volume at container creation without recording who
    # owns it, so the volumes are created here where they can be labelled.
    foreach ($volume in @($ModuleCacheVolume, $BuildCacheVolume, $StateVolume, $HomeVolume, $LocalBinVolume)) {
        if (Test-VolumeExists -Name $volume) { continue }

        $exitCode = Invoke-PodmanStep -Step "Create volume $volume" -PodmanArguments @(
            'volume', 'create',
            '--label', 'io.vibeshell.role=development',
            '--label', "io.vibeshell.checkout=$CheckoutPrefix",
            $volume
        )
        if ($exitCode -ne 0) {
            throw "Creating volume $volume failed with podman exit code $exitCode."
        }
    }
}

function Test-ContainerRunning {
    param([Parameter(Mandatory)][string]$Name)

    $state = & podman container inspect --format '{{.State.Running}}' $Name 2>$null
    if ($LASTEXITCODE -ne 0) { return $false }
    return ($state -eq 'true')
}

function Get-PublishedSshPort {
    <#
    .SYNOPSIS
        Returns the published host port, or $null when SSH is not published.
    #>
    param([Parameter(Mandatory)][string]$Name)

    $binding = @(& podman port $Name "$($SSHContainerPort)/tcp" 2>$null)
    if ($LASTEXITCODE -ne 0 -or $binding.Count -eq 0) { return $null }

    # One "<host>:<port>" line per binding, for example 127.0.0.1:46015.
    $first = $binding[0]
    if ([string]::IsNullOrWhiteSpace($first)) { return $null }
    if ($first -notmatch ':(\d+)$') {
        throw "Unexpected published port report from podman: $first"
    }
    return [int]$Matches[1]
}

function Invoke-PodmanStep {
    <#
    .SYNOPSIS
        Runs one labelled podman command and returns its exit code.
    #>
    param(
        [Parameter(Mandatory)][string]$Step,
        [Parameter(Mandatory)][string[]]$PodmanArguments
    )

    Write-Host "==> $Step"
    # Out-Host keeps podman's output on the console so that the function's only
    # pipeline output is the exit code.
    & podman @PodmanArguments | Out-Host
    return $LASTEXITCODE
}

function Invoke-ContainerStep {
    <#
    .SYNOPSIS
        Runs one command in this checkout's container and returns its exit code.
    #>
    param(
        [Parameter(Mandatory)][string]$Step,
        [Parameter(Mandatory)][string[]]$CommandArguments
    )

    $podmanArguments = @('exec', '--workdir', $WorkspacePath, $ContainerName) + $CommandArguments
    return Invoke-PodmanStep -Step $Step -PodmanArguments $podmanArguments
}

function Assert-ContainerRunning {
    if (-not (Test-ContainerExists -Name $ContainerName)) {
        throw "Container $ContainerName does not exist. Run scripts/dev.ps1 start first."
    }
    if (-not (Test-ContainerRunning -Name $ContainerName)) {
        throw "Container $ContainerName is not running. Run scripts/dev.ps1 start first."
    }
}

function New-LocalDirectory {
    <#
    .SYNOPSIS
        Creates a gitignored directory in this checkout.
    #>
    param([Parameter(Mandatory)][string]$Path)

    if (-not (Test-Path -LiteralPath $Path)) {
        New-Item -ItemType Directory -Force -Path $Path | Out-Null
    }
}

# ---------------------------------------------------------------------------
# Actions
# ---------------------------------------------------------------------------

function Invoke-ImageBuild {
    Assert-ContainerfileMatchesPins -RepositoryRoot $RepositoryRoot

    $tag = Get-DevImageTag -RepositoryRoot $RepositoryRoot
    $buildArguments = @(
        'build',
        '--file', (Join-Path $RepositoryRoot 'containers/Containerfile'),
        '--target', 'dev',
        '--tag', $tag,
        '--tag', 'localhost/vibeshell-dev:latest'
    )
    foreach ($pin in Get-PinnedArguments -RepositoryRoot $RepositoryRoot) {
        $buildArguments += "--build-arg=$($pin.Name)=$($pin.Value)"
    }
    $buildArguments += $RepositoryRoot

    $exitCode = Invoke-PodmanStep -Step "Build $tag" -PodmanArguments $buildArguments
    if ($exitCode -ne 0) {
        throw "Development image build failed with podman exit code $exitCode."
    }
    Write-Host "Development image: $tag (also tagged localhost/vibeshell-dev:latest)"
    return 0
}

function Ensure-DevImage {
    $tag = Get-DevImageTag -RepositoryRoot $RepositoryRoot
    if (Test-ContainerImageExists -Name $tag) { return $tag }
    $null = Invoke-ImageBuild
    return $tag
}

function Invoke-ContainerStart {
    $tag = Ensure-DevImage

    # Verification probes and tooling write into the checkout's ignored .dev
    # directory, so start guarantees it exists in every checkout.
    New-LocalDirectory -Path (Join-Path $RepositoryRoot '.dev')

    $exists = Test-ContainerExists -Name $ContainerName
    if ($exists) {
        # A published port cannot be added to or removed from an existing
        # container, so recreate it when the request no longer matches. The
        # named volumes carry the durable state across the recreation.
        $published = Get-PublishedSshPort -Name $ContainerName
        if (($null -ne $published) -ne $PublishSsh.IsPresent) {
            Write-Host "==> Recreate $ContainerName so the published SSH port matches the request"
            $exitCode = Invoke-PodmanStep -Step "Remove $ContainerName" -PodmanArguments @('rm', '--force', $ContainerName)
            if ($exitCode -ne 0) {
                throw "Removing $ContainerName failed with podman exit code $exitCode."
            }
            $exists = $false
        }
        elseif (Test-ContainerRunning -Name $ContainerName) {
            Write-Host "==> $ContainerName is already running"
            return 0
        }
    }

    Initialize-DevelopmentVolumes

    if ($exists) {
        $exitCode = Invoke-PodmanStep -Step "Start $ContainerName" -PodmanArguments @('start', $ContainerName)
    }
    else {
        $runArguments = @(
            'run', '--detach',
            '--name', $ContainerName,
            '--label', 'io.vibeshell.role=development',
            '--label', "io.vibeshell.checkout=$CheckoutPrefix",
            '--workdir', $WorkspacePath,
            '--volume', "$($RepositoryRoot):$WorkspacePath",
            '--volume', "$($ModuleCacheVolume):/cache/gomod",
            '--volume', "$($BuildCacheVolume):/cache/gobuild",
            '--volume', "$($StateVolume):$StatePath",
            '--volume', "$($HomeVolume):$HomePath",
            '--volume', "$($LocalBinVolume):$LocalBinPath"
        )
        if ($PublishSsh) {
            # An empty host port asks podman for a free one, and binding to the
            # loopback keeps it off every network the host is attached to.
            $runArguments += @('--publish', "127.0.0.1::$($SSHContainerPort)")
        }
        # The image is the last argument of podman run; anything after it is the
        # container command.
        $runArguments += $tag

        $exitCode = Invoke-PodmanStep -Step "Start $ContainerName from $tag" -PodmanArguments $runArguments
    }
    if ($exitCode -ne 0) {
        throw "Starting $ContainerName failed with podman exit code $exitCode."
    }

    Write-Host "Container: $ContainerName"
    Write-Host "Checkout:  $RepositoryRoot"
    $port = Get-PublishedSshPort -Name $ContainerName
    if ($null -ne $port) {
        Write-Host "SSH:       ssh -p $port <user>@127.0.0.1"
    }
    else {
        Write-Host "SSH:       not published; run scripts/dev.ps1 start -PublishSsh to publish it"
    }
    return 0
}

function Invoke-ContainerStop {
    if (-not (Test-ContainerExists -Name $ContainerName)) {
        Write-Host "==> $ContainerName does not exist"
        return 0
    }

    # Removing the container keeps the named volumes, so durable state survives
    # a stop and start cycle.
    $exitCode = Invoke-PodmanStep -Step "Stop and remove $ContainerName" -PodmanArguments @('rm', '--force', $ContainerName)
    if ($exitCode -ne 0) {
        throw "Stopping $ContainerName failed with podman exit code $exitCode."
    }
    Write-Host "Retained volumes: $ModuleCacheVolume, $BuildCacheVolume, $StateVolume, $HomeVolume, $LocalBinVolume"
    Write-Host "Delete them with: scripts/dev.ps1 clean -RemoveVolumes"
    return 0
}

function Invoke-ContainerClean {
    $null = Invoke-ContainerStop
    if ($RemoveVolumes) {
        foreach ($volume in @($ModuleCacheVolume, $BuildCacheVolume, $StateVolume, $HomeVolume, $LocalBinVolume)) {
            $exitCode = Invoke-PodmanStep -Step "Remove volume $volume" -PodmanArguments @('volume', 'rm', '--force', $volume)
            if ($exitCode -ne 0) {
                throw "Removing volume $volume failed with podman exit code $exitCode."
            }
        }
    }
    return 0
}

function Invoke-ContainerExec {
    Assert-ContainerRunning
    if (-not $Command -or $Command.Count -eq 0) {
        throw 'exec needs -Command with the argument vector to run inside the container.'
    }

    # podman exec forwards the exit code of the command it runs, and this
    # script forwards that code unchanged to its caller.
    $exitCode = Invoke-PodmanStep -Step "Exec in ${ContainerName}: $($Command -join ' ')" -PodmanArguments (@('exec', '--workdir', $WorkspacePath, $ContainerName) + $Command)
    return $exitCode
}

function Invoke-GoSmoke {
    Assert-ContainerRunning
    $steps = @(
        @{ Label = 'go version'; Arguments = @('go', 'version') },
        @{ Label = 'go build ./...'; Arguments = @('go', 'build', './...') },
        @{ Label = 'go test ./...'; Arguments = @('go', 'test', './...') }
    )
    foreach ($step in $steps) {
        $exitCode = Invoke-ContainerStep -Step $step.Label -CommandArguments $step.Arguments
        if ($exitCode -ne 0) {
            throw "$($step.Label) failed with podman exit code $exitCode."
        }
    }
    return 0
}

function Invoke-GoRaceTest {
    Assert-ContainerRunning
    $exitCode = Invoke-ContainerStep -Step 'go test -race ./...' -CommandArguments @('go', 'test', '-race', './...')
    if ($exitCode -ne 0) {
        throw "go test -race ./... failed with podman exit code $exitCode."
    }
    return 0
}

function Invoke-ApplicationBuild {
    Assert-ContainerRunning

    # The binary lands in the checkout's ignored out/ directory so that it
    # survives a container replacement and can be inspected on the host.
    New-LocalDirectory -Path (Join-Path $RepositoryRoot 'out')
    $revision = Get-HeadRevision
    $linkerFlags = "-s -w -X j0s.at/vibeshell/internal/buildinfo.revision=$revision"

    $exitCode = Invoke-ContainerStep -Step "Build out/vibeshell ($revision)" -CommandArguments @(
        'go', 'build', '-trimpath', '-ldflags', $linkerFlags, '-o', "$WorkspacePath/out/vibeshell", './cmd/vibeshell'
    )
    if ($exitCode -ne 0) {
        throw "go build failed with podman exit code $exitCode."
    }
    Write-Host "Binary: $(Join-Path $RepositoryRoot 'out/vibeshell')"
    return 0
}

function Get-HeadRevision {
    <#
    .SYNOPSIS
        Returns the short HEAD revision, marked dirty for a modified tree.
    #>
    $revision = & git -C $RepositoryRoot rev-parse --short=12 HEAD 2>$null
    if ($LASTEXITCODE -ne 0) { return 'unknown' }
    $revision = $revision.Trim()

    $status = & git -C $RepositoryRoot status --porcelain 2>$null
    if ($LASTEXITCODE -eq 0 -and $status) {
        return "$revision-dirty"
    }
    return $revision
}

function Get-ContainerStatus {
    # The JSON shape is the contract callers parse: container name and
    # published SSH port, or null when SSH is not published.
    $exists = Test-ContainerExists -Name $ContainerName
    return [pscustomobject]@{
        container = $ContainerName
        ssh       = if ($exists) { Get-PublishedSshPort -Name $ContainerName } else { $null }
    }
}

# ---------------------------------------------------------------------------
# Dispatch
# ---------------------------------------------------------------------------

$RepositoryRoot = Split-Path $PSScriptRoot -Parent
$CheckoutPrefix = Get-CheckoutPrefix -CheckoutPath $RepositoryRoot
$ContainerName = "$CheckoutPrefix-dev"
$ModuleCacheVolume = "$CheckoutPrefix-gomod"
$BuildCacheVolume = "$CheckoutPrefix-gobuild"
$StateVolume = "$CheckoutPrefix-state"
$HomeVolume = "$CheckoutPrefix-home"
$LocalBinVolume = "$CheckoutPrefix-localbin"
$script:ActionExitCode = 0

switch ($Action) {
    'image' { $script:ActionExitCode = Invoke-ImageBuild }
    'start' { $script:ActionExitCode = Invoke-ContainerStart }
    'stop' { $script:ActionExitCode = Invoke-ContainerStop }
    'exec' { $script:ActionExitCode = Invoke-ContainerExec }
    'smoke' { $script:ActionExitCode = Invoke-GoSmoke }
    'test' { $script:ActionExitCode = Invoke-GoRaceTest }
    'build' { $script:ActionExitCode = Invoke-ApplicationBuild }
    'clean' { $script:ActionExitCode = Invoke-ContainerClean }
    'status' {
        $status = Get-ContainerStatus
        if ($AsJson) {
            # The only output on the pipeline, so callers can parse it directly.
            Write-Output (ConvertTo-Json -InputObject $status -Compress)
        }
        else {
            $running = (Test-ContainerExists -Name $ContainerName) -and (Test-ContainerRunning -Name $ContainerName)
            Write-Host "Container: $($status.container) ($($running ? 'running' : 'not running'))"
            Write-Host "Checkout:  $RepositoryRoot"
            Write-Host "SSH:       $(if ($null -ne $status.ssh) { "127.0.0.1:$($status.ssh)" } else { 'not published' })"
            Write-Host "Volumes:   $ModuleCacheVolume, $BuildCacheVolume, $StateVolume, $HomeVolume, $LocalBinVolume"
        }
    }
    default { throw "Unknown action '$Action'." }
}

exit $script:ActionExitCode
