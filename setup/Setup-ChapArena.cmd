@echo off
rem Starts the setup wizard in Setup-ChapArena.ps1. Windows blocks .ps1 files by default, so this lets just this one run.
if not exist "%~dp0chap-arena.exe" (
  echo Extract the whole zip into a folder first, then run Setup-ChapArena.cmd from there.
  pause
  exit /b 1
)
powershell.exe -NoProfile -ExecutionPolicy Bypass -File "%~dp0Setup-ChapArena.ps1" %*
pause
