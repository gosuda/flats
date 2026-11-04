# End-to-end check of install.ps1 against a locally built release archive: a
# refused checksum, fresh service install, a served flat, crash recovery, an
# ended task, upgrade restart, a stopped service on upgrade, uninstall and a
# client install with options.
#
#   FLATS_RELEASE_TARGETS=windows/amd64 scripts/build-release.sh v0.0.0-ci dist
#   pwsh scripts/install-smoke.ps1 dist
#
# It installs the real per-user service with default ports and paths, so run it
# only on a disposable machine such as a CI runner.

param([Parameter(Mandatory = $true)][string]$ReleaseDir)

$ErrorActionPreference = 'Stop'
$env:FLATS_DOWNLOAD_BASE = (Resolve-Path $ReleaseDir).Path
$root = Split-Path -Parent $PSScriptRoot
$flats = Join-Path $env:LOCALAPPDATA 'Programs\Flats\flats.exe'
$data = Join-Path $env:LOCALAPPDATA 'Flats'
$url = 'http://127.0.0.1:7878'

function Step([string]$Name) { Write-Host "`n== $Name" }
function Fail([string]$Message) { throw "install-smoke: FAIL: $Message" }
function Healthy {
	try {
		Invoke-WebRequest -UseBasicParsing -TimeoutSec 3 -Uri "$url/api/status" | Out-Null
		return $true
	} catch {
		return $false
	}
}
function Wait-Healthy([int]$Seconds) {
	for ($i = 0; $i -lt $Seconds; $i++) {
		if (Healthy) { return $i }
		Start-Sleep -Seconds 1
	}
	Fail "the service did not answer within $Seconds seconds"
}
function Wait-Down([int]$Seconds) {
	for ($i = 0; $i -lt $Seconds; $i++) {
		if (-not (Healthy)) { return $i }
		Start-Sleep -Seconds 1
	}
	Fail "the service still answers after $Seconds seconds"
}
function Get-ServePid {
	$words = (Get-Content -LiteralPath (Join-Path $data 'flats.lock') -Raw).Trim() -split '\s+'
	if ($words[1] -ne 'service') { Fail "the server did not record the role of the service: $words" }
	return [int]$words[0]
}
function Install([hashtable]$Options = @{}) {
	$installer = [scriptblock]::Create((Get-Content -LiteralPath (Join-Path $root 'install.ps1') -Raw))
	$out = & $installer @Options *>&1 | Out-String
	Write-Host $out
	return $out
}
# Get-UserPath reads the user's Path as it is stored, with %VAR% entries unexpanded.
function Get-UserPath {
	$key = [Microsoft.Win32.Registry]::CurrentUser.OpenSubKey('Environment')
	try {
		return $key.GetValue('Path', '', [Microsoft.Win32.RegistryValueOptions]::DoNotExpandEnvironmentNames)
	} finally {
		$key.Close()
	}
}
function Set-UserPath([string]$Value) {
	$key = [Microsoft.Win32.Registry]::CurrentUser.OpenSubKey('Environment', $true)
	try {
		$key.SetValue('Path', $Value, [Microsoft.Win32.RegistryValueKind]::ExpandString)
	} finally {
		$key.Close()
	}
}
function Test-TaskExists {
	& schtasks.exe /Query /TN Flats 2>&1 | Out-Null
	return $LASTEXITCODE -eq 0
}
# Approve-Pending decides the waiting approval as the console does. The host is
# the disposable one of this run.
function Approve-Pending {
	$list = Invoke-RestMethod -Uri "$url/api/approvals" -Headers @{ 'X-Flats-Client' = 'cli' }
	$id = ($list.approvals | Where-Object { $_.status -eq 'pending' } | Select-Object -First 1).id
	if (-not $id) { Fail 'no pending approval' }
	Invoke-RestMethod -Method Post -Uri "$url/console/api/approvals/$id/approve" -ContentType 'application/json' -Body '{}' -Headers @{
		'X-Flats-Console' = '1'
		'Sec-Fetch-Site'  = 'same-origin'
		'Origin'          = $url
	} | Out-Null
}
function Check-AgentsPointer([string]$Out) {
	if ($Out -notmatch 'flats mcp-config') { Fail 'installer did not print how to connect an agent' }
	if ($Out -notmatch 'flats agent-setup docs') { Fail 'installer did not print the optional agent-setup command' }
}

