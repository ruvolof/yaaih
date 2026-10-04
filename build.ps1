# Windows build helper for yaaih.
#
# Points every Go cache at .go/ inside the project so the developer's global
# Go installation is left untouched. Usage:
#
#   .\build.ps1 tidy     # download dependencies into ./.go/pkg/mod
#   .\build.ps1 vendor   # tidy + go mod vendor
#   .\build.ps1 build    # build the binary using vendored deps
#   .\build.ps1 image    # build the runtime Docker image from the repo root Dockerfile
#   .\build.ps1 test     # vet + run the test suite under the race detector
#   .\build.ps1 cover    # test with coverage; per-package table plus an HTML report
#   .\build.ps1 run      # build binary, build runtime image if missing, and run the harness
#   .\build.ps1 update-playwright # upgrade playwright-go, then report the matching Dockerfile base image
#   .\build.ps1 clean    # remove vendor/, .go/, and the binary
#
# -Pkg narrows test and cover to one package, e.g.
#
#   .\build.ps1 cover -Pkg ./internal/executor/

param(
    [Parameter(Position = 0)]
    [ValidateSet('tidy', 'vendor', 'build', 'image', 'test', 'cover', 'run', 'update-playwright', 'clean')]
    [string]$Target = 'build',

    [string]$Pkg = './...'
)

$ErrorActionPreference = 'Stop'
$root = $PSScriptRoot
$image = 'yaaih-runtime:latest'

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

# The race detector needs cgo, which on Windows means a gcc on PATH. Without one
# the tests still run, but several of them — the turn loop, the executor's bash
# session, the governor — are only meaningful under -race, so say so rather than
# reporting a quieter pass.
function Get-RaceArgs {
    if (Get-Command gcc -ErrorAction SilentlyContinue) { return @('-race') }
    Write-Warning 'no gcc on PATH: running without the race detector. Install mingw-w64 to match what build.sh runs.'
    return @()
}

switch ($Target) {
    'tidy'   { & $go mod tidy }
    'vendor' { & $go mod tidy; & $go mod vendor }
    'build'  {
        if (-not (Test-Path (Join-Path $root 'vendor'))) {
            & $go mod tidy
            & $go mod vendor
        }
        & $go build -mod=vendor -o yaaih.exe .
    }
    'image'  { Build-RuntimeImage }
    'test'   {
        $race = @(Get-RaceArgs)
        & $go vet -mod=vendor $Pkg
        if ($LASTEXITCODE -ne 0) { throw 'go vet failed' }
        & $go test -mod=vendor @race $Pkg
        if ($LASTEXITCODE -ne 0) { throw 'go test failed' }
    }
    'cover'  {
        $race = @(Get-RaceArgs)
        $coverDir = Join-Path $root '.go/cover'
        $profilePath = Join-Path $coverDir 'coverage.out'
        $htmlPath = Join-Path $coverDir 'coverage.html'
        New-Item -ItemType Directory -Force -Path $coverDir | Out-Null

        # A native-command argument of the form -key=$var is passed through
        # literally unless the whole token is a quoted string.
        & $go test -mod=vendor @race -covermode=atomic "-coverprofile=$profilePath" -cover $Pkg
        if ($LASTEXITCODE -ne 0) { throw 'go test failed' }
        & $go tool cover "-html=$profilePath" -o $htmlPath

        $report = & $go tool cover "-func=$profilePath"
        $below = $report |
            Where-Object { $_ -notmatch '^total:' } |
            ForEach-Object {
                $fields = $_ -split '\s+'
                [pscustomobject]@{
                    Pct   = [double]($fields[-1] -replace '%$', '')
                    Func  = $fields[1]
                    Where = $fields[0]
                }
            } |
            Where-Object { $_.Pct -lt 100 } |
            Sort-Object Pct

        Write-Host ''
        Write-Host "total: $((($report | Select-Object -Last 1) -split '\s+')[-1])"
        Write-Host ''
        Write-Host "least-covered functions ($($below.Count) below 100%, showing 40):"
        $below | Select-Object -First 40 | ForEach-Object {
            Write-Host ('{0,7:N1}%  {1,-28} {2}' -f $_.Pct, $_.Func, $_.Where)
        }
        Write-Host ''
        Write-Host "full per-function report: go tool cover -func=$profilePath"
        Write-Host "browsable line-by-line:   $htmlPath"
    }
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
        & $go build -mod=vendor -o yaaih.exe .
        & (Join-Path $root 'yaaih.exe') --agent (Join-Path $root 'agents/chatter')
    }
    'clean'  {
        Remove-Item -Recurse -Force -ErrorAction SilentlyContinue `
            (Join-Path $root 'vendor'),
            (Join-Path $root '.go'),
            (Join-Path $root 'yaaih.exe')
    }
}
