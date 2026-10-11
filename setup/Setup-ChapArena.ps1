<#
Setup wizard for the Windows release folder, which is meant to become the home of Chap Arena on the field PC: the exe,
event.db, recordings and logs all live in it, and in-app updates replace the exe in place. Start it by double-clicking
Setup-ChapArena.cmd; Rollback-ChapArena.cmd runs it with -Rollback.

Setup moves an existing Chap Arena into this folder (copying its database and recordings, keeping its flags and
pointing its shortcuts here), patches the field access point's radio API so applying one station doesn't restart
Wi-Fi on the others, and walks through the field test: R1 keeps driving while R3 changes team. It asks before every
change and is safe to run again. Rollback starts the old Chap Arena again and puts the AP's stock radio API back.

What it keeps for rollback, never overwriting:
  - backup\old-install.json: where the old Chap Arena was and how it was started. The old folder isn't changed.
  - backup\frc-radio-api.stock: the AP's stock radio API.
Everything shown is also saved to logs\setup-*.txt.

Written for Windows PowerShell 5.1 and kept to plain ASCII, which 5.1 needs to read the file correctly.
#>
param([switch]$Rollback)

$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
$Here = $PSScriptRoot
$Exe = Join-Path $Here 'chap-arena.exe'
$Db = Join-Path $Here 'event.db'
$Recordings = Join-Path $Here 'recordings'
$PatchedApi = Join-Path $Here 'frc-radio-api'
$BackupDir = Join-Path $Here 'backup'
$OldInstallFile = Join-Path $BackupDir 'old-install.json'
$StockApiBackup = Join-Path $BackupDir 'frc-radio-api.stock'
$ApKey = Join-Path $env:USERPROFILE '.ssh\chap-arena-ap'

# ---------------------------------------------------------------------------------------------------------------------
# Wizard library, after the /wizard skill's template: one stage per screen, progress, confirmations, a closing summary.
# ---------------------------------------------------------------------------------------------------------------------

$script:TotalStages = 0
$script:StageIndex = 0
$script:Done = New-Object System.Collections.Generic.List[string]
$script:ToDo = New-Object System.Collections.Generic.List[string]

function Clear-Stage { if (-not [Console]::IsOutputRedirected) { Clear-Host } }

function Banner([string]$Title, [string[]]$Lines) {
    Clear-Stage
    Write-Host ''
    Write-Host "  $Title" -ForegroundColor Cyan
    Write-Host "  $($script:TotalStages) stages" -ForegroundColor DarkGray
    Write-Host ''
    foreach ($line in $Lines) { Write-Host "  $line" -ForegroundColor DarkGray }
    Write-Host ''
    Pause-Wizard 'Ready to start? Press Enter'
}

# Clears the screen so only the current step shows, and announces the stage with progress.
function Stage([string]$Name) {
    Clear-Stage
    $script:StageIndex++
    Write-Host ''
    Write-Host ('> Stage {0}/{1} - {2}' -f $script:StageIndex, $script:TotalStages, $Name) -ForegroundColor Cyan
    Write-Host ''
}

function Say([string]$Text) { Write-Host "  $Text" }
function Step([string]$Text) { Write-Host '  * ' -ForegroundColor Cyan -NoNewline; Write-Host $Text }
function Note([string]$Text) { Write-Host "  $Text" -ForegroundColor DarkGray }
function Ok([string]$Text) { Write-Host '  ok ' -ForegroundColor Green -NoNewline; Write-Host $Text }
function Warn([string]$Text) { Write-Host "  ! $Text" -ForegroundColor Yellow }

function Pause-Wizard([string]$Message = 'Press Enter to continue') {
    Write-Host "  $Message " -ForegroundColor DarkGray -NoNewline
    [void](Read-Host)
}

function Confirm-Wizard([string]$Question) {
    Write-Host "  ? $Question [y/N] " -ForegroundColor Yellow -NoNewline
    return ((Read-Host) -match '^[Yy]')
}

function Ask([string]$Prompt) {
    Write-Host "  $Prompt " -NoNewline
    return (Read-Host)
}

function Finish([string]$Title, [string]$LogPath) {
    Clear-Stage
    Write-Host ''
    Write-Host "  $Title" -ForegroundColor Green
    foreach ($item in $script:Done) { Ok $item }
    if ($script:ToDo.Count) {
        Write-Host ''
        Warn 'Still to do:'
        foreach ($item in $script:ToDo) { Note "  - $item" }
    }
    Write-Host ''
    Note "Everything above is saved in $LogPath"
    Write-Host ''
}

# ---------------------------------------------------------------------------------------------------------------------
# Chap Arena: finding it, moving it into this folder, starting and stopping it.
# ---------------------------------------------------------------------------------------------------------------------

# Runs a program and returns its output lines (stdout and stderr) and exit code. Windows PowerShell turns a program's
# stderr into errors, so error handling is relaxed while it runs.
function Invoke-Program([string]$File, [string[]]$Arguments) {
    $previous = $ErrorActionPreference
    $ErrorActionPreference = 'Continue'
    try {
        $lines = @(& $File @Arguments 2>&1 | ForEach-Object { "$_" })
        $code = $LASTEXITCODE
    } finally {
        $ErrorActionPreference = $previous
    }
    return [pscustomobject]@{ Lines = $lines; ExitCode = $code; Text = ($lines -join "`n") }
}

function Get-Sha256([string]$Path) { (Get-FileHash -Path $Path -Algorithm SHA256).Hash.ToLower() }

