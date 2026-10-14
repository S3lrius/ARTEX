@echo off
rem Switch the console to UTF-8; otherwise non-ASCII text in this file is garbled on a GBK terminal.
chcp 65001 >nul 2>&1
rem ARTEX supervisor (Windows)
rem
rem Usage:
rem   start.bat                  foreground (Ctrl-C to stop)
rem   start.bat -addr :9000      extra arguments are passed through to artex
rem
rem It does one thing: run artex.exe, then decide from the exit code whether to start it again.
rem
rem   0      clean user stop      -> leave the loop
rem   75     program asked to restart -> rerun immediately
rem   other  crash                 -> back off and rerun (1->2->4... up to 60 seconds)
rem
rem This script does not download or replace the binary. See the top of start.sh.
rem
setlocal enabledelayedexpansion
cd /d "%~dp0"

set "BIN=artex.exe"
if not exist "%BIN%" (
	echo [artex] executable not found: %BIN% 1>&2
	exit /b 1
)

set "RESTART_CODE=75"
set "MAX_DELAY=60"
set /a delay=1

:loop
"%BIN%" %*
set "code=!ERRORLEVEL!"

if "!code!"=="0" (
	echo [artex] exited normally
	exit /b 0
)

if "!code!"=="%RESTART_CODE%" (
	echo [artex] restart requested...
	set /a delay=1
	goto loop
)

echo [artex] abnormal exit ^(code=!code!^), restarting in !delay!s 1>&2
rem timeout fails when the console is redirected; fall back to ping (N seconds needs N+1 pings).
set /a pings=!delay!+1
ping -n !pings! 127.0.0.1 >nul 2>&1
set /a delay=!delay!*2
if !delay! gtr %MAX_DELAY% set /a delay=%MAX_DELAY%
goto loop
