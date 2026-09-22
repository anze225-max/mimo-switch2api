# 安装 MiMo Switch：复制到 %LOCALAPPDATA%\MiMoSwitch，建开始菜单快捷方式，开机静默自启并启动。
# 用法：在解压出来的文件夹里右键「使用 PowerShell 运行」，或执行  powershell -ExecutionPolicy Bypass -File install.ps1
$ErrorActionPreference = 'Stop'

$src = Split-Path -Parent $MyInvocation.MyCommand.Path
$exe = Join-Path $src 'MiMoSwitch.exe'
if (-not (Test-Path $exe)) { throw "找不到 $exe，请把 install.ps1 和 MiMoSwitch.exe 放在一起。" }

$dst = Join-Path $env:LOCALAPPDATA 'MiMoSwitch'
New-Item -ItemType Directory -Force -Path $dst | Out-Null
Copy-Item $exe (Join-Path $dst 'MiMoSwitch.exe') -Force

$shell = New-Object -ComObject WScript.Shell
$programs = [Environment]::GetFolderPath('Programs')
$lnk = $shell.CreateShortcut((Join-Path $programs 'MiMo Switch.lnk'))
$lnk.TargetPath = Join-Path $dst 'MiMoSwitch.exe'
$lnk.WorkingDirectory = $dst
$lnk.Description = 'MiMo 桌面端免费额度本地端点'
$lnk.Save()

# 开机静默自启由程序自己注册：它会确认这个 exe 是 GUI 子系统，绝不会启动一个会闪黑框的版本。
& (Join-Path $dst 'MiMoSwitch.exe') autostart on

Start-Process -FilePath (Join-Path $dst 'MiMoSwitch.exe')
Write-Host '安装完成：系统托盘会出现 MiMo 图标。首次运行会弹出小米登录窗口，登录后即可使用。'
Write-Host '控制面板： http://127.0.0.1:7864/'
