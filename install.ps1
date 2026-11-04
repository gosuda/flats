# Flats installer for Windows.
#
#   irm https://raw.githubusercontent.com/gosuda/flats/main/install.ps1 | iex
#
# Downloads the prebuilt flats.exe from the GitHub release, verifies its SHA-256
# checksum, installs it and runs Flats as a background service that starts at
# login and comes back if it exits: a scheduled task of the current user. It
# needs no administrator rights.
#
# A new service runs `flats serve` with its defaults (Local network, Portal off).
# When the service is already installed, the installer replaces the binary and
# restarts a running service with its existing settings. It leaves a stopped
# service stopped.
#
# A piped iex takes no parameters. To pass options, run:
#
#   & ([scriptblock]::Create((irm <url>))) -Version v1.2.3
#
# Options:
#   -Dir DIR        install directory (FLATS_INSTALL_DIR,
#                   default %LOCALAPPDATA%\Programs\Flats)
#   -Version TAG    release to install (FLATS_VERSION, default latest)
#   -NoService      install only the binary (FLATS_NO_SERVICE=1)
#   -NoPath         leave the user's Path alone
#   -ServeArgs ...  (re)install the service with these `flats serve` flags
#
# FLATS_DATA selects the service data directory. FLATS_DOWNLOAD_BASE replaces the
# release download location with another https URL or a local directory holding
# the release files (for mirrors and tests).

[CmdletBinding()]
param(
	[string]$Dir,
	[string]$Version,
	[switch]$NoService,
	[switch]$NoPath,
	[string[]]$ServeArgs
)

