@echo off
rem Goes back to the Chap Arena that Setup-ChapArena.cmd moved from and puts the AP's stock radio API back, asking first.
powershell.exe -NoProfile -ExecutionPolicy Bypass -File "%~dp0Setup-ChapArena.ps1" -Rollback
pause