Step 'a wrong checksum changes nothing'
$pathBefore = Get-UserPath
$release = $env:FLATS_DOWNLOAD_BASE
$bad = Join-Path ([IO.Path]::GetTempPath()) 'flats-smoke-bad-release'
New-Item -ItemType Directory -Force $bad | Out-Null
Copy-Item -LiteralPath (Join-Path $release 'flats_windows_amd64.zip') -Destination $bad -Force
Set-Content -LiteralPath (Join-Path $bad 'checksums.txt') -Value "$('0' * 64) *flats_windows_amd64.zip"
$env:FLATS_DOWNLOAD_BASE = $bad
$refused = $false
try {
	Install | Out-Null
} catch {
	if ("$_" -notmatch 'checksum mismatch') { throw }
	$refused = $true
} finally {
	$env:FLATS_DOWNLOAD_BASE = $release
}
if (-not $refused) { Fail 'the installer accepted a wrong checksum' }
if (Test-Path $flats) { Fail 'a refused install left a binary' }
if (Test-TaskExists) { Fail 'a refused install left a task' }
if ((Get-UserPath) -cne $pathBefore) { Fail 'a refused install changed the Path' }

Step 'fresh install'
$entry = '%FLATS_SMOKE_VAR%\bin'
Set-UserPath ($pathBefore.TrimEnd(';') + ";$entry")
$out = Install
Check-AgentsPointer $out
$installedPath = Get-UserPath
if (($installedPath -split ';') -cnotcontains $entry) { Fail "the install changed a %VAR% entry of the Path: $installedPath" }
Set-UserPath (($installedPath -split ';' | Where-Object { $_ -cne $entry }) -join ';')
if (-not (Test-Path $flats)) { Fail 'no installed binary' }
Wait-Healthy 30 | Out-Null
$definition = Join-Path $env:LOCALAPPDATA 'Flats\service.xml'
if ((Get-Content -LiteralPath $definition -Raw -Encoding Unicode) -notmatch '--config') { Fail 'the service does not run from config.json' }
& $flats config validate
if ($LASTEXITCODE -ne 0) { Fail 'config.json does not validate' }
$userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
if ($userPath -notmatch [regex]::Escape((Split-Path -Parent $flats))) { Fail 'the install directory is not on the user Path' }

Step 'a server flat answers through a worker'
$site = Join-Path ([IO.Path]::GetTempPath()) 'flats-smoke-site'
New-Item -ItemType Directory -Force $site | Out-Null
Set-Content -LiteralPath (Join-Path $site 'flats.json') -Value '{"kind":"server"}'
Set-Content -LiteralPath (Join-Path $site 'server.js') -Value @'
export default {
  async fetch(request, env) {
    env.DB.exec("CREATE TABLE IF NOT EXISTS hits (n INTEGER)");
    env.DB.exec("INSERT INTO hits VALUES (1)");
    const [{ n }] = env.DB.query("SELECT count(*) AS n FROM hits");
    return Response.json({ hits: n });
  }
};
'@
& $flats save-draft smoke-site $site
if ($LASTEXITCODE -ne 0) { Fail 'save-draft failed' }
& $flats create-version smoke-site | Out-Null
Approve-Pending
& $flats deploy --flat smoke-site --version 1 | Out-Null
Approve-Pending
$answer = Invoke-WebRequest -UseBasicParsing -TimeoutSec 20 -Uri 'http://127.0.0.1:7879/' -Headers @{ Host = 'smoke-site.localhost:7879' }
if ($answer.Content -notmatch '"hits":\s*1') { Fail "the flat answered: $($answer.Content)" }

Step 'a second server on the data directory is refused'
$err = & $flats serve --data $data 2>&1 | Out-String
if ($err -notmatch 'already in use') { Fail "second server: $err" }

Step 'the service comes back after a crash'
$serve = Get-ServePid
Stop-Process -Id $serve -Force
Wait-Down 10 | Out-Null
$seconds = Wait-Healthy 120
Write-Host "back after ${seconds}s"

