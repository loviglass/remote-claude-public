@echo off
rem Double-click installer for Windows. Text is in install.ps1; this file stays ASCII.
powershell.exe -NoProfile -ExecutionPolicy Bypass -File "%~dp0install.ps1" -Interactive %*
echo.
pause