function Test-SamePath([string]$A, [string]$B) {
    if (-not $A -or -not $B) { return $false }
    return ([IO.Path]::GetFullPath($A).TrimEnd('\') -ieq [IO.Path]::GetFullPath($B).TrimEnd('\'))
}

# A running Chap Arena: its exe, the flags after it, and where its database is. Flags are as typed on the command line.
function New-Install([string]$ExePath, [string]$Arguments) {
    $dbPath = Join-Path (Split-Path $ExePath) 'event.db'
    if ($Arguments -match '(?:^|\s)--?db(?:=|\s+)(?:"([^"]+)"|(\S+))') {
        $dbPath = $Matches[1]
        if (-not $dbPath) { $dbPath = $Matches[2] }
        if (-not [IO.Path]::IsPathRooted($dbPath)) { $dbPath = Join-Path (Split-Path $ExePath) $dbPath }
    }
    return [pscustomobject]@{ Exe = $ExePath; Arguments = $Arguments; Db = $dbPath }
}

# The web UI address for the given flags.
function Get-ArenaUrl([string]$Arguments) {
    $port = 8080
    if ($Arguments -match '(?:^|\s)--?port(?:=|\s+)(\d+)') { $port = [int]$Matches[1] }
    return "http://localhost:$port"
}

# Flags for running from this folder: the same as before, minus -db so the database is the one here, and with
# -recordings-port renamed to its newer name, -team-port.
function Convert-Arguments([string]$Arguments) {
    $converted = $Arguments -replace '(?:^|\s)--?db(?:=|\s+)(?:"[^"]+"|\S+)', ''
    $converted = $converted -replace '(^|\s)(--?)recordings-port\b', '$1$2team-port'
    return ($converted -replace '\s+', ' ').Trim()
}

function Get-InstallProcess([string]$ExePath) {
    Get-CimInstance Win32_Process | Where-Object { Test-SamePath $_.ExecutablePath $ExePath } | Select-Object -First 1
}

# The Chap Arena this folder runs: its flags and web UI address. Filled in by Find-Arena.
$script:Arena = [pscustomobject]@{ Arguments = ''; Url = 'http://localhost:8080' }
# The Chap Arena being moved from, if any. Filled in by Find-Arena.
$script:OldInstall = $null
# Settings read from this folder's Chap Arena once it's running.
$script:Settings = $null

function Get-ArenaJson([string]$Path) { Invoke-RestMethod -Uri "$($script:Arena.Url)$Path" -TimeoutSec 3 }

# Works out which Chap Arena runs now. If it's this folder's, that's the one to use. If it's another one, or none runs
# and one is named, that's the old install to move from; a new install has neither. Once Chap Arena has been moved
# here, a stopped one means this folder's, so a second run can't copy the old, stale database over this one.
function Find-Arena {
    $running = Get-CimInstance Win32_Process |
        Where-Object { $_.Name -in @('chap-arena.exe', 'cheesy-arena.exe') } | Select-Object -First 1
    $arguments = ''
    if ($running -and $running.CommandLine -match '^\s*("[^"]*"|\S+)\s*(.*)$') { $arguments = $Matches[2].Trim() }

    if ($running -and (Test-SamePath $running.ExecutablePath $Exe)) {
        Ok 'Chap Arena already runs from this folder.'
        if ($arguments) { Note "started with: $arguments" }
        $script:Arena = [pscustomobject]@{ Arguments = $arguments; Url = (Get-ArenaUrl $arguments) }
        return
    }
    if (-not $running -and (Test-Path -LiteralPath $OldInstallFile)) {
        $old = Get-Content -LiteralPath $OldInstallFile -Raw | ConvertFrom-Json
        $arguments = Convert-Arguments $old.Arguments
        Ok "Chap Arena was already moved here from $(Split-Path $old.Exe)."
        $script:Arena = [pscustomobject]@{ Arguments = $arguments; Url = (Get-ArenaUrl $arguments) }
        return
    }

    $oldExe = ''
    if ($running) {
        Say "Chap Arena is running from $($running.ExecutablePath)"
        if ($arguments) { Note "started with: $arguments" }
        if (Confirm-Wizard 'Is this the Chap Arena the field uses?') { $oldExe = $running.ExecutablePath }
        else { $arguments = '' }
    } else {
        Say 'Chap Arena isn''t running right now.'
    }
    if (-not $oldExe) {
        $answer = (Ask 'Full path to the chap-arena.exe the field uses (Enter if there isn''t one):').Trim().Trim('"')
        while ($answer -and -not (Test-Path -LiteralPath $answer -PathType Leaf)) {
            Warn "There's no file at $answer"
            $answer = (Ask 'Full path to chap-arena.exe (Enter if there isn''t one):').Trim().Trim('"')
        }
        if ($answer) {
            $oldExe = $answer
            $arguments = (Ask 'Flags it runs with, if any (Enter for none):').Trim()
        }
    }

    if ($oldExe) {
        $script:OldInstall = New-Install $oldExe $arguments
        $arguments = Convert-Arguments $arguments
        Ok "Moving from $oldExe"
    } else {
        Ok 'Setting up Chap Arena fresh in this folder.'
    }
    $script:Arena = [pscustomobject]@{ Arguments = $arguments; Url = (Get-ArenaUrl $arguments) }
}

# Asks for the Chap Arena running from ExePath to be closed, waits for it, and offers to force it.
function Stop-Install([string]$ExePath, [string]$Url) {
    if (-not (Get-InstallProcess $ExePath)) { return }
    try {
        if ((Invoke-RestMethod -Uri "$Url/api/status" -TimeoutSec 3).recording.active) {
            Warn 'Chap Arena is recording. Stop the recording first so the files are finished properly.'
            Pause-Wizard 'Press Enter once it has stopped'
        }
    } catch { }
    Step "Close the Chap Arena running from $ExePath (click its X, or press Ctrl+C in it)."
    Note 'Waiting for it to close...'
    $deadline = (Get-Date).AddSeconds(90)
    while ((Get-InstallProcess $ExePath) -and (Get-Date) -lt $deadline) { Start-Sleep -Seconds 1 }
    if (Get-InstallProcess $ExePath) {
        if (-not (Confirm-Wizard 'It''s still running. Close it from here?')) {
            throw 'Chap Arena has to be closed first.'
        }
        Stop-Process -Id (Get-InstallProcess $ExePath).ProcessId -Force
        Start-Sleep -Seconds 1
    }
    Ok 'Chap Arena is closed.'
}

# Starts a Chap Arena and waits for its web UI. Returns its settings.
function Start-Install([string]$ExePath, [string]$Arguments) {
    $parameters = @{ FilePath = $ExePath; WorkingDirectory = (Split-Path $ExePath) }
    if ($Arguments) { $parameters.ArgumentList = $Arguments }
    Start-Process @parameters
    $url = Get-ArenaUrl $Arguments
    $deadline = (Get-Date).AddSeconds(30)
    while ((Get-Date) -lt $deadline) {
        try { return (Invoke-RestMethod -Uri "$url/api/settings" -TimeoutSec 3) } catch { Start-Sleep -Seconds 1 }
    }
    throw "Chap Arena didn't start answering at $url."
}

# Shortcuts this user can change that start the given exe: on the desktop, in Startup and in the Start menu.
function Find-Shortcuts([string]$ExePath, [string[]]$Folders = @([Environment]::GetFolderPath('Desktop'),
        [Environment]::GetFolderPath('Startup'), [Environment]::GetFolderPath('Programs'))) {
    $shell = New-Object -ComObject WScript.Shell
    foreach ($folder in $Folders) {
        if (-not $folder -or -not (Test-Path -LiteralPath $folder)) { continue }
        foreach ($file in Get-ChildItem -LiteralPath $folder -Filter '*.lnk' -Recurse -ErrorAction SilentlyContinue) {
            $shortcut = $shell.CreateShortcut($file.FullName)
            if (Test-SamePath $shortcut.TargetPath $ExePath) { $shortcut }
        }
    }
}

# Copies the old install's database and recordings here, starts Chap Arena from this folder with the same flags, and
# points the old install's shortcuts here. The old folder isn't changed, so rollback can go back to it.
function Move-Install {
    $old = $script:OldInstall
    $oldRecordings = Join-Path (Split-Path $old.Db) 'recordings'
    $recordingFiles = @()
    if (Test-Path -LiteralPath $oldRecordings) {
        $recordingFiles = @(Get-ChildItem -LiteralPath $oldRecordings -Recurse -File -ErrorAction SilentlyContinue)
    }
    $recordingMb = [math]::Round(($recordingFiles | Measure-Object -Property Length -Sum).Sum / 1MB)

    Say 'This copies the old Chap Arena''s data here and runs Chap Arena from this folder from now on:'
    Note "  database:   $($old.Db)"
    if ($recordingFiles.Count) { Note "  recordings: $oldRecordings ($($recordingFiles.Count) files, $recordingMb MB)" }
    if ($script:Arena.Arguments) { Note "  flags:      $($script:Arena.Arguments)" }
    Say 'The old folder is left as it is. Chap Arena restarts, which drops driver stations for a moment.'
    if (-not (Confirm-Wizard 'Move Chap Arena into this folder now?')) {
        $script:ToDo.Add('Chap Arena still runs from its old folder.')
        return $false
    }

    New-Item -ItemType Directory -Force -Path $BackupDir | Out-Null
    if (-not (Test-Path -LiteralPath $OldInstallFile)) {
        $old | ConvertTo-Json | Set-Content -LiteralPath $OldInstallFile
    }
    Stop-Install $old.Exe (Get-ArenaUrl $old.Arguments)

    if (Test-Path -LiteralPath $old.Db) {
        if ((Test-Path -LiteralPath $Db) -and (Get-Sha256 $Db) -ne (Get-Sha256 $old.Db)) {
            $kept = Join-Path $Here ('event.replaced-{0}.db' -f (Get-Date -Format 'yyyyMMdd-HHmmss'))
            Note "This folder already had an event.db; it's kept as $kept"
            Move-Item -LiteralPath $Db -Destination $kept
        }
        Copy-Item -LiteralPath $old.Db -Destination $Db -Force
        Ok 'Copied the database.'
    } else {
        Warn "There's no database at $($old.Db), so Chap Arena starts with an empty one."
    }
    if ($recordingFiles.Count) {
        New-Item -ItemType Directory -Force -Path $Recordings | Out-Null
        Copy-Item -Path (Join-Path $oldRecordings '*') -Destination $Recordings -Recurse -Force
        Ok 'Copied the recordings.'
    }

    Say 'Starting Chap Arena from this folder...'
    Warn 'If Windows asks whether Chap Arena may use the network, tick every box and click Allow access.'
    Warn 'Driver stations can''t connect otherwise.'
    $script:Settings = Start-Install $Exe $script:Arena.Arguments
    Ok 'Chap Arena runs from this folder.'
    $script:Done.Add("Moved Chap Arena into $Here (the old folder is untouched).")

    $shortcuts = @(Find-Shortcuts $old.Exe)
    if ($shortcuts.Count) {
        Say 'These shortcuts start the old Chap Arena:'
        $shortcuts | ForEach-Object { Note "  $($_.FullName)" }
        if (Confirm-Wizard 'Point them at this folder?') {
            foreach ($shortcut in $shortcuts) {
                $shortcut.TargetPath = $Exe
                $shortcut.WorkingDirectory = $Here
                $shortcut.Arguments = Convert-Arguments $shortcut.Arguments
                $shortcut.Save()
            }
            Ok 'Shortcuts now start Chap Arena from this folder.'
        }
    } elseif (Confirm-Wizard 'Add a Chap Arena shortcut to the desktop?') {
        New-DesktopShortcut
    }
    Note 'If anything else starts the old Chap Arena (like a scheduled task), point it at'
    Note "  $Exe"
    return $true
}

function New-DesktopShortcut {
    $shell = New-Object -ComObject WScript.Shell
    $shortcut = $shell.CreateShortcut((Join-Path ([Environment]::GetFolderPath('Desktop')) 'Chap Arena.lnk'))
    $shortcut.TargetPath = $Exe
    $shortcut.WorkingDirectory = $Here
    $shortcut.Arguments = $script:Arena.Arguments
    $shortcut.Save()
    Ok 'Added a Chap Arena shortcut to the desktop.'
}

# Makes sure this folder's Chap Arena is running and reads its settings and the AP address.
function Read-ArenaSettings([switch]$Start) {
    try { $script:Settings = Get-ArenaJson '/api/settings' } catch {
        $script:Settings = $null
        if ($Start) {
            Say 'Starting Chap Arena...'
            Warn 'If Windows asks whether Chap Arena may use the network, tick every box and click Allow access.'
            $script:Settings = Start-Install $Exe $script:Arena.Arguments
        }
    }
    if ($script:Settings) { $script:ApAddress = $script:Settings.ApAddress }
    if (-not $script:ApAddress) {
        $script:ApAddress = (Ask 'Access point address (Enter for 10.0.100.2):').Trim()
        if (-not $script:ApAddress) { $script:ApAddress = '10.0.100.2' }
    }
    $script:ApHost = $script:ApAddress -replace ':\d+$', ''
}

# Starts the old Chap Arena again, from what Move-Install saved.
function Restore-OldInstall {
    if (-not (Test-Path -LiteralPath $OldInstallFile)) {
        Note 'Chap Arena wasn''t moved here from another folder, so there''s nothing to go back to.'
        return
    }
    $old = Get-Content -LiteralPath $OldInstallFile -Raw | ConvertFrom-Json
    if (-not (Test-Path -LiteralPath $old.Exe)) {
        Warn "The old Chap Arena isn't at $($old.Exe) anymore."
        return
    }
    Say "This goes back to the Chap Arena in $(Split-Path $old.Exe)."
    Note 'Anything changed since the move (teams, passwords, settings) is in this folder''s'
    Note 'event.db, not the old one, and won''t carry back.'
    if (-not (Confirm-Wizard 'Go back to the old Chap Arena?')) { return }
    $current = Get-InstallProcess $Exe
    if ($current) {
        $arguments = ''
        if ($current.CommandLine -match '^\s*("[^"]*"|\S+)\s*(.*)$') { $arguments = $Matches[2].Trim() }
        Stop-Install $Exe (Get-ArenaUrl $arguments)
    }
    Start-Install $old.Exe $old.Arguments | Out-Null
    Ok 'The old Chap Arena is running again.'
    Note 'Shortcuts still point at this folder; point them back by hand if you''re staying on the old one.'
    $script:Done.Add('Went back to the old Chap Arena.')
}

# ---------------------------------------------------------------------------------------------------------------------
# The access point: SSH, installing the radio API, reading its log.
# ---------------------------------------------------------------------------------------------------------------------

$script:ApAddress = ''
$script:ApHost = ''
$script:ApReady = $false
$script:ScpArgs = @()

# Checks the OpenSSH client is installed and works out how scp has to talk to the AP.
function Initialize-Ssh {
    foreach ($tool in @('ssh', 'scp', 'ssh-keygen')) {
        if (-not (Get-Command $tool -ErrorAction SilentlyContinue)) {
            throw "$tool isn't installed. Add 'OpenSSH Client' under Settings > System > Optional features, then run this again."
        }
    }
    # OpenSSH 9 and later copy files over SFTP unless told otherwise, and the AP only speaks the older SCP protocol.
    $sshVersion = (Invoke-Program 'ssh' @('-V')).Text.Trim()
    if ($sshVersion -match 'OpenSSH_(?:for_Windows_)?(\d+)' -and [int]$Matches[1] -ge 9) { $script:ScpArgs = @('-O') }
    Ok "SSH is installed ($sshVersion)."
}

function Get-SshArgs { @('-i', $ApKey, '-o', 'BatchMode=yes', '-o', 'ConnectTimeout=8', '-o', 'StrictHostKeyChecking=accept-new') }

# Runs a shell command on the AP as root with this PC's key. Commands avoid double quotes, which Windows PowerShell
# mangles when passing arguments to programs.
function Invoke-AP([string]$Command) { Invoke-Program 'ssh' ((Get-SshArgs) + @("root@$($script:ApHost)", $Command)) }

function Copy-ToAP([string]$Local, [string]$Remote) {
    Invoke-Program 'scp' ($script:ScpArgs + (Get-SshArgs) + @($Local, "root@$($script:ApHost):$Remote"))
}

function Copy-FromAP([string]$Remote, [string]$Local) {
    Invoke-Program 'scp' ($script:ScpArgs + (Get-SshArgs) + @("root@$($script:ApHost):$Remote", $Local))
}

function Test-ApHealthy {
    try { return ((Invoke-RestMethod -Uri "http://$($script:ApAddress)/health" -TimeoutSec 3) -match 'OK') } catch { return $false }
}

function Wait-ApHealthy([int]$Seconds = 40) {
    $deadline = (Get-Date).AddSeconds($Seconds)
    while ((Get-Date) -lt $deadline) {
        if (Test-ApHealthy) { return $true }
        Start-Sleep -Seconds 1
    }
    return $false
}

# Logs in to the AP with this PC's key, first adding the key to the AP (one root password prompt) if it isn't there.
function Connect-AP {
    Say "Access point: $($script:ApHost)"
    if (-not (Test-NetConnection -ComputerName $script:ApHost -Port 22 -InformationLevel Quiet -WarningAction SilentlyContinue)) {
        Warn "Can't reach SSH on $($script:ApHost). Is this PC plugged into the field network?"
        return $false
    }
    if (-not (Test-Path -LiteralPath $ApKey)) {
        New-Item -ItemType Directory -Force -Path (Split-Path $ApKey) | Out-Null
        Start-Process -FilePath 'ssh-keygen' -NoNewWindow -Wait `
            -ArgumentList "-t ed25519 -N `"`" -q -C chap-arena-field-pc -f `"$ApKey`""
    }
    if ((Invoke-AP 'echo ready').ExitCode -eq 0) {
        Ok 'Logged in to the AP with this PC''s key.'
        return $true
    }

    Say 'Every later step logs in to the AP. Adding this PC''s SSH key to it means you type'
    Say 'its root password once instead of every time. The key stays on the AP.'
    if (-not (Confirm-Wizard 'Add this PC''s key to the AP?')) { return $false }
    Step 'Type the AP''s root password when asked. If it asks to trust the AP, type yes.'
    $publicKey = (Get-Content -LiteralPath "$ApKey.pub" -Raw).Trim()
    $install = "mkdir -p /etc/dropbear /root/.ssh && for f in /etc/dropbear/authorized_keys /root/.ssh/authorized_keys; " +
        "do touch `$f; grep -qF '$publicKey' `$f || echo '$publicKey' >> `$f; chmod 600 `$f; done"
    $previous = $ErrorActionPreference
    $ErrorActionPreference = 'Continue'
    & ssh -o ConnectTimeout=8 -o StrictHostKeyChecking=accept-new "root@$($script:ApHost)" $install
    $ErrorActionPreference = $previous

    $probe = Invoke-AP 'echo ready'
    if ($probe.ExitCode -eq 0) {
        Ok 'Key added; logged in to the AP.'
        return $true
    }
    $probe.Lines | ForEach-Object { Note $_ }
    if ($probe.Text -match 'IDENTIFICATION HAS CHANGED') {
        Warn "This PC remembers a different AP at $($script:ApHost). If the AP was reflashed, run"
        Warn "  ssh-keygen -R $($script:ApHost)"
        Warn 'and then this wizard again.'
    } else {
        Warn 'Couldn''t log in to the AP as root. The AP part has to wait until SSH works.'
    }
    return $false
}

# What the AP reports about itself, as a hashtable. Avoids double quotes; see Invoke-AP.
function Get-ApFacts {
    $command = @(
        'echo model=$(uci get system.@system[0].model 2>/dev/null)'
        'echo firmware=$(cat /etc/vh_firmware 2>/dev/null)'
        'echo binary=$(test -f /usr/bin/frc-radio-api && echo yes)'
        'echo init=$(test -f /etc/init.d/frc-radio-api && echo yes)'
        'echo hostapdconf=$(test -f /var/run/hostapd-ath12.conf && echo yes)'
        'echo hostapdsocket=$(test -S /var/run/hostapd/global && echo yes)'
        'echo wpacli=$(command -v wpa_cli)'
        'echo freekb=$( (df -k /overlay 2>/dev/null || df -k /) | awk ''NR==2 {print $4}'')'
        'echo size=$(wc -c < /usr/bin/frc-radio-api 2>/dev/null)'
        'echo sha256=$(sha256sum /usr/bin/frc-radio-api 2>/dev/null | cut -d '' '' -f1)'
    ) -join '; '
    $facts = @{}
    foreach ($line in (Invoke-AP $command).Lines) {
        if ($line -match '^(\w+)=(.*)$') { $facts[$Matches[1]] = $Matches[2].Trim() }
    }
    return $facts
}

# Checks that a file uploaded to the AP matches the local one, by SHA-256 or, without sha256sum, by size.
function Test-ApCopy([string]$Remote, [string]$Local) {
    $result = Invoke-AP "sha256sum $Remote 2>/dev/null | cut -d ' ' -f1; wc -c < $Remote"
    $lines = @($result.Lines | Where-Object { $_ })
    if ($lines.Count -ge 2) { return ($lines[0] -eq (Get-Sha256 $Local)) }
    if ($lines.Count -eq 1) { return ([int64]$lines[0].Trim() -eq (Get-Item -LiteralPath $Local).Length) }
    return $false
}

# Puts the given local radio API binary on the AP and restarts the API. The file is staged next to the old binary
# first, so running out of flash fails before the working binary is touched. Wi-Fi isn't touched.
function Install-ApApi([string]$Local) {
    if ((Copy-ToAP $Local '/tmp/frc-radio-api.upload').ExitCode -ne 0 -or -not (Test-ApCopy '/tmp/frc-radio-api.upload' $Local)) {
        Warn 'Copying the file to the AP failed; nothing on the AP was changed.'
        return $false
    }
    $swap = 'cp /tmp/frc-radio-api.upload /usr/bin/frc-radio-api.next || { rm -f /usr/bin/frc-radio-api.next; ' +
        'rm -f /tmp/frc-radio-api.upload; exit 1; }; chmod +x /usr/bin/frc-radio-api.next; ' +
        '/etc/init.d/frc-radio-api stop; mv /usr/bin/frc-radio-api.next /usr/bin/frc-radio-api && ' +
        'rm -f /tmp/frc-radio-api.upload && /etc/init.d/frc-radio-api start'
    $result = Invoke-AP $swap
    $result.Lines | ForEach-Object { Note $_ }
    if ($result.ExitCode -ne 0) {
        Warn 'Swapping the binary on the AP failed (likely out of flash space).'
        if (-not (Test-ApHealthy)) { Invoke-AP '/etc/init.d/frc-radio-api start' | Out-Null }
        return $false
    }
    if (-not (Wait-ApHealthy)) {
        Warn 'The radio API didn''t come back. Run Rollback-ChapArena.cmd to put the stock one back.'
        return $false
    }
    return $true
}

# Puts the AP's stock radio API back from backup\.
function Restore-ApApi {
    if (-not (Test-Path -LiteralPath $StockApiBackup)) {
        Note "There's no stock radio API saved in $BackupDir, so there's nothing to put back."
        return
    }
    if (-not (Confirm-Wizard 'Put the AP''s stock radio API back?')) { return }
    if (Install-ApApi $StockApiBackup) {
        Ok 'The AP runs its stock radio API again.'
        $script:Done.Add('Put the AP''s stock radio API back.')
    } else {
        $script:ToDo.Add('Putting the AP''s stock radio API back failed; see the log above.')
    }
}

# Prints what the AP's radio API logged about recent configurations and says which path it took. Leaves out the
# request lines, which hold WPA keys.
function Show-ApLog {
    $pattern = 'Configured Wi-Fi by restarting|reloading all of Wi-Fi|Couldn.t restart|Successfully configured Wi-Fi|Wi-Fi configuration'
    $lines = @((Invoke-AP "grep -E '$pattern' /root/frc-radio-api.log | tail -n 8").Lines)
    if (-not $lines) {
        Note 'The AP''s radio API log has nothing about configuring Wi-Fi yet.'
        return
    }
    Say 'Latest Wi-Fi configuration lines from the AP''s radio API log:'
    $lines | ForEach-Object { Note "  $_" }
    $latest = $lines[-1]
    if ($latest -match 'restarting only') { Ok 'The last configuration restarted only the changed stations.' }
    elseif ($latest -match 'Successfully configured') { Warn 'The last configuration reloaded all of Wi-Fi; the lines above say why.' }
}

# ---------------------------------------------------------------------------------------------------------------------
# The field test.
# ---------------------------------------------------------------------------------------------------------------------

# One line describing a poll of Chap Arena: each assigned station's team, radio link and, while FMS is on, whether its
# driver station has robot comms.
function Format-Sample($status) {
    if ($null -eq $status) { return 'Chap Arena isn''t answering' }
    $parts = @()
    foreach ($station in $status.stations) {
        if (-not $station.assignment.teamId) { continue }
        $radio = 'DOWN'
        if ($station.wifi.RadioLinked) { $radio = 'up' }
        $text = '{0} {1} radio {2}' -f $station.station, $station.assignment.teamId, $radio
        if ($status.driverStationMode -ne 'off') {
            $robot = 'DOWN'
            if ($station.driverStation.connected -and $station.driverStation.robotLinked) { $robot = 'up' }
            $text += ", robot comms $robot"
        }
        $parts += $text
    }
    $parts += "AP $($status.accessPointStatus), switch $($status.switchStatus)"
    return ($parts -join ' | ')
}

# Polls Chap Arena twice a second until Enter is pressed, printing whenever something changes. Returns the samples.
function Watch-Field {
    $samples = New-Object System.Collections.Generic.List[object]
    $start = Get-Date
    $last = ''
    while ([Console]::KeyAvailable) { [void][Console]::ReadKey($true) }
    while (((Get-Date) - $start).TotalMinutes -lt 10) {
        if ([Console]::KeyAvailable -and [Console]::ReadKey($true).Key -eq 'Enter') { break }
        $status = $null
        try { $status = Get-ArenaJson '/api/status' } catch { }
        $seconds = [math]::Round(((Get-Date) - $start).TotalSeconds, 1)
        $samples.Add([pscustomobject]@{ Seconds = $seconds; Status = $status })
        $line = Format-Sample $status
        if ($line -ne $last) {
            Write-Host ('  {0,6:N1}s  {1}' -f $seconds, $line)
            $last = $line
        }
        Start-Sleep -Milliseconds 500
    }
    return $samples
}

# Looks at a watch: which stations changed team, and whether any station that kept its team lost its radio link or
# robot comms. Prints the findings and returns whether the test passed.
function Test-Watch($samples) {
    $polled = @($samples | Where-Object { $_.Status })
    if ($polled.Count -lt 2) {
        Warn 'Chap Arena didn''t answer during the watch, so there''s nothing to judge.'
        return $false
    }
    $first = $polled[0].Status
    $fmsOn = $first.driverStationMode -ne 'off'
    $changed = @()
    $passed = $true
    for ($i = 0; $i -lt 6; $i++) {
        $name = $first.stations[$i].station
        $startTeam = $first.stations[$i].assignment.teamId
        $teams = @($polled | ForEach-Object { $_.Status.stations[$i].assignment.teamId } | Select-Object -Unique)
        if ($teams.Count -gt 1) {
            $newTeam = $polled[-1].Status.stations[$i].assignment.teamId
            $changeAt = ($polled | Where-Object { $_.Status.stations[$i].assignment.teamId -ne $startTeam } | Select-Object -First 1).Seconds
            $linkedAt = ($polled | Where-Object {
                $_.Seconds -ge $changeAt -and $_.Status.stations[$i].wifi.RadioLinked -and $_.Status.stations[$i].wifi.TeamId -eq $newTeam
            } | Select-Object -First 1).Seconds
            $changed += $name
            $relink = 'its new radio never linked during the watch'
            if (-not $newTeam) { $relink = 'it was emptied' }
            elseif ($null -ne $linkedAt) { $relink = 'its new radio linked {0:N1}s later' -f ($linkedAt - $changeAt) }
            Say ('{0} changed from {1} to {2} at {3:N1}s; {4}.' -f $name, $startTeam, $newTeam, $changeAt, $relink)
            continue
        }
        if (-not $startTeam -or -not $first.stations[$i].wifi.RadioLinked) { continue }
        $radioDown = @($polled | Where-Object { -not $_.Status.stations[$i].wifi.RadioLinked }).Count
        $robotDown = 0
        if ($fmsOn) {
            $robotDown = @($polled | Where-Object {
                -not ($_.Status.stations[$i].driverStation.connected -and $_.Status.stations[$i].driverStation.robotLinked)
            }).Count
        }
        if ($radioDown -or $robotDown) {
            $passed = $false
            Warn ('{0} ({1}) kept its team but lost its radio link for about {2:N1}s and robot comms for about {3:N1}s.' -f `
                $name, $startTeam, ($radioDown * 0.5), ($robotDown * 0.5))
        } else {
            Ok ('{0} ({1}) kept its team and never lost its radio link{2}.' -f $name, $startTeam, $(if ($fmsOn) { ' or robot comms' } else { '' }))
        }
    }
    if (-not $changed) {
        Warn 'No station changed team during the watch.'
        return $false
    }
    if (-not $fmsOn) { Note 'FMS was off, so only radio links were watched, not driver station robot comms.' }
    return $passed
}

# ---------------------------------------------------------------------------------------------------------------------
# Stages.
# ---------------------------------------------------------------------------------------------------------------------

New-Item -ItemType Directory -Force -Path (Join-Path $Here 'logs'), $BackupDir | Out-Null
$mode = 'setup'
if ($Rollback) { $mode = 'rollback' }
$LogPath = Join-Path $Here ('logs\{0}-{1}.txt' -f $mode, (Get-Date -Format 'yyyyMMdd-HHmmss'))
Start-Transcript -Path $LogPath | Out-Null

try {
    if ($Rollback) {
        $script:TotalStages = 2
        Banner 'Go back to the old Chap Arena and the AP''s stock radio API' @(
            'Each part asks before it changes anything. Stop any time with Ctrl+C.'
        )

        Stage 'Go back to the old Chap Arena'
        Restore-OldInstall
        Pause-Wizard

        Stage 'Put the AP''s stock radio API back'
        Initialize-Ssh
        $current = Get-InstallProcess $Exe
        if ($current -and $current.CommandLine -match '^\s*("[^"]*"|\S+)\s*(.*)$') {
            $script:Arena = [pscustomobject]@{ Arguments = $Matches[2].Trim(); Url = (Get-ArenaUrl $Matches[2].Trim()) }
        }
        Read-ArenaSettings
        if (Connect-AP) { Restore-ApApi }
        Pause-Wizard

        Finish 'Rollback finished' $LogPath
        return
    }

    $script:TotalStages = 8
    Banner 'Set up Chap Arena in this folder and patch the field AP' @(
        "From now on Chap Arena runs from $Here"
        'and updates itself from the Settings page. Keep this folder where it is.'
        ''
        'Do stages 1 to 6 with no robot running: Chap Arena may restart in stage 3.'
        'Stage 7 is the field test and needs a robot at R1 and a second team for R3.'
        'Every change asks first, and Rollback-ChapArena.cmd undoes them. Stop any time with Ctrl+C.'
    )

    Stage 'Check this folder and this PC''s tools'
    foreach ($file in @($Exe, $PatchedApi)) {
        if (-not (Test-Path -LiteralPath $file)) { throw "$(Split-Path $file -Leaf) is missing from $Here. Extract the whole zip again." }
    }
    # Files from a downloaded zip are marked as coming from the internet, which makes Windows ask before starting them.
    Unblock-File -LiteralPath $Exe
    Ok 'chap-arena.exe and the patched radio API are here.'
    if ($Here -match '\\OneDrive\\|\\Downloads\\|\\Temp\\') {
        Warn "This folder is under $($Matches[0].Trim('\')). Chap Arena keeps its database here, so a"
        Warn 'permanent spot like C:\chap-arena is better. To move it, stop here (Ctrl+C), move the'
        Warn 'folder and run this again.'
    }
    Initialize-Ssh
    Pause-Wizard

    Stage 'Find the Chap Arena the field uses'
    Find-Arena
    Pause-Wizard

    Stage 'Run Chap Arena from this folder'
    if ($script:OldInstall) {
        [void](Move-Install)
        Read-ArenaSettings
    } else {
        Read-ArenaSettings -Start
        if (-not (Find-Shortcuts $Exe) -and (Confirm-Wizard 'Add a Chap Arena shortcut to the desktop?')) { New-DesktopShortcut }
    }
    if ($script:Settings -and $script:Settings.PSObject.Properties.Name -contains 'SCCStation1Port') {
        Ok "Chap Arena is running at $($script:Arena.Url)"
    } elseif ($script:Settings) {
        Warn 'The Chap Arena that''s running doesn''t look like this folder''s.'
        $script:ToDo.Add('Check which Chap Arena is running.')
    }
    Pause-Wizard

    Stage 'Check the SCC station ports'
    if ($script:Settings -and $script:Settings.SCCManagementEnabled) {
        Say 'SCC management is on. Applying turns off only the changed stations'' ports, using:'
        Note "  Station 1: $($script:Settings.SCCStation1Port)"
        Note "  Station 2: $($script:Settings.SCCStation2Port)"
        Note "  Station 3: $($script:Settings.SCCStation3Port)"
        Step 'Check that these are the SCC ports driver stations 1, 2 and 3 plug into, on both SCCs.'
        Step 'If not, fix them on the Settings page (opening it now) and click Save.'
        Start-Process "$($script:Arena.Url)/#settings"
    } else {
        Ok 'SCC management is off, so there are no SCC ports to check.'
    }
    Pause-Wizard

    Stage 'Log in to the access point'
    $script:ApReady = Connect-AP
    if (-not $script:ApReady) { $script:ToDo.Add('Patch the AP''s radio API once SSH to it works (run this again).') }
    Pause-Wizard

    Stage 'Patch the AP''s radio API'
    if ($script:ApReady) {
        $facts = Get-ApFacts
        Say "AP model $($facts['model']), firmware $($facts['firmware'])"
        $newApiHash = Get-Sha256 $PatchedApi
        $newApiKb = [math]::Ceiling((Get-Item -LiteralPath $PatchedApi).Length / 1024)
        if ($facts['sha256'] -and $facts['sha256'] -eq $newApiHash) {
            Ok 'The AP already runs this patched radio API.'
        } elseif ($facts['model'] -notmatch 'VH') {
            Warn 'This isn''t a Vivid-Hosting AP, so the patch doesn''t apply.'
            $script:ApReady = $false
        } elseif ($facts['binary'] -ne 'yes' -or $facts['init'] -ne 'yes') {
            Warn 'The radio API isn''t at /usr/bin/frc-radio-api on this firmware, so the patch can''t be installed.'
            $script:ApReady = $false
        } elseif ($facts['hostapdconf'] -ne 'yes' -or $facts['hostapdsocket'] -ne 'yes' -or -not $facts['wpacli']) {
            Warn 'This firmware doesn''t run hostapd the way the patch expects, so it would always fall back to'
            Warn 'reloading all of Wi-Fi. Leaving the stock radio API alone.'
            $script:ApReady = $false
        } elseif ($facts['freekb'] -match '^\d+$' -and [int]$facts['freekb'] -lt $newApiKb + 512) {
            Warn "The AP has $($facts['freekb']) KB of flash free and the patch needs about $($newApiKb + 512) KB."
            $script:ApReady = $false
        } else {
            $configPage = $false
            try { $configPage = [bool](Invoke-WebRequest -Uri "http://$($script:ApAddress)/configuration" -UseBasicParsing -TimeoutSec 3) } catch { }
            if (-not (Test-Path -LiteralPath $StockApiBackup)) {
                $copy = Copy-FromAP '/usr/bin/frc-radio-api' $StockApiBackup
                $saved = ($copy.ExitCode -eq 0) -and (Test-Path -LiteralPath $StockApiBackup)
                if ($saved -and $facts['sha256']) { $saved = (Get-Sha256 $StockApiBackup) -eq $facts['sha256'] }
                if (-not $saved) {
                    $copy.Lines | ForEach-Object { Note $_ }
                    Remove-Item -LiteralPath $StockApiBackup -ErrorAction SilentlyContinue
                    throw 'Couldn''t save a copy of the AP''s stock radio API; nothing on the AP was changed.'
                }
                ($facts.GetEnumerator() | Sort-Object Name | ForEach-Object { "$($_.Name)=$($_.Value)" }) |
                    Set-Content -LiteralPath (Join-Path $BackupDir 'ap-info.txt')
            }
            Ok "The AP's stock radio API is saved in $StockApiBackup"
            Say 'This swaps the AP''s radio API for the patched one and restarts the API.'
            Say 'Wi-Fi isn''t touched, so connected robots stay connected.'
            if ($configPage) {
                Warn "The AP serves a configuration page at http://$($script:ApAddress)/configuration. The patched API"
                Warn 'is built from the public code, which doesn''t have it, so that page goes away until rollback.'
                Warn 'Chap Arena doesn''t use it.'
            }
            if (Confirm-Wizard 'Install the patched radio API on the AP?') {
                if (Install-ApApi $PatchedApi) {
                    Ok 'The AP runs the patched radio API.'
                    $script:Done.Add('Installed the patched radio API on the AP (stock one saved in backup\).')
                } else {
                    $script:ApReady = $false
                    $script:ToDo.Add('Installing the patched radio API failed; see the log above.')
                }
            } else {
                $script:ApReady = $false
                $script:ToDo.Add('The AP still runs its stock radio API.')
            }
        }
    } else {
        Note 'Skipped: this PC couldn''t log in to the AP.'
    }
    Pause-Wizard

    Stage 'Field test: R1 keeps driving while R3 changes team'
    if (-not $script:ApReady) {
        Warn 'The AP still runs its stock radio API, so R1''s radio is expected to drop. This test'
        Warn 'still shows whether the wired side (switch and SCCs) left R1 alone.'
    }
    Step "Open Chap Arena ($($script:Arena.Url)), assign R1 and R3, and click Apply once."
    Note 'The first apply after Chap Arena starts redoes every station, which is expected.'
    Note 'Do it before R1 starts driving, so the real test starts from a known switch.'
    Start-Process $script:Arena.Url
    Pause-Wizard 'Press Enter once that apply is done'
    Step 'Link R1''s robot and start driving it.'
    Pause-Wizard 'Press Enter when R1 is driving'
    Clear-Stage
    Write-Host ''
    Write-Host '  Watching the field now. Lines appear when something changes.' -ForegroundColor Cyan
    Step 'Change R3''s team number in Chap Arena and click Apply.'
    Step 'Keep driving R1 for about 30 seconds after, then press Enter here.'
    Write-Host ''
    $samples = Watch-Field
    Write-Host ''
    $passed = Test-Watch $samples
    if ($script:ApReady) { Show-ApLog }
    $saw = Confirm-Wizard 'Did R1''s robot keep driving the whole time, with no comms loss on its driver station?'
    if ($passed -and $saw) {
        Ok 'R1 kept driving while R3 changed team.'
        $script:Done.Add('Field test passed: R1 kept driving while R3 changed team.')
    } else {
        $passed = $false
        Warn 'The test didn''t pass.'
        $script:ToDo.Add("The field test didn't pass; the log in $Here\logs shows why.")
    }
    Pause-Wizard

    Stage 'Keep or roll back'
    if ($passed) {
        Ok 'Everything worked, so nothing needs rolling back.'
        Note 'To undo it later, double-click Rollback-ChapArena.cmd.'
    } else {
        Say 'The AP''s stock radio API can be put back on its own. Robots at unchanged stations'
        Say 'then drop again on every apply, as before.'
        if (Test-Path -LiteralPath $StockApiBackup) {
            if (-not $script:ApReady) { $script:ApReady = Connect-AP }
            if ($script:ApReady) { Restore-ApApi }
        }
        Note 'To go back to the old Chap Arena too, double-click Rollback-ChapArena.cmd.'
    }
    Pause-Wizard

    Finish 'Done' $LogPath
} catch {
    Write-Host ''
    Warn "Stopped: $($_.Exception.Message)"
    Note "Everything up to here is saved in $LogPath"
} finally {
    Stop-Transcript | Out-Null
}
