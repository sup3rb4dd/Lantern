@echo off
rem Builds Lantern.exe and a ready-to-copy drive folder in dist\Lantern (needs Go from https://go.dev/dl)
setlocal
cd /d "%~dp0"
if exist dist rmdir /s /q dist
mkdir dist\Lantern\lantern\web dist\Lantern\lantern\source
pushd launcher
set GOOS=windows
set GOARCH=amd64
set CGO_ENABLED=0
go build -trimpath -ldflags "-s -w" -o ..\dist\Lantern\Lantern.exe . || (popd & exit /b 1)
popd
xcopy /e /i /q web dist\Lantern\lantern\web >nul
copy /y launcher\*.go dist\Lantern\lantern\source >nul
copy /y launcher\go.mod dist\Lantern\lantern\source >nul
copy /y package\README.txt dist\Lantern\README.txt >nul
echo Built dist\Lantern - copy its contents to the root of an exFAT drive.
