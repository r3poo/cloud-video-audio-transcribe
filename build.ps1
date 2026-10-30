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

    # A build without the manifest produces an exe that cannot open a window
    # ("TTM_ADDTOOL failed"), so fail here instead of on the user's machine.
    $ascii = [Text.Encoding]::ASCII.GetString([IO.File]::ReadAllBytes($full))
    if ($ascii -notlike '*Microsoft.Windows.Common-Controls*') {
        throw @"
        $Output has no application manifest, so it cannot show a window.
        app_windows_amd64.syso is missing from the project root. Regenerate it:
            go install github.com/akavel/rsrc@latest
            rsrc -manifest app.manifest -arch amd64 -o app_windows_amd64.syso
        "@
    }

    $size = [math]::Round((Get-Item $full).Length / 1MB, 1)
    Write-Host "Built $full ($size MB)"
    Write-Host "Remember to place a .env with OPENROUTER_API_KEY next to it."
} finally {
    Pop-Location
}
