@echo off
cd /d "%~dp0"
title workbuddy2api
echo ============================================
echo  workbuddy2api  http://localhost:7863
echo  Close this window to stop the service.
echo ============================================
wb2api.exe -config config.json
echo.
echo Service exited.
pause
