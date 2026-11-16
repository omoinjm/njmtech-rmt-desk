Unicode true

####
## rmt-desk installer (NSIS). Built by `wails build -nsis`, which generates
## wails_tools.nsh next to this file and passes -DARG_WAILS_<ARCH>_BINARY.
## rmtd.exe (the host service) must already be in ..\..\bin\ (the release
## workflow builds it there first); override with -DRMTD_BINARY=path.
####
!define INFO_PROJECTNAME "rmt-desk"
!define INFO_COMPANYNAME "rmt-desk"
!define INFO_PRODUCTNAME "rmt-desk"
!define UNINST_KEY_NAME  "rmt-desk"
!ifndef RMTD_BINARY
  !define RMTD_BINARY "..\..\bin\rmtd.exe"
!endif

!include "wails_tools.nsh"
!include "LogicLib.nsh"
!include "StrFunc.nsh"
${StrStr}

VIProductVersion "${INFO_PRODUCTVERSION}.0"
VIFileVersion    "${INFO_PRODUCTVERSION}.0"
VIAddVersionKey "CompanyName"     "${INFO_COMPANYNAME}"
VIAddVersionKey "FileDescription" "${INFO_PRODUCTNAME} Installer"
VIAddVersionKey "ProductVersion"  "${INFO_PRODUCTVERSION}"
VIAddVersionKey "FileVersion"     "${INFO_PRODUCTVERSION}"
VIAddVersionKey "LegalCopyright"  "${INFO_COPYRIGHT}"
VIAddVersionKey "ProductName"     "${INFO_PRODUCTNAME}"

ManifestDPIAware true

!include "MUI.nsh"

!define MUI_ICON "..\icon.ico"
!define MUI_UNICON "..\icon.ico"
!define MUI_ABORTWARNING
!define MUI_WELCOMEPAGE_TEXT "This installs rmt-desk: remote desktop between your own devices over your Tailscale network.$\r$\n$\r$\nInstalling does not open this computer to remote access. You turn that on in the app, and choose who may connect."
# Launch the app as the signed-in user, not elevated like this installer.
!define MUI_FINISHPAGE_RUN
!define MUI_FINISHPAGE_RUN_TEXT "Start rmt-desk"
!define MUI_FINISHPAGE_RUN_FUNCTION LaunchAsUser

!insertmacro MUI_PAGE_WELCOME
!insertmacro MUI_PAGE_DIRECTORY
!insertmacro MUI_PAGE_INSTFILES
!insertmacro MUI_PAGE_FINISH
!insertmacro MUI_UNPAGE_CONFIRM
!insertmacro MUI_UNPAGE_INSTFILES
!insertmacro MUI_LANGUAGE "English"

Name "${INFO_PRODUCTNAME}"
OutFile "..\..\bin\${INFO_PROJECTNAME}-${ARCH}-setup.exe"
InstallDir "$PROGRAMFILES64\${INFO_PRODUCTNAME}"
ShowInstDetails show

Var ServiceWasRunning

Function .onInit
  !insertmacro wails.checkArchitecture
FunctionEnd

Function LaunchAsUser
  Exec '"$WINDIR\explorer.exe" "$INSTDIR\${PRODUCT_EXECUTABLE}"'
FunctionEnd

Section
  !insertmacro wails.setShellContext
  !insertmacro wails.webview2runtime

  # Upgrade: the service and app lock their executables.
  StrCpy $ServiceWasRunning "0"
  ${If} ${FileExists} "$INSTDIR\rmtd.exe"
    nsExec::ExecToStack 'sc.exe query rmtd'
    Pop $0
    Pop $1
    ${StrStr} $2 $1 "RUNNING"
    ${If} $0 == 0
    ${AndIf} $2 != ""
      StrCpy $ServiceWasRunning "1"
      DetailPrint "Stopping the rmt-desk host service"
      nsExec::ExecToLog '"$INSTDIR\rmtd.exe" stop'
      Sleep 1500
    ${EndIf}
  ${EndIf}
  nsExec::Exec 'taskkill.exe /IM ${PRODUCT_EXECUTABLE}'
  Sleep 500

  SetOutPath $INSTDIR
  !insertmacro wails.files
  File "/oname=rmtd.exe" "${RMTD_BINARY}"

  CreateShortcut "$SMPROGRAMS\${INFO_PRODUCTNAME}.lnk" "$INSTDIR\${PRODUCT_EXECUTABLE}"

  ${If} $ServiceWasRunning == "1"
    DetailPrint "Restarting the rmt-desk host service"
    nsExec::ExecToLog '"$INSTDIR\rmtd.exe" start'
  ${EndIf}

  !insertmacro wails.writeUninstaller
SectionEnd

Section "uninstall"
  !insertmacro wails.setShellContext

  nsExec::Exec 'taskkill.exe /IM ${PRODUCT_EXECUTABLE}'
  ${If} ${FileExists} "$INSTDIR\rmtd.exe"
    DetailPrint "Removing the rmt-desk host service"
    nsExec::ExecToLog '"$INSTDIR\rmtd.exe" stop'
    nsExec::ExecToLog '"$INSTDIR\rmtd.exe" uninstall'
    Sleep 1000
  ${EndIf}

  # The host's tailnet key, password hash and audit log. Keeping them lets a
  # reinstall pick up where it left off; removing them signs this computer
  # out (also remove it from the Tailscale admin console).
  MessageBox MB_YESNO|MB_ICONQUESTION "Also remove this computer's rmt-desk sign-in, settings and audit log?" /SD IDNO IDNO keep
    RMDir /r "$COMMONAPPDATA\rmt-desk"
  keep:

  RMDir /r "$AppData\${PRODUCT_EXECUTABLE}" # WebView2 data
  RMDir /r $INSTDIR
  Delete "$SMPROGRAMS\${INFO_PRODUCTNAME}.lnk"

  !insertmacro wails.deleteUninstaller
SectionEnd
