# Code signing (OSSign)

Apex RMM Desktop releases are signed through [OSSign](https://github.com/ossign), the Open Source
Codesigning Initiative.

## How it fits together

1. A maintainer pushes a tag `desktop-vX.Y.Z`.
2. [`desktop-release.yml`](../.github/workflows/desktop-release.yml) builds the installer,
   publishes it **unsigned** on a GitHub release (so it's usable right away), then sends a signing
   request to OSSign with `ossign/actions/workflow/dispatch` (using the `OSSIGN_USER` /
   `OSSIGN_TOKEN` secrets).
3. OSSign's signing repository for this project builds the same tag, signs it and publishes the
   signed files.
4. [`desktop-sign-wait.yml`](../.github/workflows/desktop-sign-wait.yml) checks back every 20 minutes
   (the wait timer on the `Signatures` environment). When the signed files are ready it replaces the
   unsigned installer on the release.

The app's built-in updater only installs updates that carry a valid Authenticode signature;
unsigned releases just open the release page instead.

## Build steps for the signing repository

The build runs on `windows-latest` with Go (version from `desktop/go.mod`); NSIS is installed
automatically if missing. Version = the tag without `desktop-v`.

```powershell
cd desktop

# 1. build the app for x64 and ARM64  ->  dist/ApexRMM-amd64.exe, dist/ApexRMM-arm64.exe
./build.ps1 -Version 1.2.3 -Step app

# 2. sign both (pecoff):  dist/ApexRMM-amd64.exe  dist/ApexRMM-arm64.exe   (in place)

# 3. package the signed exes  ->  dist/ApexRMM-Desktop-Setup-1.2.3.exe
./build.ps1 -Version 1.2.3 -Step installer

# 4. sign the installer (pecoff):  dist/ApexRMM-Desktop-Setup-1.2.3.exe
```

Publish **`ApexRMM-Desktop-Setup-X.Y.Z.exe`** under that exact name. The app's updater looks for
that file name on releases tagged `desktop-vX.Y.Z`.

## One-time setup in this repository

After OSSign approves the project:

1. **Settings → Secrets and variables → Actions → New repository secret**: add `OSSIGN_USER` and
   `OSSIGN_TOKEN` (repository secrets, not environment secrets).
2. **Settings → Environments → New environment** named `Signatures`, tick **Wait timer** and set it
   to **20** minutes, then **Save protection rules**.

Until the secrets exist, releases are simply published unsigned.
