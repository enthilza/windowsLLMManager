@echo off
setlocal
powershell.exe -NoLogo -NoProfile -ExecutionPolicy Bypass -File "%~dp0rotate-token.ps1"
set "EXIT_CODE=%errorlevel%"
echo.
if not "%EXIT_CODE%"=="0" echo Token rotation failed with exit code %EXIT_CODE%.
pause
exit /b %EXIT_CODE%
