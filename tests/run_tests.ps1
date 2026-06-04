# ==============================================================================
# xraytool Integration Tests Runner
# Runs CLI conversion and subscription endpoints on compiled binary
# ==============================================================================

$ErrorActionPreference = "Stop"

Write-Host "=== 1. Building xraytool.exe ===" -ForegroundColor Cyan
go build -o build/xraytool.exe .
if ($LASTEXITCODE -ne 0) {
    Write-Error "Failed to build xraytool binary"
}
Write-Host "[OK] Binary compiled successfully." -ForegroundColor Green

Write-Host "`n=== 2. Testing CLI: JSON to VLESS via Stdin ===" -ForegroundColor Cyan
$jsonInput = '{"outbounds":[{"protocol":"vless","settings":{"vnext":[{"address":"1.2.3.4","port":443,"users":[{"id":"e93fca7e-3cf1-4545-8c01-7fa918b95888","encryption":"none","flow":"xtls-rprx-vision"}]}]},"streamSettings":{"network":"tcp","security":"reality","realitySettings":{"sni":"yahoo.com","fingerprint":"chrome","publicKey":"test_public_key_value","shortId":"01234567"}},"tag":"my-reality-node"}]}'
$vlessOutput = $jsonInput | ./build/xraytool.exe convert --config tests/config.yaml -

Write-Host "Output VLESS Link:" -ForegroundColor Gray
Write-Host $vlessOutput -ForegroundColor Yellow

if ($vlessOutput -like "*vless://e93fca7e-3cf1-4545-8c01-7fa918b95888@1.2.3.4:443*") {
    Write-Host "[OK] JSON to VLESS conversion passed." -ForegroundColor Green
} else {
    Write-Error "JSON to VLESS conversion failed! Output does not contain expected VLESS link format."
}

Write-Host "`n=== 2b. Testing CLI: JSON Array to VLESS via Stdin ===" -ForegroundColor Cyan
$jsonArrayInput = '[{"protocol":"vless","settings":{"vnext":[{"address":"4.3.2.1","port":443,"users":[{"id":"e93fca7e-3cf1-4545-8c01-7fa918b95888","encryption":"none","flow":"xtls-rprx-vision"}]}]},"streamSettings":{"network":"tcp","security":"reality","realitySettings":{"sni":"yahoo.com","fingerprint":"chrome","publicKey":"test_public_key_value","shortId":"01234567"}},"tag":"my-reality-array-node"}]'
$vlessArrayOutput = $jsonArrayInput | ./build/xraytool.exe convert --config tests/config.yaml -

Write-Host "Output VLESS Link for Array:" -ForegroundColor Gray
Write-Host $vlessArrayOutput -ForegroundColor Yellow

if ($vlessArrayOutput -like "*vless://e93fca7e-3cf1-4545-8c01-7fa918b95888@4.3.2.1:443*") {
    Write-Host "[OK] JSON Array to VLESS conversion passed (auto-wrapped array)." -ForegroundColor Green
} else {
    Write-Error "JSON Array to VLESS conversion failed!"
}


Write-Host "`n=== 3. Testing CLI: VLESS to JSON via Stdin ===" -ForegroundColor Cyan
$vlessLink = "vless://e93fca7e-3cf1-4545-8c01-7fa918b95888@1.2.3.4:443?security=reality&sni=yahoo.com&fp=chrome&pbk=test_public_key_value&sid=01234567&type=tcp&flow=xtls-rprx-vision#my-reality-node"
$jsonOutput = $vlessLink | ./build/xraytool.exe convert --config tests/config.yaml -

Write-Host "Output JSON config:" -ForegroundColor Gray
Write-Host $jsonOutput -ForegroundColor Yellow

if ($jsonOutput -like '*"protocol":"vless"*' -and $jsonOutput -like '*"address":"1.2.3.4"*') {
    Write-Host "[OK] VLESS to JSON conversion passed." -ForegroundColor Green
} else {
    Write-Error "VLESS to JSON conversion failed! Output does not contain expected JSON keys."
}

Write-Host "`n=== 4. Testing Subscription: Standard JSON Subscription (format=json) ===" -ForegroundColor Cyan
$subReqJSON = '{"remote_addr":"127.0.0.1","user_agent":"megasupersecretua","query":{"id":"testclient"},"headers":{"x-request-path":"/client?id=testclient"}}'
$subJSONResult = $subReqJSON | ./build/xraytool.exe --config tests/config.yaml sub

Write-Host "Raw JSON Subscription Response:" -ForegroundColor Gray
Write-Host $subJSONResult -ForegroundColor Yellow

# Parse the Response Envelope
$envelope = ConvertFrom-Json $subJSONResult
if ($envelope.status_code -ne 200) {
    Write-Error "Subscription failed with status $($envelope.status_code): $($envelope.body)"
}

$renderedJSON = ConvertFrom-Json $envelope.body
if ($renderedJSON.outbounds[0].protocol -eq "vless" -and $renderedJSON.outbounds[0].settings.vnext[0].address -eq "1.2.3.4") {
    Write-Host "[OK] Standard subscription rendering passed (properly replaced {HOST}, {UUID}, etc)." -ForegroundColor Green
} else {
    Write-Error "Standard subscription validation failed!"
}

Write-Host "`n=== 5. Testing Subscription: Dynamic VLESS Subscription (format=vless) ===" -ForegroundColor Cyan
$subVlessReqJSON = '{"remote_addr":"127.0.0.1","user_agent":"megasupersecretua","query":{"id":"testclient","format":"vless"},"headers":{"x-request-path":"/client?id=testclient&format=vless"}}'
$subVlessResult = $subVlessReqJSON | ./build/xraytool.exe --config tests/config.yaml sub

Write-Host "Raw VLESS Subscription Response:" -ForegroundColor Gray
Write-Host $subVlessResult -ForegroundColor Yellow

$vlessEnvelope = ConvertFrom-Json $subVlessResult
if ($vlessEnvelope.status_code -ne 200) {
    Write-Error "VLESS subscription failed with status $($vlessEnvelope.status_code): $($vlessEnvelope.body)"
}

# The body should be VLESS share links, dynamically converted!
$links = $vlessEnvelope.body -split "`n"
$hasVless = $false
foreach ($link in $links) {
    if ($link -like "*vless://e93fca7e-3cf1-4545-8c01-7fa918b95888@1.2.3.4:443*") {
        $hasVless = $true
        break
    }
}

if ($hasVless) {
    Write-Host "[OK] Dynamic VLESS Subscription rendering passed." -ForegroundColor Green
} else {
    Write-Error "Dynamic VLESS Subscription rendering failed! Output links: $($vlessEnvelope.body)"
}

Write-Host "`n==============================================================================" -ForegroundColor Green
Write-Host " ALL TESTS PASSED SUCCESSFULLY! Ready to deploy to server." -ForegroundColor Green
Write-Host "==============================================================================" -ForegroundColor Green