Step 'ending the task stops the server in order, and the service comes back'
$before = Get-ServePid
& schtasks.exe /End /TN Flats | Out-Null
Wait-Down 20 | Out-Null
$log = Join-Path $data 'logs\serve.log'
if ((Get-Content -LiteralPath $log -Raw) -notmatch 'shutting down') { Fail 'the server did not shut down in order' }
Wait-Healthy 120 | Out-Null
if ((Get-ServePid) -eq $before) { Fail 'the same server still runs' }

Step 'rerun restarts the service'
$before = Get-ServePid
$out = Install
Wait-Healthy 30 | Out-Null
if ((Get-ServePid) -eq $before) { Fail 'the service was not restarted' }

Step 'a stopped service stays stopped on upgrade'
& $flats uninstall
Install | Out-Null
Wait-Healthy 30 | Out-Null
& schtasks.exe /Change /TN Flats /DISABLE | Out-Null
& schtasks.exe /End /TN Flats | Out-Null
Wait-Down 20 | Out-Null
$out = Install
Check-AgentsPointer $out
if ($out -notmatch 'Start the service') { Fail 'installer did not print how to start the service' }
Start-Sleep -Seconds 3
if (Healthy) { Fail 'the installer started a service the user stopped' }

Step 'install starts it again'
& $flats install
if ($LASTEXITCODE -ne 0) { Fail 'install failed' }
Wait-Healthy 30 | Out-Null

Step 'an upgrade keeps a data directory with a space in its path'
& $flats uninstall
Wait-Down 20 | Out-Null
$custom = Join-Path ([IO.Path]::GetTempPath()) 'flats smoke data'
Remove-Item -Recurse -Force $custom -ErrorAction SilentlyContinue
$customUrl = 'http://127.0.0.1:27878'
$install = [scriptblock]::Create((Get-Content -LiteralPath (Join-Path $root 'install.ps1') -Raw))
$env:FLATS_DATA = $custom
$out = & $install -ServeArgs '--listen', '127.0.0.1:27878', '--local-addr', '127.0.0.1:27879' *>&1 | Out-String
Write-Host $out
Remove-Item Env:FLATS_DATA
if ($out -notmatch [regex]::Escape("Flats is running at $customUrl")) { Fail 'the installer did not wait for the --listen address' }
$url = $customUrl
Wait-Healthy 30 | Out-Null
$before = Get-Content -LiteralPath (Join-Path $custom 'flats.lock') -Raw
& $install *>&1 | Out-String | Write-Host
Wait-Healthy 30 | Out-Null
if ((Get-Content -LiteralPath (Join-Path $custom 'flats.lock') -Raw) -eq $before) { Fail 'the upgrade did not restart the service' }
$status = Invoke-RestMethod -Uri "$url/api/status"
if ($status.system.data_dir -ne $custom) { Fail "the upgrade moved the host to $($status.system.data_dir)" }

Step 'uninstall'
& $flats uninstall
if ($LASTEXITCODE -ne 0) { Fail 'uninstall failed' }
Wait-Down 20 | Out-Null
& schtasks.exe /Query /TN Flats 2>&1 | Out-Null
if ($LASTEXITCODE -eq 0) { Fail 'the task is still registered' }
if (-not (Test-Path (Join-Path $data 'flats.db'))) { Fail 'uninstall removed the data' }
if (-not (Test-Path (Join-Path $custom 'flats.db'))) { Fail 'uninstall removed the custom data' }

Step 'a client install with options'
$client = Join-Path ([IO.Path]::GetTempPath()) 'flats smoke client'
$pathBefore = Get-UserPath
$out = Install @{ Dir = $client; Version = 'v9.9.9'; NoService = $true; NoPath = $true }
if ($out -notmatch '\(v9\.9\.9\)') { Fail 'the installer did not take -Version' }
& (Join-Path $client 'flats.exe') version
if ($LASTEXITCODE -ne 0) { Fail 'the client binary does not run from -Dir' }
if (Test-TaskExists) { Fail '-NoService installed a task' }
if ((Get-UserPath) -cne $pathBefore) { Fail '-NoPath changed the Path' }

Write-Host "`ninstall-smoke: PASS"
