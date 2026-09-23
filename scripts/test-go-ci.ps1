$ErrorActionPreference = 'Stop'

# Git Bash enables backup privileges for its children, which lets reads bypass exclusive file locks.
$extra = $args
$packages = go list -tags fts5 ./... | Where-Object { $_ -ne 'go.kenn.io/docbank/internal/store' }
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }

# Storage dominates CI. Split it without adding runners or changing tests.
# ponytail: two name ranges balance the current suite; remeasure if it grows unevenly.
$storage = @('^(Test|Example|Fuzz)[A-L]', '^(Test|Example|Fuzz)($|[^A-L])') | ForEach-Object {
    $process = Start-Process go -NoNewWindow -PassThru -ArgumentList (
        @('test', '-timeout', '30m', '-tags', 'fts5') + $extra + @('-run', $_, './internal/store')
    )
    # Cache the handle so ExitCode stays readable after the process exits.
    $null = $process.Handle
    $process
}

$status = 0
go test -timeout 30m -tags fts5 @extra @packages
if ($LASTEXITCODE -ne 0) { $status = $LASTEXITCODE }
foreach ($process in $storage) {
    $process.WaitForExit()
    if ($process.ExitCode -ne 0) { $status = $process.ExitCode }
}
exit $status
