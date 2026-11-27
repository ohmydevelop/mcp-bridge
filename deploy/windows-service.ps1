[CmdletBinding()]
param([ValidateSet('install', 'status', 'refresh-python', 'serve', 'uninstall')][string]$Action = 'status')
$ErrorActionPreference = 'Stop'
# Prefer the modules shipped with this PowerShell when launched from pwsh or an IDE.
$env:PSModulePath = "$PSHOME\Modules;$env:PSModulePath"
$identity = [Security.Principal.WindowsIdentity]::GetCurrent()
$sid = $identity.User
$state = Join-Path $env:USERPROFILE '.mcp-bridge'
$app = Join-Path $env:LOCALAPPDATA 'mcp-bridge'
$binary = Join-Path $app 'mcp-bridge.exe'
$service = Join-Path $app 'windows-service.ps1'
$task = "MCP Bridge ($($sid.Value))"
$keyFile = Join-Path $state 'master.key'
$tokenFile = Join-Path $state 'http.token'
$configFile = Join-Path $app 'environment.json'
$pythonHint = Join-Path $app 'python-path'

function New-PrivateDirectory($path) {
    [IO.Directory]::CreateDirectory($path) | Out-Null
    $acl = Get-Acl -LiteralPath $path
    if ($acl.GetOwner([Security.Principal.SecurityIdentifier]) -ne $sid) {
        # An administrator token may default to the Administrators group as owner.
        $acl.SetOwner($sid)
    }
    $acl.SetAccessRuleProtection($true, $false)
    foreach ($rule in @($acl.Access)) { $acl.RemoveAccessRuleSpecific($rule) }
    $rule = New-Object Security.AccessControl.FileSystemAccessRule(
        $sid, 'FullControl', 'ContainerInherit,ObjectInherit', 'None', 'Allow')
    $acl.AddAccessRule($rule)
    Set-Acl -LiteralPath $path -AclObject $acl
}

function Stop-BridgeTask($existing) {
    if ($existing.State -ne 'Running') { return }
    Stop-ScheduledTask -InputObject $existing
    for ($i = 0; $i -lt 50; $i++) {
        if ((Get-ScheduledTask -TaskName $task).State -ne 'Running') { return }
        Start-Sleep -Milliseconds 200
    }
    throw "Scheduled task did not stop: $task"
}

function New-Secret {
    $bytes = New-Object byte[] 32
    $generator = [Security.Cryptography.RandomNumberGenerator]::Create()
    try { $generator.GetBytes($bytes) } finally { $generator.Dispose() }
    return [BitConverter]::ToString($bytes).Replace('-', '').ToLowerInvariant()
}

function Load-Environment {
    $config = [IO.File]::ReadAllText($configFile) | ConvertFrom-Json
    $env:BRIDGE_MASTER_KEY = [IO.File]::ReadAllText($keyFile).Trim()
    $token = [IO.File]::ReadAllText($tokenFile).Trim()
    $env:BRIDGE_TOKENS = @{agent = $token} | ConvertTo-Json -Compress
    $env:PATH = $config.path
    $env:BRIDGE_PYTHON_HINT_FILE = $pythonHint
}

function Update-PythonHint {
    $oldHintFile = $env:BRIDGE_PYTHON_HINT_FILE
    try {
        Remove-Item Env:BRIDGE_PYTHON_HINT_FILE -ErrorAction SilentlyContinue
        $python = & $binary python-path
        if ($LASTEXITCODE -ne 0) { return $false }
    } finally {
        if ($null -ne $oldHintFile) { $env:BRIDGE_PYTHON_HINT_FILE = $oldHintFile }
    }
    $temp = Join-Path $app ([Guid]::NewGuid().ToString() + '.tmp')
    try {
        [IO.File]::WriteAllText($temp, [string]$python)
        if (-not [IO.File]::Exists($pythonHint)) {
            try { [IO.File]::Move($temp, $pythonHint) }
            catch [IO.IOException] { if (-not [IO.File]::Exists($pythonHint)) { throw } }
        }
        if ([IO.File]::Exists($temp)) { [IO.File]::Replace($temp, $pythonHint, [NullString]::Value) }
    } finally {
        if ([IO.File]::Exists($temp)) { [IO.File]::Delete($temp) }
    }
    Write-Host "Handler Python: $python"
    return $true
}

