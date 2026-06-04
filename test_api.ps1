$ErrorActionPreference = "Stop"

Write-Host "Starting Xraytool API server..."
$serverJob = Start-Job {
    Set-Location C:\Dev\SERVER\xraytool-go
    .\xraytool.exe --config tests/config.yaml start-server
}

Start-Sleep -Seconds 3

$Headers = @{
    "X-API-Key" = "megasupersecretkey"
    "Content-Type" = "application/json"
}

# Test newuser
Write-Host "Testing newuser API..."
$newuserPayload = @{ email = "api_test_user"; limit = 5 } | ConvertTo-Json
$res = Invoke-RestMethod -Uri "http://127.0.0.1:8090/api/rest/xraytool/newuser" -Method Post -Headers $Headers -Body $newuserPayload
Write-Host "Newuser response: " $res.status " -> " $res.output

# Test setlimit
Write-Host "Testing setlimit API..."
$setlimitPayload = @{ email = "api_test_user"; limit = 10 } | ConvertTo-Json
$res = Invoke-RestMethod -Uri "http://127.0.0.1:8090/api/rest/xraytool/setlimit" -Method Post -Headers $Headers -Body $setlimitPayload
Write-Host "Setlimit response: " $res.status " -> " $res.output

# Test setexpire
Write-Host "Testing setexpire API..."
$setexpirePayload = @{ email = "api_test_user"; expire = "01-01-2030" } | ConvertTo-Json
$res = Invoke-RestMethod -Uri "http://127.0.0.1:8090/api/rest/xraytool/setexpire" -Method Post -Headers $Headers -Body $setexpirePayload
Write-Host "Setexpire response: " $res.status " -> " $res.output

# Stop server
Stop-Job $serverJob
Remove-Job $serverJob

Write-Host "All tests finished!"
