# Builds cloud-transcribe.exe without a console window.
# Usage: .\build.ps1 [-Output .\cloud-transcribe.exe]
param(
    [string]$Output = "cloud-transcribe.exe"
)

$ErrorActionPreference = "Stop"

Push-Location $PSScriptRoot
try {
    go build -trimpath -ldflags "-H windowsgui -s -w" -o $Output .
    if ($LASTEXITCODE -ne 0) { throw "go build failed" }

    $full = (Resolve-Path $Output).Path
    $size = [math]::Round((Get-Item $full).Length / 1MB, 1)
    Write-Host "Built $full ($size MB)"
    Write-Host "Remember to place a .env with OPENROUTER_API_KEY next to it."
} finally {
    Pop-Location
}
