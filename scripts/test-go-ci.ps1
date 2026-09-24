$ErrorActionPreference = 'Stop'

# Git Bash enables backup privileges for its children, which lets reads bypass exclusive file locks.
go test -timeout 30m -tags fts5 @args ./...
exit $LASTEXITCODE