switch ($Action) {
    'refresh-python' {
        if (-not (Update-PythonHint)) { exit 1 }
    }
    install {
        New-PrivateDirectory $state
        New-PrivateDirectory $app
        if (-not [IO.File]::Exists($keyFile)) {
            if ($env:BRIDGE_MASTER_KEY) {
                if ($env:BRIDGE_MASTER_KEY -notmatch '^[0-9a-fA-F]{64}$') { throw 'Invalid BRIDGE_MASTER_KEY' }
                [IO.File]::WriteAllText($keyFile, $env:BRIDGE_MASTER_KEY)
            } elseif ([IO.File]::Exists((Join-Path $state 'bridge.sqlite'))) {
                throw 'Existing database: set BRIDGE_MASTER_KEY to its original key before installing.'
            } else {
                [IO.File]::WriteAllText($keyFile, (New-Secret))
            }
        }
        if (-not [IO.File]::Exists($tokenFile)) { [IO.File]::WriteAllText($tokenFile, (New-Secret)) }
        $existing = Get-ScheduledTask -TaskName $task -ErrorAction SilentlyContinue
        if ($existing) { Stop-BridgeTask $existing }
        if ($PSScriptRoot -ne $app) {
            Copy-Item -LiteralPath (Join-Path $PSScriptRoot 'mcp-bridge.exe') -Destination $binary -Force
            Copy-Item -LiteralPath $PSCommandPath -Destination $service -Force
        }
        if (-not (Update-PythonHint)) { Write-Warning 'Run refresh-python after installing Python.' }
        $config = @{path = $env:PATH} | ConvertTo-Json
        [IO.File]::WriteAllText($configFile, $config)
        $engine = Join-Path $env:SystemRoot 'System32\WindowsPowerShell\v1.0\powershell.exe'
        $arguments = "-NoLogo -NoProfile -NonInteractive -WindowStyle Hidden -ExecutionPolicy RemoteSigned -File `"$service`" serve"
        $launch = New-ScheduledTaskAction -Execute $engine -Argument $arguments
        $trigger = New-ScheduledTaskTrigger -AtLogOn -User $identity.Name
        $principal = New-ScheduledTaskPrincipal -UserId $identity.Name -LogonType Interactive -RunLevel Limited
        $settings = New-ScheduledTaskSettingsSet -ExecutionTimeLimit ([TimeSpan]::Zero) -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -RestartCount 3 -RestartInterval (New-TimeSpan -Minutes 1)
        Register-ScheduledTask -TaskName $task -Action $launch -Trigger $trigger -Principal $principal -Settings $settings -Force | Out-Null
        Start-ScheduledTask -TaskName $task
        Write-Output "Installed $binary. Run & '$service' status."
    }
    serve {
        Load-Environment
        & $binary serve -data $state -listen 127.0.0.1:8088
        exit $LASTEXITCODE
    }
    status {
        $job = Get-ScheduledTask -TaskName $task
        Write-Output "Scheduled task: $($job.State)"
        & $binary doctor -data $state
        exit $LASTEXITCODE
    }
    uninstall {
        $existing = Get-ScheduledTask -TaskName $task -ErrorAction SilentlyContinue
        if ($existing) {
            Stop-BridgeTask $existing
            Unregister-ScheduledTask -TaskName $task -Confirm:$false
        }
        foreach ($path in @($binary, $configFile, $pythonHint, $service)) {
            if (Test-Path -LiteralPath $path) { Remove-Item -LiteralPath $path -Force }
        }
        Write-Output "Service removed. Data and keys retained in $state"
    }
}
