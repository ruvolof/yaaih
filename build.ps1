# Windows build helper for aibitat-harness-go.
#
# Points every Go cache at .go/ inside the project so the developer's global
# Go installation is left untouched. Usage:
#
#   .\build.ps1 tidy     # download dependencies into ./.go/pkg/mod
#   .\build.ps1 vendor   # tidy + go mod vendor
#   .\build.ps1 build    # build the binary using vendored deps
#   .\build.ps1 image    # build the runtime Docker image from the repo root Dockerfile
#   .\build.ps1 run      # build binary, build runtime image if missing, and run the harness
#   .\build.ps1 update-playwright # upgrade playwright-go, then report the matching Dockerfile base image
#   .\build.ps1 clean    # remove vendor/, .go/, and the binary

param(
    [Parameter(Position = 0)]
    [ValidateSet('tidy', 'vendor', 'build', 'image', 'run', 'update-playwright', 'clean')]
    [string]$Target = 'build'
)

$ErrorActionPreference = 'Stop'
$root = $PSScriptRoot
$image = 'aibitat-harness-runtime:latest'

$env:GOPATH     = Join-Path $root '.go'
$env:GOMODCACHE = Join-Path $root '.go/pkg/mod'
$env:GOCACHE    = Join-Path $root '.go/cache'
$env:GOBIN      = Join-Path $root '.go/bin'

$go = 'C:\Program Files\Go\bin\go.exe'
if (-not (Test-Path $go)) { $go = (Get-Command go).Source }

function Test-RuntimeImage {
    & docker image inspect $image *> $null
    return ($LASTEXITCODE -eq 0)
}

function Build-RuntimeImage {
    & docker build -t $image -f (Join-Path $root 'Dockerfile') $root
    if ($LASTEXITCODE -ne 0) { throw "docker build failed for $image" }
}

switch ($Target) {
    'tidy'   { & $go mod tidy }
    'vendor' { & $go mod tidy; & $go mod vendor }
    'build'  {
        if (-not (Test-Path (Join-Path $root 'vendor'))) {
            & $go mod tidy
            & $go mod vendor
        }
        & $go build -mod=vendor -o aibitat-harness-go.exe .
    }
    'image'  { Build-RuntimeImage }
    'update-playwright' {
        & $go get -u github.com/mxschmitt/playwright-go
        & $go mod tidy
        & $go mod vendor
        $runGo = Join-Path $root 'vendor/github.com/mxschmitt/playwright-go/run.go'
        $driverVersion = (Select-String -Path $runGo -Pattern 'playwrightCliVersion\s*=\s*"([^"]+)"' |
            Select-Object -First 1).Matches.Groups[1].Value
        Write-Host ''
        if ($driverVersion) {
            Write-Host "playwright-go now targets Playwright v$driverVersion."
            Write-Host "Set the Dockerfile base image to mcr.microsoft.com/playwright:v$driverVersion-noble, then run: .\build.ps1 image"
        } else {
            Write-Host 'playwright-go updated, but its target Playwright version could not be read.'
            Write-Host "Check playwrightCliVersion in $runGo, match the Dockerfile base image, then run: .\build.ps1 image"
        }
    }
    'run'    {
        if (-not (Test-Path (Join-Path $root 'vendor'))) {
            & $go mod tidy
            & $go mod vendor
        }
        if (-not (Test-RuntimeImage)) {
            Write-Host "Building runtime image $image from $(Join-Path $root 'Dockerfile')..."
            Build-RuntimeImage
        }
        & $go build -mod=vendor -o aibitat-harness-go.exe .
        & (Join-Path $root 'aibitat-harness-go.exe') --agent (Join-Path $root 'agents/chatter')
    }
    'clean'  {
        Remove-Item -Recurse -Force -ErrorAction SilentlyContinue `
            (Join-Path $root 'vendor'),
            (Join-Path $root '.go'),
            (Join-Path $root 'aibitat-harness-go.exe')
    }
}
