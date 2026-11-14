param([string]$Invitation, [switch]$Interactive)
$ErrorActionPreference = 'Stop'

function Find-Invitation {
    $places = @($PSScriptRoot, (Split-Path $PSScriptRoot -Parent),
        (Join-Path $env:USERPROFILE 'Downloads'), (Join-Path $env:USERPROFILE 'Desktop'))
    $found = @($places | Where-Object { Test-Path -LiteralPath $_ } |
        ForEach-Object { Get-ChildItem -LiteralPath $_ -Filter 'remote-claude-*.json' -File -ErrorAction SilentlyContinue } |
        Sort-Object FullName -Unique)
    if ($found.Count -eq 1) {
        Write-Host "Найдено приглашение: $($found[0].FullName)"
        return $found[0].FullName
    }
    if ($found.Count -gt 1) {
        Write-Host 'Найдено несколько приглашений:'
        for ($i = 0; $i -lt $found.Count; $i++) { Write-Host "  $($i + 1). $($found[$i].FullName)" }
        $choice = Read-Host 'Введите номер своего файла'
        $index = 0
        if ([int]::TryParse($choice, [ref]$index) -and $index -ge 1 -and $index -le $found.Count) {
            return $found[$index - 1].FullName
        }
        throw 'Неверный номер.'
    }
    Write-Host 'Файл приглашения remote-claude-NAME.json не найден.'
    Write-Host 'Перетащите его мышью в это окно и нажмите Enter:'
    $path = (Read-Host).Trim().Trim('"')
    if (-not $path -or -not (Test-Path -LiteralPath $path -PathType Leaf)) { throw "Файл не найден: $path" }
    return $path
}

function Register-FolderMenu([string]$Target, [string[]]$ProxyArgs) {
    $extra = ($ProxyArgs -join ' ')
    if ($extra) { $extra += ' ' }
    $entries = @(
        @{ Key = 'RemoteClaude'; Title = 'Открыть в Remote Claude'; Args = '' },
        @{ Key = 'RemoteClaudeResume'; Title = 'Remote Claude: продолжить разговор'; Args = '--resume ' }
    )
    foreach ($parent in 'Directory\shell', 'Directory\Background\shell') {
        foreach ($entry in $entries) {
            $key = "HKCU:\Software\Classes\$parent\$($entry.Key)"
            $command = 'cmd.exe /k ""' + $Target + '" ' + $extra + $entry.Args + '"%V""'
            New-Item -Path "$key\command" -Force | Out-Null
            Set-ItemProperty -LiteralPath $key -Name '(default)' -Value $entry.Title
            Set-ItemProperty -LiteralPath $key -Name 'Icon' -Value $Target
            Set-ItemProperty -LiteralPath "$key\command" -Name '(default)' -Value $command
        }
    }
}

try {
    $InstallDir = Join-Path $env:LOCALAPPDATA 'Programs\remote-claude'
    $Source = Join-Path $PSScriptRoot 'remote-claude.exe'
    if (-not (Test-Path -LiteralPath $Source)) {
        throw 'Рядом с установщиком нет remote-claude.exe. Сначала распакуйте ZIP: правой кнопкой по архиву -> «Извлечь все», затем запустите install.cmd из распакованной папки.'
    }
    New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
    $Target = Join-Path $InstallDir 'remote-claude.exe'
    $SearchTarget = Join-Path $InstallDir 'remote-claude-rg.exe'
    try {
        Copy-Item -LiteralPath (Join-Path $PSScriptRoot 'remote-claude-rg.exe') -Destination $SearchTarget -Force
        Copy-Item -LiteralPath $Source -Destination $Target -Force
    } catch {
        throw 'Не удалось заменить файлы клиента. Закройте все окна с remote-claude и запустите установку снова.'
    }
    # Applies only to the downloaded application, not to system policy.
    Unblock-File -LiteralPath $SearchTarget -ErrorAction SilentlyContinue
    Unblock-File -LiteralPath $Target -ErrorAction SilentlyContinue
    $UserPath = [Environment]::GetEnvironmentVariable('Path', 'User')
    if ($null -eq $UserPath) { $UserPath = '' }
    if (($UserPath -split ';') -notcontains $InstallDir) {
        [Environment]::SetEnvironmentVariable('Path', ($UserPath.TrimEnd(';') + ';' + $InstallDir), 'User')
    }
    $env:Path = $InstallDir + ';' + $env:Path
    & $Target version
    if ($LASTEXITCODE -ne 0) { throw 'Application launch failed' }

    $ProfileConfig = Join-Path ([Environment]::GetFolderPath('ApplicationData')) 'remote-claude\default\config.json'
    $Enrolled = Test-Path -LiteralPath $ProfileConfig
    $ProxyArgs = @()
    if ($Interactive) {
        Write-Host ''
        Write-Host 'Подключаться к серверу через HTTP CONNECT-прокси (127.0.0.1:10809)?'
        Write-Host 'Выберите «да», если прямое соединение не работает или часто рвётся.'
        $answer = Read-Host 'Через HTTP-прокси? [д/Н]'
        if ($answer -match '^\s*(д|y)') {
            $ProxyArgs = @('--proxy')
            $port = Read-Host 'Порт HTTP CONNECT-прокси (Enter — 10809)'
            if ($port.Trim()) {
                $number = 0
                if (-not [int]::TryParse($port.Trim(), [ref]$number) -or $number -lt 1 -or $number -gt 65535) { throw "Неверный порт: $port" }
                $ProxyArgs += @('--proxy-port', "$number")
            }
        }
        if ($Enrolled -and $Invitation) {
            Write-Host 'Клиент уже зарегистрирован на этом компьютере, приглашение не используется.'
            $Invitation = ''
        } elseif (-not $Enrolled -and -not $Invitation) {
            $Invitation = Find-Invitation
        }
    }
    if ($Invitation) {
        & $Target setup @ProxyArgs $Invitation
        if ($LASTEXITCODE -ne 0) {
            if ($Interactive) { throw 'Регистрация не удалась. Проверьте интернет (и HTTP-прокси, если выбран) и запустите install.cmd ещё раз с тем же приглашением.' }
            throw 'Enrollment failed; rerun with the same invitation'
        }
        if ($Interactive) {
            $answer = Read-Host "Готово. Удалить файл приглашения $Invitation? Он больше не нужен [Д/н]"
            if ($answer -notmatch '^\s*(н|n)') { Remove-Item -LiteralPath $Invitation -Force }
        }
    }
    if ($Interactive) {
        Register-FolderMenu $Target $ProxyArgs
        Write-Host ''
        Write-Host 'Установка завершена.'
        Write-Host 'Как начать: щёлкните правой кнопкой по папке проекта (или по пустому месту внутри неё)'
        Write-Host 'и выберите «Открыть в Remote Claude». В Windows 11 пункт находится в «Показать дополнительные параметры».'
    } else {
        Write-Host 'Open a new terminal in your project and run: remote-claude .'
    }
} catch {
    if (-not $Interactive) { throw }
    Write-Host ''
    Write-Host "ОШИБКА: $($_.Exception.Message)" -ForegroundColor Red
    exit 1
}
