@echo off
setlocal
pushd "%~dp0" || exit /b 1
powershell.exe -NoLogo -NoProfile -ExecutionPolicy Bypass -File "%~dp0scripts\release.ps1" %*
set "EXIT_CODE=%errorlevel%"
popd
exit /b %EXIT_CODE%