function Invoke-FlatsInstall {
	param([string]$Dir, [string]$Version, [bool]$NoService, [bool]$NoPath, [string[]]$ServeArgs)

	$ErrorActionPreference = 'Stop'
	$ProgressPreference = 'SilentlyContinue'
	$repo = 'gosuda/flats'
	$defaultUrl = 'http://127.0.0.1:7878'

	function Say([string]$Message) { Write-Host "flats-install: $Message" }
	function Warn([string]$Message) { Write-Warning "flats-install: $Message" }
	function Die([string]$Message) { throw "flats-install: $Message" }

	function Test-Api([string]$Url) {
		try {
			Invoke-WebRequest -UseBasicParsing -TimeoutSec 3 -Uri "$Url/api/status" | Out-Null
			return $true
		} catch {
			return $false
		}
	}

	# Get-ConsoleUrl reads the console address from the config.json of the
	# installed service, or returns the default.
	function Get-ConsoleUrl([string]$Definition) {
		$xml = [xml](Get-Content -LiteralPath $Definition -Raw -Encoding Unicode)
		$arguments = $xml.Task.Actions.Exec.Arguments
		if ($arguments -notmatch '--config\s+(?:"([^"]+)"|(\S+))') { return $defaultUrl }
		$config = if ($Matches[1]) { $Matches[1] } else { $Matches[2] }
		if (-not (Test-Path -LiteralPath $config)) { return $defaultUrl }
		try {
			$doc = Get-Content -LiteralPath $config -Raw | ConvertFrom-Json
			if ($doc.host.console_addr) { return "http://$($doc.host.console_addr)" }
		} catch {
			Warn "cannot read $config; using the default console address"
		}
		return $defaultUrl
	}

	# Get-ListenUrl prints the console URL for the serve flags given.
	function Get-ListenUrl([string[]]$Flags) {
		for ($i = 0; $i -lt $Flags.Count; $i++) {
			if ($Flags[$i] -match '^--?listen=(.+)$') { return "http://$($Matches[1])" }
			if ($Flags[$i] -match '^--?listen$' -and $i + 1 -lt $Flags.Count) { return "http://$($Flags[$i + 1])" }
		}
		return $defaultUrl
	}

	# Send-EnvironmentChange tells the desktop that the Path changed, so that a new
	# terminal finds flats. It skips a window that does not respond: the broadcast
	# of [Environment]::SetEnvironmentVariable waits on such a window, and took
	# more than three minutes behind one hung program. The Path is already stored,
	# so a failure here costs only that message.
	function Send-EnvironmentChange {
		try {
			if (-not ('FlatsInstall.Native' -as [Type])) {
				Add-Type -Namespace FlatsInstall -Name Native -MemberDefinition @'
[DllImport("user32.dll", CharSet = CharSet.Unicode, SetLastError = true)]
public static extern IntPtr SendMessageTimeout(IntPtr hWnd, uint msg, UIntPtr wParam, string lParam, uint flags, uint timeout, out UIntPtr result);
'@
			}
			$broadcast = [IntPtr]0xffff
			$settingChange = 0x1a
			$abortIfHung = 2
			$result = [UIntPtr]::Zero
			[void][FlatsInstall.Native]::SendMessageTimeout($broadcast, $settingChange, [UIntPtr]::Zero, 'Environment', $abortIfHung, 2000, [ref]$result)
		} catch {
			Warn 'a terminal may not find flats until your next login'
		}
	}

	# Add-UserPath adds the directory to the user's Path. The value is read
	# without expansion and written as an expandable string, so that existing
	# %VAR% entries stay as they are.
	function Add-UserPath([string]$Directory) {
		$key = [Microsoft.Win32.Registry]::CurrentUser.OpenSubKey('Environment', $true)
		try {
			$current = $key.GetValue('Path', '', [Microsoft.Win32.RegistryValueOptions]::DoNotExpandEnvironmentNames)
			$wanted = $Directory.TrimEnd('\')
			foreach ($entry in ($current -split ';')) {
				if ($entry.TrimEnd('\') -ieq $wanted) { return $false }
			}
			$value = if ($current) { $current.TrimEnd(';') + ';' + $Directory } else { $Directory }
			$key.SetValue('Path', $value, [Microsoft.Win32.RegistryValueKind]::ExpandString)
		} finally {
			$key.Close()
		}
		Send-EnvironmentChange
		return $true
	}

	function Get-Release([string]$Source, [string]$Destination) {
		if ($Source -match '^https://') {
			Invoke-WebRequest -UseBasicParsing -Uri $Source -OutFile $Destination
		} else {
			Copy-Item -LiteralPath $Source -Destination $Destination
		}
	}

	# Get-LatestTag resolves the latest tag once, so that the archive and
	# checksums.txt come from one release.
	function Get-LatestTag {
		try {
			$request = [System.Net.WebRequest]::Create("https://github.com/$repo/releases/latest")
			$request.Method = 'HEAD'
			$response = $request.GetResponse()
			$leaf = $response.ResponseUri.Segments[-1]
			$response.Close()
			if ($leaf -match '^v[0-9]') { return $leaf }
			Die "no Flats release found at https://github.com/$repo/releases"
		} catch [System.Net.WebException] {
			Warn 'could not look up the latest release tag; downloading through the latest-release link'
			return $null
		}
	}

	if ($env:OS -ne 'Windows_NT') { Die 'this installer is for Windows; on macOS and Linux use install.sh' }
	$arch = $env:PROCESSOR_ARCHITEW6432
	if (-not $arch) { $arch = $env:PROCESSOR_ARCHITECTURE }
	switch ($arch) {
		'AMD64' { }
		'ARM64' { Say 'running the amd64 build through Windows emulation' }
		default { Die "unsupported CPU architecture: $arch (supported: amd64)" }
	}
	if (-not $env:LOCALAPPDATA) { Die 'LOCALAPPDATA is not set' }
	[Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12

	if (-not $Dir) { $Dir = $env:FLATS_INSTALL_DIR }
	if (-not $Dir) { $Dir = Join-Path $env:LOCALAPPDATA 'Programs\Flats' }
	$Dir = [IO.Path]::GetFullPath($Dir)
	if (-not $Version) { $Version = $env:FLATS_VERSION }
	if (-not $Version) { $Version = 'latest' }
	if ($Version -match '^[0-9]') { $Version = "v$Version" }
	if ($env:FLATS_NO_SERVICE -and $env:FLATS_NO_SERVICE -ne '0') { $NoService = $true }
	if ($NoService -and $ServeArgs) { Die 'serve flags need the service; drop -NoService' }
	$binary = Join-Path $Dir 'flats.exe'
	$definition = Join-Path $env:LOCALAPPDATA 'Flats\service.xml'
	$hasService = Test-Path -LiteralPath $definition

	# Decide what happens to the service before touching anything, so that a
	# conflict stops the installer with the old binary in place.
	$mode = 'none'
	$url = $defaultUrl
	if (-not $NoService) {
		if ($hasService -and -not $ServeArgs) {
			$url = Get-ConsoleUrl $definition
			if (Test-Api $url) { $mode = 'restart' } else { $mode = 'replace' }
			if ($env:FLATS_DATA) { Warn 'FLATS_DATA applies only when the service is reinstalled with -ServeArgs' }
		} else {
			$mode = 'install'
			$url = Get-ListenUrl $ServeArgs
			if (-not $hasService -and (Test-Api $url)) {
				Die "a Flats server is already running at $url outside the service; stop it first, or rerun with -NoService"
			}
		}
	} elseif ($hasService) {
		Warn 'the installed Flats service keeps running its current binary until it restarts'
	}

	$asset = 'flats_windows_amd64.zip'
	if ($env:FLATS_DOWNLOAD_BASE) {
		$base = $env:FLATS_DOWNLOAD_BASE.TrimEnd('/', '\')
	} else {
		if ($Version -eq 'latest') {
			$tag = Get-LatestTag
			if ($tag) { $Version = $tag }
		}
		if ($Version -eq 'latest') {
			$base = "https://github.com/$repo/releases/latest/download"
		} else {
			$base = "https://github.com/$repo/releases/download/$Version"
		}
	}

	$tmp = Join-Path ([IO.Path]::GetTempPath()) ("flats-install-" + [guid]::NewGuid().ToString('N'))
	New-Item -ItemType Directory -Path $tmp | Out-Null
	$staged = $null
	try {
		Say "downloading $asset ($Version)"
		$archive = Join-Path $tmp $asset
		$sums = Join-Path $tmp 'checksums.txt'
		try {
			Get-Release "$base/$asset" $archive
			Get-Release "$base/checksums.txt" $sums
		} catch {
			Die "download failed from $base (release $Version may not exist or may not have a Windows archive; see https://github.com/$repo/releases): $_"
		}
		$want = $null
		foreach ($line in Get-Content -LiteralPath $sums) {
			$fields = -split $line
			if ($fields.Count -eq 2 -and $fields[1].TrimStart('*') -eq $asset) {
				$want = $fields[0].ToLowerInvariant()
				break
			}
		}
		if (-not $want) { Die "checksums.txt has no entry for $asset" }
		$got = (Get-FileHash -Algorithm SHA256 -LiteralPath $archive).Hash.ToLowerInvariant()
		if ($want -ne $got) { Die "checksum mismatch for $asset" }
		$extracted = Join-Path $tmp 'x'
		Expand-Archive -LiteralPath $archive -DestinationPath $extracted
		$new = Join-Path $extracted 'flats.exe'
		if (-not (Test-Path -LiteralPath $new)) { Die "$asset has no flats.exe" }

		try {
			New-Item -ItemType Directory -Force -Path $Dir | Out-Null
			# Copy next to the target, so that the rename below stays on one
			# volume and nobody sees a partly written binary.
			$staged = Join-Path $Dir ".flats.install.$PID.exe"
			Copy-Item -LiteralPath $new -Destination $staged -Force
			Unblock-File -LiteralPath $staged
		} catch {
			Die "cannot write to $Dir; pass a writable -Dir: $_"
		}
		$newVersion = & $staged version 2>&1
		if ($LASTEXITCODE -ne 0) { Die "the downloaded binary does not run on this system: $newVersion" }

		# Windows lets a running binary move but not be replaced. A running
		# service keeps the old file open until the restart below.
		if (Test-Path -LiteralPath $binary) {
			$aside = "$binary.old-$([DateTime]::UtcNow.Ticks)"
			Move-Item -LiteralPath $binary -Destination $aside
			try {
				Move-Item -LiteralPath $staged -Destination $binary
			} catch {
				Move-Item -LiteralPath $aside -Destination $binary
				throw
			}
		} else {
			Move-Item -LiteralPath $staged -Destination $binary
		}
		$staged = $null
		Say "installed ${binary}: $newVersion"
	} finally {
		if ($staged) { Remove-Item -LiteralPath $staged -Force -ErrorAction SilentlyContinue }
		Remove-Item -LiteralPath $tmp -Recurse -Force -ErrorAction SilentlyContinue
	}

	switch ($mode) {
		'replace' {
			Warn 'the Flats service is installed but stopped; it uses the new binary once started'
		}
		'restart' {
			Say 'restarting the existing Flats service'
			& $binary --url $url install --executable $binary
			if ($LASTEXITCODE -ne 0) { Die 'the service did not restart' }
		}
		'install' {
			Say 'installing the Flats service'
			if ($ServeArgs) {
				& $binary --url $url install --executable $binary -- @ServeArgs
			} else {
				& $binary --url $url install --executable $binary
			}
			if ($LASTEXITCODE -ne 0) { Die 'service installation failed' }
		}
	}

	if ($mode -eq 'restart' -or $mode -eq 'install') {
		$healthy = $false
		for ($i = 0; $i -lt 30; $i++) {
			if (Test-Api $url) { $healthy = $true; break }
			Start-Sleep -Seconds 1
		}
		if ($healthy) {
			Say "Flats is running at $url"
		} else {
			Warn "the service is not answering at $url yet; see the log named above and ``flats status``"
		}
	}

	# An earlier binary is free now, unless a process still runs it.
	Get-ChildItem -LiteralPath $Dir -Filter 'flats.exe.old-*' -ErrorAction SilentlyContinue |
		Remove-Item -Force -ErrorAction SilentlyContinue

	$onPath = ($env:Path -split ';') | Where-Object { $_.TrimEnd('\') -ieq $Dir.TrimEnd('\') }
	if (-not $onPath) {
		if ($NoPath) {
			Say "$Dir is not on Path; add it to run ``flats`` by name"
		} elseif (Add-UserPath $Dir) {
			$env:Path = $env:Path.TrimEnd(';') + ';' + $Dir
			Say "added $Dir to your Path; a new terminal finds flats"
		} else {
			$env:Path = $env:Path.TrimEnd(';') + ';' + $Dir
		}
	}

	Write-Host ''
	if ($mode -eq 'none' -and $hasService) {
		Write-Host 'If the Flats service is running, restart it to use the new binary:  flats install'
	} elseif ($mode -eq 'replace') {
		Write-Host 'Start the service:  flats install'
	} elseif ($mode -eq 'none') {
		Write-Host 'Run a host in the foreground:  flats serve'
		Write-Host 'Or as a background service:    rerun this installer without -NoService'
	} else {
		$status = 'flats status'
		if ($url -ne $defaultUrl) { $status = "flats --url $url status" }
		Write-Host "Console:  $url"
		Write-Host "Status:   $status"
		Write-Host 'Remove:   flats uninstall   (keeps your flats and data)'
	}
	Write-Host ''
	Write-Host 'Agents:   flats mcp-config          (connect an agent)'
	Write-Host '          flats agent-setup docs    (optional: use Flats for documents)'
}

Invoke-FlatsInstall -Dir $Dir -Version $Version -NoService:$NoService.IsPresent -NoPath:$NoPath.IsPresent -ServeArgs $ServeArgs
