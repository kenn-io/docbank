# Install the latest docbank release on 64-bit Windows.
# Usage: irm https://raw.githubusercontent.com/kenn-io/docbank/main/scripts/install.ps1 | iex

$ErrorActionPreference = 'Stop'

$repo = 'kenn-io/docbank'
$binaryName = 'docbank.exe'

function Write-Info([string]$Message) { Write-Host $Message -ForegroundColor Green }
function Write-WarningMessage([string]$Message) { Write-Host $Message -ForegroundColor Yellow }

function Test-EnvBool([string]$Name) {
    $value = [Environment]::GetEnvironmentVariable($Name)
    return $value -match '^(1|true|yes)$'
}

function Get-Architecture {
    try {
        $architecture = [System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture.ToString()
        switch ($architecture) {
            'X64' { return 'amd64' }
            'Arm64' { return 'arm64' }
            default { throw "unsupported Windows architecture: $architecture" }
        }
    } catch {
        $architecture = if ($env:PROCESSOR_ARCHITEW6432) {
            $env:PROCESSOR_ARCHITEW6432
        } else {
            $env:PROCESSOR_ARCHITECTURE
        }
        switch ($architecture) {
            'AMD64' { return 'amd64' }
            'ARM64' { return 'arm64' }
            default { throw "unsupported Windows architecture: $architecture" }
        }
    }
}

function Invoke-WebRequestCompat {
    param([Parameter(Mandatory)][string]$Uri, [string]$OutFile, [string]$Method = 'Get')

    if ($PSVersionTable.PSVersion.Major -lt 6) {
        [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
    }
    $parameters = @{ Uri = $Uri; Method = $Method; ErrorAction = 'Stop' }
    if ($OutFile) { $parameters.OutFile = $OutFile }
    if ($PSVersionTable.PSVersion.Major -lt 6) { $parameters.UseBasicParsing = $true }
    return Invoke-WebRequest @parameters
}

function Get-FinalUrl($Response) {
    if ($Response.BaseResponse.ResponseUri) {
        return $Response.BaseResponse.ResponseUri.AbsoluteUri
    }
    if ($Response.BaseResponse.RequestMessage.RequestUri) {
        return $Response.BaseResponse.RequestMessage.RequestUri.AbsoluteUri
    }
    return $null
}

function Get-LatestVersion {
    if ($env:DOCBANK_VERSION) { return $env:DOCBANK_VERSION }

    $latestUrl = "https://github.com/$repo/releases/latest"
    $response = Invoke-WebRequestCompat -Uri $latestUrl -Method Head
    $finalUrl = Get-FinalUrl $response
    if (-not $finalUrl -or $finalUrl -notmatch '/releases/tag/([^/]+)/?$') {
        throw "could not resolve the latest release tag from $latestUrl"
    }
    return $Matches[1]
}

function Get-InstallDirectory {
    if ($env:DOCBANK_INSTALL_DIR) { return $env:DOCBANK_INSTALL_DIR }
    if ($env:LOCALAPPDATA) { return Join-Path $env:LOCALAPPDATA 'Programs\docbank\bin' }
    return Join-Path $env:USERPROFILE '.local\bin'
}

function Add-ToUserPath([string]$Directory) {
    $currentPath = [Environment]::GetEnvironmentVariable('Path', 'User')
    $normalized = $Directory.TrimEnd('\', '/')
    $present = $currentPath -split ';' | Where-Object {
        $_.TrimEnd('\', '/') -ieq $normalized
    }
    if ($present) { return $false }

    $newPath = if ($currentPath) { "$currentPath;$Directory" } else { $Directory }
    [Environment]::SetEnvironmentVariable('Path', $newPath, 'User')
    $env:Path = "$env:Path;$Directory"
    return $true
}

function Get-ExpectedChecksum([string]$Path, [string]$ArchiveName) {
    $checksumMatches = @()
    foreach ($line in Get-Content -LiteralPath $Path) {
        if ($line -match '^\s*$') { continue }
        $parts = $line -split '\s+', 2
        if ($parts.Count -ne 2) { continue }
        $name = $parts[1] -replace '^\*', '' -replace '^\.\\', '' -replace '^\./', ''
        if ($name -eq $ArchiveName) { $checksumMatches += $parts[0] }
    }
    if ($checksumMatches.Count -ne 1) {
        throw "SHA256SUMS must contain exactly one entry for $ArchiveName"
    }
    if ($checksumMatches[0] -notmatch '^[0-9a-fA-F]{64}$') {
        throw "invalid SHA-256 value for $ArchiveName"
    }
    return $checksumMatches[0].ToLowerInvariant()
}

function Assert-ZipLayout([string]$Path) {
    Add-Type -AssemblyName System.IO.Compression.FileSystem
    $zip = [System.IO.Compression.ZipFile]::OpenRead($Path)
    try {
        if ($zip.Entries.Count -ne 1 -or $zip.Entries[0].FullName -ne $binaryName) {
            throw "release archive must contain only $binaryName at its root"
        }
    } finally {
        $zip.Dispose()
    }
}

function Install-Docbank {
    if ($PSVersionTable.PSVersion.Major -lt 5) {
        throw 'PowerShell 5.0 or later is required'
    }

    $architecture = Get-Architecture
    $version = Get-LatestVersion
    if ($version -notmatch '^v[0-9]+\.[0-9]+\.[0-9]+$') {
        throw "release tag is not vX.Y.Z: $version"
    }
    $versionNumber = $version.Substring(1)
    $archiveName = "docbank_${versionNumber}_windows_${architecture}.zip"
    $baseUrl = if ($env:DOCBANK_RELEASE_BASE_URL) {
        $env:DOCBANK_RELEASE_BASE_URL.TrimEnd('/')
    } else {
        "https://github.com/$repo/releases/download/$version"
    }
    $installDirectory = Get-InstallDirectory
    New-Item -ItemType Directory -Path $installDirectory -Force | Out-Null

    $temporaryDirectory = Join-Path ([IO.Path]::GetTempPath()) "docbank-install-$([guid]::NewGuid())"
    New-Item -ItemType Directory -Path $temporaryDirectory | Out-Null
    $staged = $null
    try {
        $archive = Join-Path $temporaryDirectory $archiveName
        $checksums = Join-Path $temporaryDirectory 'SHA256SUMS'
        Write-Info "Installing docbank $version for windows/$architecture..."
        Invoke-WebRequestCompat -Uri "$baseUrl/$archiveName" -OutFile $archive | Out-Null
        Invoke-WebRequestCompat -Uri "$baseUrl/SHA256SUMS" -OutFile $checksums | Out-Null

        $expected = Get-ExpectedChecksum -Path $checksums -ArchiveName $archiveName
        $actual = (Get-FileHash -LiteralPath $archive -Algorithm SHA256).Hash.ToLowerInvariant()
        if ($actual -ne $expected) {
            throw "checksum mismatch for $archiveName (expected $expected, got $actual)"
        }
        Write-Info 'Checksum verified.'

        Assert-ZipLayout -Path $archive
        Expand-Archive -LiteralPath $archive -DestinationPath $temporaryDirectory
        $source = Join-Path $temporaryDirectory $binaryName
        if (-not (Test-Path -LiteralPath $source -PathType Leaf)) {
            throw "release archive does not contain $binaryName"
        }

        $destination = Join-Path $installDirectory $binaryName
        $staged = Join-Path $installDirectory ".docbank-install-$([guid]::NewGuid()).exe"
        Copy-Item -LiteralPath $source -Destination $staged
        try {
            Move-Item -LiteralPath $staged -Destination $destination -Force
        } catch {
            throw "could not replace $destination; stop any running Docbank daemon and retry: $_"
        }
        $staged = $null

        Write-Info "Installed $destination"
        if (-not (Test-EnvBool 'DOCBANK_NO_MODIFY_PATH')) {
            if (Add-ToUserPath $installDirectory) {
                Write-WarningMessage "Added $installDirectory to PATH; restart your terminal to use it."
            }
        }
        Write-Info 'Get started: docbank add ~/Documents --dest /archive'
    } finally {
        if ($staged -and (Test-Path -LiteralPath $staged)) {
            Remove-Item -LiteralPath $staged -Force -ErrorAction SilentlyContinue
        }
        Remove-Item -LiteralPath $temporaryDirectory -Recurse -Force -ErrorAction SilentlyContinue
    }
}

Install-Docbank
