param(
    [switch]$Minimal
)

$env:GOOS="linux"
$env:GOARCH="amd64"
$env:CGO_ENABLED="0"

if ($Minimal) {
    go build -tags minimal -ldflags "-s -w" -o build/xraytool-minimal
    exit $LASTEXITCODE
}

go build -ldflags "-s -w" -o build/xraytool
