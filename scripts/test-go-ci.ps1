param(
    [ValidateSet('all', '1', '2', '3', '4')]
    [string]$Shard = 'all',
    [ValidateSet(4)]
    [int]$ShardCount = 4
)

$ErrorActionPreference = 'Stop'

# Seed measured hot packages; distribute the remainder round-robin as a best-effort balance.
$groups = @{
    1 = @('./internal/store')
    2 = @('./docbank', './internal/api')
    3 = @('./cmd/docbank')
    4 = @('./internal/processing', './internal/daemonconn', './internal/backupapp')
}
$packages = @('./...')
if ($Shard -ne 'all') {
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
}

# Git Bash enables backup privileges for its children, which lets reads bypass exclusive file locks.
if ($Shard -eq '3') {
    # Lifecycle tests spawn daemons with readiness deadlines; give them the runner first.
    go test -timeout 30m -tags fts5 @args ./cmd/docbank
    if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
    $packages = @($packages | Where-Object { $_ -ne './cmd/docbank' })
}
go test -timeout 30m -tags fts5 @args @packages
exit $LASTEXITCODE
