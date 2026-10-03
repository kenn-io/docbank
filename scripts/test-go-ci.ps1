param(
    [ValidateSet('all', '1', '2')]
    [string]$Shard = 'all'
)

$ErrorActionPreference = 'Stop'

# Shard 1 runs the slowest packages and shard 2 everything else, so a new package lands in shard 2.
$slowest = @('.', './internal/store', './cmd/docbank')
$packages = @('./...')
if ($Shard -eq '1') {
    $packages = $slowest
} elseif ($Shard -eq '2') {
    $skip = @(go list -tags fts5 @slowest)
    if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
    $packages = @(go list -tags fts5 ./... | Where-Object { $_ -notin $skip })
    if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
}

# Git Bash enables backup privileges for its children, which lets reads bypass exclusive file locks.
go test -timeout 30m -tags fts5 @args @packages
exit $LASTEXITCODE
