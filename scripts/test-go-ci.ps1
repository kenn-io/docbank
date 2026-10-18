param(
    [ValidateSet('all', '1', '2', '3', '4')]
    [string]$Shard = 'all',
    [ValidateSet(2, 4)]
    [int]$ShardCount = 2
)

$ErrorActionPreference = 'Stop'

# Remove once the main-pinned caller passes -ShardCount 4; until then it needs these groups.
$legacy = @('./docbank', './internal/store', './cmd/docbank')
# Seed measured hot packages; distribute the remainder round-robin as a best-effort balance.
$groups = @{
    1 = @($legacy[1])
    2 = @($legacy[0], './internal/api')
    3 = @($legacy[2], './internal/daemonconn')
    4 = @('./internal/processing')
}
$packages = @('./...')
if ($Shard -ne 'all' -and [int]$Shard -gt $ShardCount) {
    throw "Shard $Shard exceeds shard count $ShardCount"
}
if ($ShardCount -eq 4 -and $Shard -ne 'all') {
    $packages = @($groups[[int]$Shard])
    if ($Shard -ne '1') {
        $seeded = @($groups.Values | ForEach-Object { $_ })
        $skip = @(go list -tags fts5 @seeded)
        if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
        $remaining = @(go list -tags fts5 ./... | Where-Object { $_ -notin $skip })
        if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
        for ($i = [int]$Shard - 2; $i -lt $remaining.Count; $i += 3) {
            $packages += $remaining[$i]
        }
    }
} elseif ($Shard -eq '1') {
    $packages = $legacy
} elseif ($Shard -ne 'all') {
    $skip = @(go list -tags fts5 @legacy)
    if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
    $packages = @(go list -tags fts5 ./... | Where-Object { $_ -notin $skip })
    if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
}

# Git Bash enables backup privileges for its children, which lets reads bypass exclusive file locks.
if ($ShardCount -eq 4 -and $Shard -eq '3') {
    # Lifecycle tests spawn daemons with readiness deadlines; give them the runner first.
    go test -timeout 30m -tags fts5 @args ./cmd/docbank
    if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
    $packages = @($packages | Where-Object { $_ -ne './cmd/docbank' })
}
go test -timeout 30m -tags fts5 @args @packages
exit $LASTEXITCODE
