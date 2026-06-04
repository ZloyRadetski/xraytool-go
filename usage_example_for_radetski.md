Самое главное - настроить перед использованием AGY(Antigravity Cli)

$env:HTTP_PROXY="socks5://happ-socks:LSzPjA8S8uBu@127.0.0.1:10808"
$env:HTTPS_PROXY="socks5://happ-socks:LSzPjA8S8uBu@127.0.0.1:10808"
$env:NO_PROXY="localhost,127.0.0.1"
$env:CLOUD_CODE_URL="https://cloudcode-pa.googleapis.com"
$env:JETSKI_CLOUD_CODE_URL="https://cloudcode-pa.googleapis.com"
agy --conversation=f261c7fd-04cc-4432-a894-4c536bfe8fe2


BUILD
$env:GOOS="linux"; $env:GOARCH="amd64"; $env:CGO_ENABLED="0"; go build -ldflags "-s -w" -o build/xraytool

ДИАГНОСТИКА
su -s /bin/bash www-data -c 'echo "{\"remote_addr\":\"127.0.0.1\",\"user_agent\":\"incy\",\"query\":{\"id\":\"2D9x3P70iWqIRDcv\"},\"headers\":{}}" | xraytool sub'