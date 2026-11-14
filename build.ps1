$ErrorActionPreference = 'Stop'
Push-Location $PSScriptRoot
try {
    go test ./...
    if ($LASTEXITCODE -ne 0) { throw 'Tests failed' }
    go vet ./...
    if ($LASTEXITCODE -ne 0) { throw 'Static checks failed' }
    go build -trimpath -ldflags '-H windowsgui -s -w' -o GoUninstaller.exe .
    if ($LASTEXITCODE -ne 0) { throw 'Build failed' }
    Write-Output 'Built GoUninstaller.exe'
} finally {
    Pop-Location
}
