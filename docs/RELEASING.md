# Releasing CogOS

## Prerequisites
- All CI checks passing on main

## Steps

1. Verify recent PR titles follow conventional-commit format — they become the auto-generated release notes (via `gh release create --generate-notes`; see CONTRIBUTING.md). Optionally amend the historical-entries section of CHANGELOG.md only if there is a specific note that does not fit a PR title.
2. Commit: `git commit -am "release: prepare v0.x.y"`
3. Tag: `git tag v0.x.y`
4. Push: `git push origin main --tags`
5. GitHub Actions builds binaries and creates the release automatically

## What happens automatically
- CI runs (build, test, lint)
- Cross-compiled binaries for:
  - linux/amd64, linux/arm64
  - darwin/amd64, darwin/arm64 (macOS Intel + Apple Silicon)
  - windows/amd64, windows/arm64
- SHA-256 checksums generated
- `checksums.txt` signed keylessly with Sigstore/cosign (see
  [release-signing.md](release-signing.md) for what's signed and how to verify)
- GitHub Release created with binaries attached and auto-generated release notes

## Installing from a release
```sh
# macOS (Apple Silicon)
curl -L https://github.com/myrgic/cogos/releases/latest/download/cogos-darwin-arm64 -o cogos
chmod +x cogos
./cogos serve

# Linux
curl -L https://github.com/myrgic/cogos/releases/latest/download/cogos-linux-amd64 -o cogos
chmod +x cogos
./cogos serve

# Windows
# Download cogos-windows-amd64.exe from the latest release
cogos.exe serve
```

## Installing on Windows (developer preview)

The Windows binaries are a developer preview: they are **not yet code-signed**, so Windows SmartScreen will warn before the first run. The binary itself is the same PE32+ executable that CI produces from the same source tree as the macOS and Linux targets.

### Download

Pick the right architecture for your machine (most users want `amd64`; `arm64` is for Windows-on-ARM devices such as Surface Pro X / Copilot+ PCs).

Using PowerShell:
```powershell
# amd64 (x86-64)
Invoke-WebRequest -Uri https://github.com/myrgic/cogos/releases/latest/download/cogos-windows-amd64.exe -OutFile cogos.exe

# arm64
Invoke-WebRequest -Uri https://github.com/myrgic/cogos/releases/latest/download/cogos-windows-arm64.exe -OutFile cogos.exe
```

Or download manually from the [latest release](https://github.com/myrgic/cogos/releases/latest) and rename to `cogos.exe`.

Verify the SHA-256 against `checksums.txt` from the same release:
```powershell
Get-FileHash -Algorithm SHA256 cogos.exe
```

### SmartScreen on first run

Because the binary is unsigned, Windows Defender SmartScreen will show a blue "Windows protected your PC" dialog the first time you run it. To proceed:

1. Click **More info** in the dialog.
2. Click **Run anyway**.

This is a one-time prompt per binary. Code signing will remove this step in a future release.

### Install to PATH

The recommended location for per-user installs is `%LOCALAPPDATA%\cogos\`, which does not require admin privileges. This refuses to overwrite a binary a running `cogos.exe` daemon is executing, using the shared guard at `scripts/lib/refuse-if-running.ps1` (the same one `make install`'s bash counterpart uses on macOS/Linux — see that file's header for why Windows has its own implementation rather than sharing the bash one) — fetched here rather than inlined so this doc can't drift from the canonical check the way the bash copies once did (see `cog://mem/working/2026-07-30-self-review-spike/RETRO-486.md`):

```powershell
# Create the install directory
$InstallDir = "$env:LOCALAPPDATA\cogos"
New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null

# Refuse to overwrite a running daemon's binary (throws, doesn't `exit` --
# safe to paste into an interactive session; see refuse-if-running.ps1)
Invoke-WebRequest -Uri https://raw.githubusercontent.com/myrgic/cogos/main/scripts/lib/refuse-if-running.ps1 -OutFile "$env:TEMP\refuse-if-running.ps1"
. "$env:TEMP\refuse-if-running.ps1"
Assert-CogosNotRunning -Target "$InstallDir\cogos.exe"

# Move the binary
Move-Item -Force .\cogos.exe "$InstallDir\cogos.exe"

# Add to the User PATH (persists across sessions; no admin needed)
$UserPath = [Environment]::GetEnvironmentVariable("Path", "User")
if ($UserPath -notlike "*$InstallDir*") {
    [Environment]::SetEnvironmentVariable("Path", "$UserPath;$InstallDir", "User")
}
```

Set `$env:ALLOW_RUNNING_INSTALL = '1'` to override, if you mean it (e.g. a deliberate in-place upgrade you'll restart the daemon after).

Open a **new** PowerShell window so the updated PATH is picked up.

### Sanity check

Verify the install:
```powershell
cogos.exe version
```
You should see a line like `cogos version=dev build=<timestamp>` (or the real version string on a tagged release).

To confirm the daemon comes up and the HTTP API responds, in one terminal:
```powershell
cogos.exe serve
```
and in a second terminal:
```powershell
cogos.exe health
```
Expected output: `healthy`.

### Stretch goal: Windows Service / SCM integration

Running `cogos` as a background Windows Service (via the Service Control Manager) is not included in the v0.x developer preview. For now, run `cogos serve` in a foreground terminal, or wrap it with Task Scheduler / NSSM if you need it to start at login. First-class `cogos install-service` support is a planned follow-up and will land in a later release.
