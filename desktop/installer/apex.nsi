; Apex RMM Desktop installer (NSIS 3). Per-user install: no administrator rights needed.
; Built by ../build.ps1:  makensis /DVERSION=1.2.3 /DVERSIONNUM=1.2.3 installer/apex.nsi
Unicode true
ManifestDPIAware true
SetCompressor /SOLID lzma

!ifndef VERSION
  !define VERSION "0.0.0"
!endif
!ifndef VERSIONNUM
  !define VERSIONNUM "0.0.0"
!endif
!define APPNAME "Apex RMM"
!define EXE "ApexRMM.exe"
!define UNINSTKEY "Software\Microsoft\Windows\CurrentVersion\Uninstall\ApexRMMDesktop"

Name "${APPNAME}"
OutFile "..\dist\ApexRMM-Desktop-Setup-${VERSION}.exe"
RequestExecutionLevel user
InstallDir "$LOCALAPPDATA\Programs\Apex RMM"
InstallDirRegKey HKCU "${UNINSTKEY}" "InstallLocation"
BrandingText "${APPNAME} ${VERSION}"

VIProductVersion "${VERSIONNUM}.0"
VIAddVersionKey "ProductName" "${APPNAME}"
VIAddVersionKey "FileDescription" "${APPNAME} Desktop Setup"
VIAddVersionKey "FileVersion" "${VERSION}"
VIAddVersionKey "ProductVersion" "${VERSION}"
VIAddVersionKey "LegalCopyright" "Copyright (c) 2026 Cade Hardy (Hardy Networks)"
VIAddVersionKey "CompanyName" "Hardy Networks"

!include "MUI2.nsh"
!include "x64.nsh"
!define MUI_ICON "..\build\icon.ico"
!define MUI_UNICON "..\build\icon.ico"
!define MUI_ABORTWARNING
!define MUI_FINISHPAGE_RUN "$INSTDIR\${EXE}"
!define MUI_FINISHPAGE_RUN_TEXT "Start ${APPNAME}"
!insertmacro MUI_PAGE_WELCOME
!insertmacro MUI_PAGE_INSTFILES
!insertmacro MUI_PAGE_FINISH
!insertmacro MUI_UNPAGE_CONFIRM
!insertmacro MUI_UNPAGE_INSTFILES
!insertmacro MUI_LANGUAGE "English"

Function .onInit
  ${IfNot} ${RunningX64}
  ${AndIfNot} ${IsNativeARM64}
    MessageBox MB_ICONSTOP "${APPNAME} needs 64-bit Windows 10 or 11."
    Abort
  ${EndIf}
FunctionEnd

Section "Install"
  ; close a running copy so its files can be replaced
  nsExec::Exec 'taskkill /IM ${EXE} /F'
  Sleep 800

  SetOutPath "$INSTDIR"
  ${If} ${IsNativeARM64}
    File "/oname=${EXE}" "..\dist\ApexRMM-arm64.exe"
  ${Else}
    File "/oname=${EXE}" "..\dist\ApexRMM-amd64.exe"
  ${EndIf}
  WriteUninstaller "$INSTDIR\uninstall.exe"

  CreateShortcut "$SMPROGRAMS\${APPNAME}.lnk" "$INSTDIR\${EXE}"
  IfSilent +2
    CreateShortcut "$DESKTOP\${APPNAME}.lnk" "$INSTDIR\${EXE}"

  WriteRegStr HKCU "${UNINSTKEY}" "DisplayName" "${APPNAME}"
  WriteRegStr HKCU "${UNINSTKEY}" "DisplayVersion" "${VERSION}"
  WriteRegStr HKCU "${UNINSTKEY}" "Publisher" "Hardy Networks"
  WriteRegStr HKCU "${UNINSTKEY}" "URLInfoAbout" "https://github.com/hardynetworks/apex-rmm"
  WriteRegStr HKCU "${UNINSTKEY}" "DisplayIcon" "$INSTDIR\${EXE}"
  WriteRegStr HKCU "${UNINSTKEY}" "InstallLocation" "$INSTDIR"
  WriteRegStr HKCU "${UNINSTKEY}" "UninstallString" '"$INSTDIR\uninstall.exe"'
  WriteRegStr HKCU "${UNINSTKEY}" "QuietUninstallString" '"$INSTDIR\uninstall.exe" /S'
  WriteRegDWORD HKCU "${UNINSTKEY}" "NoModify" 1
  WriteRegDWORD HKCU "${UNINSTKEY}" "NoRepair" 1
  WriteRegDWORD HKCU "${UNINSTKEY}" "EstimatedSize" 9000

  ; silent install = an in-app update: start the app again
  IfSilent 0 +2
    Exec '"$INSTDIR\${EXE}" --background'
SectionEnd

Section "Uninstall"
  nsExec::Exec 'taskkill /IM ${EXE} /F'
  Sleep 800
  Delete "$INSTDIR\${EXE}"
  Delete "$INSTDIR\uninstall.exe"
  RMDir "$INSTDIR"
  Delete "$SMPROGRAMS\${APPNAME}.lnk"
  Delete "$DESKTOP\${APPNAME}.lnk"
  DeleteRegValue HKCU "Software\Microsoft\Windows\CurrentVersion\Run" "${APPNAME}"
  DeleteRegKey HKCU "${UNINSTKEY}"
  ; settings and the WebView2 cache are in %LOCALAPPDATA%\ApexRMM; remove them too
  MessageBox MB_YESNO|MB_ICONQUESTION "Also remove your ${APPNAME} settings and sign-in data?" /SD IDNO IDNO +2
    RMDir /r "$LOCALAPPDATA\ApexRMM"
SectionEnd
