param(
	[string]$Version = "2.0.3",
    [string]$OutputDir = "",
    [string]$ZipName = ""
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

$Version = $Version.Trim().TrimStart('v')
if ($Version -notmatch '^\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?$') {
    throw "Invalid release version: $Version"
}
$releaseTag = 'v' + $Version

$repoRoot = Split-Path -Parent $PSScriptRoot
if ([string]::IsNullOrWhiteSpace($OutputDir)) {
    $OutputDir = Join-Path $repoRoot ('releases\bknetwork-' + $releaseTag)
}
if ([string]::IsNullOrWhiteSpace($ZipName)) {
    $ZipName = 'BKNetwork-' + $releaseTag + '-windows-x64.zip'
}

$webSource = Join-Path $repoRoot 'web'
$webTarget = Join-Path $OutputDir 'web'
$exeTarget = Join-Path $OutputDir 'bknetwork.exe'
$zipDir = Join-Path $repoRoot 'releases'
$zipTarget = Join-Path $repoRoot ('releases\' + $ZipName)
$resourceDir = Join-Path $repoRoot 'cmd\bknetwork'
$resourceSource = Join-Path $resourceDir 'bknetwork.rc'
$resourceTarget = Join-Path $resourceDir 'rsrc_windows_amd64.syso'

if (Test-Path $OutputDir) {
    Remove-Item $OutputDir -Recurse -Force
}
New-Item -ItemType Directory -Path $OutputDir | Out-Null
if (-not (Test-Path $zipDir)) {
    New-Item -ItemType Directory -Path $zipDir | Out-Null
}

Copy-Item -Path $webSource -Destination $webTarget -Recurse -Force

$windresCommand = Get-Command windres.exe -ErrorAction SilentlyContinue
if ($null -ne $windresCommand) {
    Push-Location $resourceDir
    try {
        & $windresCommand.Source -i (Split-Path -Leaf $resourceSource) -J rc -O coff -F pe-x86-64 -o (Split-Path -Leaf $resourceTarget)
        if ($LASTEXITCODE -ne 0 -or -not (Test-Path $resourceTarget)) {
            throw "windres failed with exit code $LASTEXITCODE"
        }
    } finally {
        Pop-Location
    }
} elseif (-not (Test-Path $resourceTarget)) {
    throw "windres.exe is unavailable and $resourceTarget does not exist"
} else {
    Write-Warning "windres.exe is unavailable; using the existing Windows resource file"
}

Push-Location $repoRoot
try {
    go build -buildvcs=false -o $exeTarget ./cmd/bknetwork
    if ($LASTEXITCODE -ne 0 -or -not (Test-Path $exeTarget)) {
        throw "go build failed with exit code $LASTEXITCODE"
    }
} finally {
    Pop-Location
}

if (Test-Path $zipTarget) {
    Remove-Item $zipTarget -Force
}

Compress-Archive -Path (Join-Path $OutputDir '*') -DestinationPath $zipTarget -Force

Write-Host "Release staged at $OutputDir"
Write-Host "Release archive created at $zipTarget"
