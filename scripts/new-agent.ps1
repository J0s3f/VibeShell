#Requires -Version 7.2

<#
.SYNOPSIS
    Creates an isolated Git worktree and branch for one implementation subagent.

.DESCRIPTION
    Every implementation subagent develops in its own checkout so that
    concurrent agents never share files, a container, build caches, durable
    state, or a published port. Run this from the integration checkout; the
    worktree starts from the current HEAD on a new agent/<name> branch.

    scripts/dev.ps1 derives its resource prefix from the checkout path, so a
    worktree at .worktrees/<name> automatically owns the container
    vibeshell-<name>-dev and the volumes vibeshell-<name>-*.

.EXAMPLE
    scripts/new-agent.ps1 -Name quickjs-bridge
    Creates .worktrees/quickjs-bridge on branch agent/quickjs-bridge.
#>
[CmdletBinding()]
param(
    [Parameter(Mandatory)]
    [ValidatePattern('^[a-z0-9][a-z0-9._-]{0,39}$')]
    [string]$Name
)

Set-StrictMode -Version Latest

# See the same note in scripts/dev.ps1: callers run with
# $ErrorActionPreference = Stop and git writes progress to stderr.
$ErrorActionPreference = 'Continue'
$PSNativeCommandErrorActionPreference = $false

$repositoryRoot = Split-Path $PSScriptRoot -Parent
$worktreeRoot = Join-Path $repositoryRoot '.worktrees'
$worktreePath = Join-Path $worktreeRoot $Name
$branchName = "agent/$Name"

if (Test-Path -LiteralPath $worktreePath) {
    throw "Checkout $worktreePath already exists. Pick another name, or remove it with 'git worktree remove $worktreePath' first."
}

& git -C $repositoryRoot rev-parse --verify --quiet "refs/heads/$branchName" *> $null
if ($LASTEXITCODE -eq 0) {
    throw "Branch $branchName already exists. Pick another name, or delete the branch first."
}

New-Item -ItemType Directory -Force -Path $worktreeRoot | Out-Null

& git -C $repositoryRoot worktree add -b $branchName $worktreePath HEAD
$exitCode = $LASTEXITCODE
if ($exitCode -ne 0) {
    throw "git worktree add failed with exit code $exitCode."
}

Write-Host "Checkout: $worktreePath"
Write-Host "Branch:   $branchName"
Write-Host "Resources: vibeshell-$Name-dev plus the vibeshell-$Name-* volumes and its own SSH port"
Write-Host "Next: make sure the worktree has the committed scripts/ and containers/ directories, then run scripts/dev.ps1 start in it."
exit 0
