#Requires -Version 7.2

<#
.SYNOPSIS
    Runs the SSH and terminal qualification spike with a real OpenSSH client.

.DESCRIPTION
    Everything happens in a container built from experiments/ssh-terminal/
    Containerfile: the Go spike server, the real ssh(1) client, and the probe
    programs. The host only runs Podman and this script; nothing is installed on
    the host.

    The script builds that image, starts one container, and inside it:

      1. runs go test -race ./... so the receipt covers the Go tests too,
      2. starts the spike server in public mode and drives it with ssh(1):
         public authentication with no prompt, pty negotiation, a resize
         delivered as window-change, a Ctrl-C byte, end of input, a refused exec,
         a refused subsystem (ssh -s), and refused forwarding,
      3. restarts the server in password mode and checks a correct password, a
         wrong password, and a client that offers no password at all,
      4. runs the terminal-primitive probe and the transport probe,
      5. collects the server's own log for each mode.

    Every command's output is appended to experiments/ssh-terminal/receipts/,
    and the script exits non-zero if any check fails.

.PARAMETER KeepContainer
    Leaves the spike container running after the checks.

.EXAMPLE
    pwsh -File experiments/ssh-terminal/run-spike.ps1
#>
[CmdletBinding()]
param(
    [switch]$KeepContainer
)

Set-StrictMode -Version Latest
# Native failures stay non-terminating so each step reports its own result.
$ErrorActionPreference = 'Continue'
$PSNativeCommandErrorActionPreference = $false

$SpikeRoot = $PSScriptRoot
$RepositoryRoot = Split-Path (Split-Path $SpikeRoot -Parent) -Parent
$ReceiptDirectory = Join-Path $SpikeRoot 'receipts'
$ServerPort = 2222
$ServerAddress = "127.0.0.1:$ServerPort"
$ServerLog = '/tmp/spike-server.log'
$PublicUser = 'ada'
$PasswordUser = 'grace'
# The password is a spike fixture written into the container at run time, never
# a real credential and never a command-line argument on the host.
$Password = 'open sesame'
$PasswordFile = '/tmp/spike-password'
$WrongPasswordFile = '/tmp/spike-wrong-password'

$script:Failures = [System.Collections.Generic.List[string]]::new()

# Resource names are derived from the checkout exactly as scripts/dev.ps1 does,
# so this spike never collides with the development container or another agent.
$CheckoutPath = [System.IO.Path]::GetFullPath($RepositoryRoot).TrimEnd([System.IO.Path]::DirectorySeparatorChar)
$Identity = 'main'
if ((Split-Path (Split-Path $CheckoutPath -Parent) -Leaf) -eq '.worktrees') {
    $Identity = Split-Path $CheckoutPath -Leaf
}
$Prefix = "vibeshell-$($Identity.ToLowerInvariant() -replace '[^a-z0-9._-]', '-')"
$ContainerName = "$Prefix-sshspike"
$ImageTag = "localhost/$Prefix-sshspike:local"

function Write-Heading {
    param([Parameter(Mandatory)][string]$Text)
    Write-Host "==> $Text"
}

function Add-Receipt {
    param([Parameter(Mandatory)][AllowEmptyString()][string[]]$Lines)
    foreach ($line in $Lines) { Add-Content -LiteralPath $ReceiptPath -Value $line }
}

function Get-PinnedGoImage {
    <#
    .SYNOPSIS
        Reads GO_IMAGE from the repository's pin file, so the spike image uses
        the same base image as the development container.
    #>
    foreach ($line in Get-Content -LiteralPath (Join-Path $RepositoryRoot 'containers/versions.env')) {
        if ($line.Trim() -match '^GO_IMAGE=(.+)$') { return $Matches[1] }
    }
    throw 'GO_IMAGE is missing from containers/versions.env'
}

function Invoke-Spike {
    <#
    .SYNOPSIS
        Runs a command inside the spike container and returns its output lines.
    #>
    param([Parameter(Mandatory)][string[]]$Command)
    return @(& podman exec --workdir /workspace $ContainerName @Command 2>&1)
}

function Add-Check {
    <#
    .SYNOPSIS
        Records one check's outcome in the receipt and on the console.
    #>
    param(
        [Parameter(Mandatory)][string]$Name,
        [Parameter(Mandatory)][bool]$Passed,
        [Parameter(Mandatory)][string]$Detail
    )
    $line = if ($Passed) { "PASS  $Name -- $Detail" } else { "FAIL  $Name -- $Detail" }
    Add-Receipt @($line)
    Write-Host "  $line"
    if (-not $Passed) { $script:Failures.Add($Name) }
}

function Invoke-ExpectOutput {
    <#
    .SYNOPSIS
        Runs a shell command inside the container and asserts that its combined
        output contains every expected string. The command is expected to report
        its own client status in its output, because ssh's status is the
        interesting part rather than the exit code of the wrapping shell.
    #>
    param(
        [Parameter(Mandatory)][string]$Name,
        [Parameter(Mandatory)][string]$Command,
        [Parameter(Mandatory)][string[]]$Expect
    )
    Add-Receipt @('', "`$ $Command")
    $output = @(& podman exec --workdir /workspace $ContainerName sh -c $Command 2>&1)
    $exitCode = $LASTEXITCODE
    Add-Receipt @($output | ForEach-Object { "  $_" })
    Add-Receipt @("  [podman exit $exitCode]")

    $joined = $output -join "`n"
    $missing = @($Expect | Where-Object { -not $joined.Contains($_) })
    $passed = ($missing.Count -eq 0)
    Add-Check -Name $Name -Passed $passed `
        -Detail $(if ($missing.Count -gt 0) { "missing: $($missing -join ' | ')" } else { 'output matched' })
}

function Invoke-ExpectServerLog {
    <#
    .SYNOPSIS
        Asserts that the spike server's own log records something. A refusal
        reason only reaches a real client on its error stream for some request
        types, so the log is the authoritative record of what was refused.
    #>
    param(
        [Parameter(Mandatory)][string]$Name,
        [Parameter(Mandatory)][string[]]$Expect
    )
    $log = (Invoke-Spike -Command @('cat', $ServerLog)) -join "`n"
    $missing = @($Expect | Where-Object { -not $log.Contains($_) })
    $passed = ($missing.Count -eq 0)
    Add-Check -Name $Name -Passed $passed `
        -Detail $(if ($missing.Count -gt 0) { "server log missing: $($missing -join ' | ')" } else { 'logged' })
}

function Start-SpikeServer {
    <#
    .SYNOPSIS
        Starts the spike server inside the container and waits for its log line.
    #>
    param([Parameter(Mandatory)][string[]]$ServerArguments)
    Invoke-Spike -Command @('sh', '-c', "rm -f $ServerLog") | Out-Null
    # Every argument is single-quoted: a password fixture with a space in it
    # would otherwise be split by the container shell into two arguments, and the
    # server would then accept only the first word.
    $quoted = ($ServerArguments | ForEach-Object { "'$_'" }) -join ' '
    $command = "/out/spike-server $quoted >> $ServerLog 2>&1"
    & podman exec --detach $ContainerName sh -c $command | Out-Null
    for ($attempt = 0; $attempt -lt 150; $attempt++) {
        $probe = (Invoke-Spike -Command @('sh', '-c', "grep -c listening $ServerLog 2>/dev/null || true")) -join ''
        if ($probe.Trim() -match '^[1-9]') {
            Write-Host "    server ready on $ServerAddress"
            return
        }
        Start-Sleep -Milliseconds 200
    }
    throw 'The spike server did not report that it is listening.'
}

function Stop-SpikeServer {
    Invoke-Spike -Command @('sh', '-c', 'pkill -f /out/spike-server') | Out-Null
    Start-Sleep -Milliseconds 400
}

function Add-ServerLog {
    param([Parameter(Mandatory)][string]$Label)
    Add-Receipt @('', "--- spike server log: $Label ---")
    Add-Receipt @(Invoke-Spike -Command @('cat', $ServerLog) | ForEach-Object { "  $_" })
}

# ---------------------------------------------------------------------------
# Build and start
# ---------------------------------------------------------------------------

New-Item -ItemType Directory -Force -Path $ReceiptDirectory | Out-Null
$ReceiptPath = Join-Path $ReceiptDirectory 'real-openssh.txt'
$GoImage = Get-PinnedGoImage

Write-Heading "Build $ImageTag"
Write-Host "    base image $GoImage"
$buildOutput = @(& podman build --file (Join-Path $SpikeRoot 'Containerfile') --build-arg "GO_IMAGE=$GoImage" --tag $ImageTag $SpikeRoot 2>&1)
foreach ($line in $buildOutput) { Write-Host "    $line" }
if ($LASTEXITCODE -ne 0) { throw "Spike image build failed with podman exit code $LASTEXITCODE." }

& podman rm --force $ContainerName 2>&1 | Out-Null
Write-Heading "Start $ContainerName"
& podman run --detach --name $ContainerName --label 'io.vibeshell.role=spike' --workdir /workspace $ImageTag | Out-Null
if ($LASTEXITCODE -ne 0) { throw "Starting $ContainerName failed with podman exit code $LASTEXITCODE." }

Set-Content -LiteralPath $ReceiptPath -Value 'vibeshell SSH and terminal spike receipts'
Add-Receipt @(
    "generated: $([DateTime]::UtcNow.ToString('yyyy-MM-ddTHH:mm:ssZ'))",
    "image:    $ImageTag",
    "base:     $GoImage",
    "container: $ContainerName",
    "client:   $((Invoke-Spike -Command @('ssh', '-V')) -join ' ')",
    "python:   $((Invoke-Spike -Command @('python3', '--version')) -join ' ')"
)

# ---------------------------------------------------------------------------
# 1. Go tests inside the spike image
# ---------------------------------------------------------------------------

Write-Heading 'go test -race ./...'
$testOutput = @(& podman exec --workdir /workspace $ContainerName sh -c 'go test -race -count=1 -timeout 300s ./... 2>&1' 2>&1)
$testExit = $LASTEXITCODE
foreach ($line in $testOutput) { Write-Host "    $line" }
$testOutput | Set-Content -LiteralPath (Join-Path $ReceiptDirectory 'go-test-race.txt')
Add-Check -Name 'go test -race' -Passed ($testExit -eq 0) -Detail "exit $testExit (receipt go-test-race.txt)"

# ---------------------------------------------------------------------------
# 2. Public mode with a real ssh client
# ---------------------------------------------------------------------------

Write-Heading 'Public mode: SSH none authentication, real ssh client'
Start-SpikeServer -ServerArguments @('-addr', $ServerAddress, '-host-key', '/tmp/spike_host_key', '-mode', 'public')

$sshBase = "ssh -p $ServerPort -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR"

# BatchMode=yes disables password prompts entirely, so success proves the server
# never needed one.
Invoke-ExpectOutput -Name 'public auth: no password prompt, exit status 3' `
    -Command "printf 'exit 3\r' | ssh -tt -p $ServerPort -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o BatchMode=yes -o LogLevel=ERROR $PublicUser@127.0.0.1; echo `"client exit `$?`"" `
    -Expect @('welcome user=ada', 'exit 3', 'client exit 3')

Invoke-ExpectOutput -Name 'public auth: a client with no pty gets the documented default size' `
    -Command "printf 'exit 0\r' | ssh -T -p $ServerPort -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o BatchMode=yes -o LogLevel=ERROR $PublicUser@127.0.0.1; echo `"client exit `$?`"" `
    -Expect @('terminal= size=80x24 pty=false', 'client exit 0')

# The spike session only reports end-of-input when the input did not already ask
# to exit, so this check types a line without a command and then ends the stream.
Invoke-ExpectOutput -Name 'public auth: end of input is an SSH EOF, exit status 0' `
    -Command "printf 'ls\r' | ssh -tt -p $ServerPort -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o BatchMode=yes -o LogLevel=ERROR $PublicUser@127.0.0.1; echo `"client exit `$?`"" `
    -Expect @('unknown-command ls', 'eof err=<nil>', 'client exit 0')

Invoke-ExpectOutput -Name 'exec is refused' `
    -Command "$sshBase $PublicUser@127.0.0.1 ls -la; echo `"client exit `$?`"" `
    -Expect @('exec request failed on channel 0', 'client exit 255')

# ssh -s sends a subsystem request and reports the client's own failure text; the
# reason the server gave is on its error stream, which ssh -s does not print, so
# the server log carries the assertion.
Invoke-ExpectOutput -Name 'a requested subsystem (ssh -s) is refused' `
    -Command "timeout 20 ssh -s -p $ServerPort -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR $PublicUser@127.0.0.1 sftp; echo `"client exit `$?`"" `
    -Expect @('subsystem request failed on channel 0', 'client exit 255')

Invoke-ExpectServerLog -Name 'the server refuses exec and subsystem with a reason' `
    -Expect @(
        'type=exec reason="exec is not supported: vibeshell serves interactive shell sessions only"',
        'type=subsystem reason="subsystem is not supported: vibeshell serves interactive shell sessions only"'
    )

# `ssh -A` only sends auth-agent-req when the socket exists, so the helper above
# stands in for a running agent.
Invoke-Spike -Command @('sh', '-c', 'rm -f /tmp/fake-agent.sock') | Out-Null
# Refusing agent forwarding must not end the session: a real client continues
# without the capability, and the exec it sends next is refused on its own terms.
# `ssh -A` sends auth-agent-req only when SSH_AUTH_SOCK connects, and a real
# agent answers on that socket. The helper stands in for one: it accepts a
# connection and replies that it holds no identities, which is all ssh needs to
# offer agent forwarding.
Invoke-ExpectOutput -Name 'agent forwarding is refused but the session continues' `
    -Command "timeout 40 python3 /workspace/agentsocket.py ssh -A -p $ServerPort -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR $PublicUser@127.0.0.1 ls < /dev/null" `
    -Expect @('agent forwarding is not supported', 'exec request failed on channel 0')
Invoke-ExpectServerLog -Name 'the server refuses agent forwarding before the exec' `
    -Expect @('type=auth-agent-req@openssh.com reason="ssh-agent forwarding is not supported"')

# A local forward is refused when a client actually uses it: ssh -L only opens a
# local listener, so the direct-tcpip channel arrives on first use.
Invoke-ExpectOutput -Name 'a used local forward is refused at the direct-tcpip channel' `
    -Command "timeout 40 python3 /workspace/forwardcheck.py --port $ServerPort --user $PublicUser" `
    -Expect @('[forward] the forward was closed without data')
Invoke-ExpectServerLog -Name 'the server refuses the direct-tcpip channel' `
    -Expect @('type=direct-tcpip reason="vibeshell opens interactive session channels only')

# The pty driver gives the client a real terminal, so the pty request, the
# resize, and the Ctrl-C byte are all produced by ssh(1) itself. The driver
# types an arrow key one byte at a time, resizes the pty, sends 0x03, and then
# types "exit 0" so the session ends with a status the client reports.
Invoke-ExpectOutput -Name 'pty resize, fragmented key, and Ctrl-C over a real pty' `
    -Command "timeout 90 python3 /workspace/ptyclient.py --port $ServerPort --user $PublicUser --initial 24x80 --resize-to 40x132 --fragment `"`$(printf '\033[A')`" --send-hex 03 --send-text 'exit 0\r' --server-log $ServerLog" `
    -Expect @('welcome user=ada terminal=xterm-256color size=80x24 pty=true',
        'key up',
        'key ctrl+c',
        'interrupt ctrl-c',
        'resize 132x40',
        'exit 0',
        'ssh exited with status 0')
Invoke-ExpectServerLog -Name 'the server records the window-change a real client sent' `
    -Expect @('msg="window change" user=ada terminal="xterm-256color 132x40"')

Add-ServerLog -Label 'public mode'

# ---------------------------------------------------------------------------
# 3. Password mode with the real ssh client
# ---------------------------------------------------------------------------

Write-Heading 'Password mode: only password authentication is advertised'
Stop-SpikeServer
Start-SpikeServer -ServerArguments @(
    '-addr', $ServerAddress, '-host-key', '/tmp/spike_host_key',
    '-mode', 'password', '-user', $PasswordUser, '-password', $Password
)

# The askpass helper reads the password from a file in the container, so the
# receipt never contains it and ssh never sees it on a command line. ssh execs
# SSH_ASKPASS directly, and the helper arrives from a Windows checkout without
# its executable bit, so a shell wrapper provides it.
Invoke-Spike -Command @('sh', '-c',
    "printf '%s' '$Password' > $PasswordFile; printf '%s' 'wrong-password' > $WrongPasswordFile; chmod 0600 $PasswordFile $WrongPasswordFile") | Out-Null
Invoke-Spike -Command @('sh', '-c',
    "printf '#!/bin/sh`nexec python3 /workspace/askpass.py`n' > /tmp/askpass.sh; chmod 0700 /tmp/askpass.sh") | Out-Null
# The helper must hand back exactly the stored password: a helper that fails
# would make the acceptance check below pass for the wrong reason.
Invoke-ExpectOutput -Name 'the askpass helper supplies the stored password' `
    -Command "test `"`$(ASKPASS_FILE=$PasswordFile /tmp/askpass.sh)`" = '$Password' && echo askpass-ok" `
    -Expect @('askpass-ok')
$askpassEnvironment = "SSH_ASKPASS=/tmp/askpass.sh SSH_ASKPASS_REQUIRE=force ASKPASS_FILE=$PasswordFile"
$wrongAskpassEnvironment = "SSH_ASKPASS=/tmp/askpass.sh SSH_ASKPASS_REQUIRE=force ASKPASS_FILE=$WrongPasswordFile"

Invoke-ExpectOutput -Name 'password mode: correct password is accepted' `
    -Command "printf 'exit 0\r' | $askpassEnvironment ssh -tt -p $ServerPort -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -o PreferredAuthentications=password $PasswordUser@127.0.0.1; echo `"client exit `$?`"" `
    -Expect @("welcome user=$PasswordUser", 'client exit 0')

Invoke-ExpectOutput -Name 'password mode: wrong password is refused' `
    -Command "$wrongAskpassEnvironment ssh -tt -p $ServerPort -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -o NumberOfPasswordPrompts=1 -o PreferredAuthentications=password $PasswordUser@127.0.0.1; echo `"client exit `$?`"" `
    -Expect @('Permission denied', 'client exit 255')

Invoke-ExpectOutput -Name 'password mode: a client with no password is refused' `
    -Command "ssh -T -p $ServerPort -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o BatchMode=yes -o LogLevel=ERROR $PasswordUser@127.0.0.1 </dev/null; echo `"client exit `$?`"" `
    -Expect @('Permission denied', 'client exit 255')

Add-ServerLog -Label 'password mode'
Stop-SpikeServer

# ---------------------------------------------------------------------------
# 4. Probes
# ---------------------------------------------------------------------------

Write-Heading 'Terminal primitive and transport probes'
$probeOutput = @(& podman exec --workdir /workspace $ContainerName sh -c '/out/terminal-primitives 2>&1; echo; /out/transport-probe 2>&1' 2>&1)
$probeExit = $LASTEXITCODE
foreach ($line in $probeOutput) { Write-Host "    $line" }
$probeOutput | Set-Content -LiteralPath (Join-Path $ReceiptDirectory 'probes.txt')
Add-Check -Name 'terminal and transport probes' -Passed ($probeExit -eq 0) -Detail "exit $probeExit (receipt probes.txt)"

# ---------------------------------------------------------------------------
# Result
# ---------------------------------------------------------------------------

Add-Receipt @('')
if ($script:Failures.Count -eq 0) {
    Add-Receipt @('result: all checks passed')
    Write-Heading 'All checks passed'
}
else {
    Add-Receipt @("result: failed checks: $($script:Failures -join ', ')")
    Write-Heading "Failed checks: $($script:Failures -join ', ')"
}

if (-not $KeepContainer) {
    & podman rm --force $ContainerName 2>&1 | Out-Null
    Write-Host "Removed $ContainerName"
}

exit $(if ($script:Failures.Count -eq 0) { 0 } else { 1 })
