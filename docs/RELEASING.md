# Releasing CogOS

## Prerequisites
- All CI checks passing on main
- CHANGELOG.md updated with changes since last release

## Steps

1. Update CHANGELOG.md — move "Unreleased" items under a new version header
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
- GitHub Release created with binaries attached and auto-generated release notes

## Installing from a release
```sh
# macOS (Apple Silicon)
curl -L https://github.com/cogos-dev/cogos/releases/latest/download/cogos-darwin-arm64 -o cogos
chmod +x cogos
./cogos serve

# Linux
curl -L https://github.com/cogos-dev/cogos/releases/latest/download/cogos-linux-amd64 -o cogos
chmod +x cogos
./cogos serve

# Windows
# Download cogos-windows-amd64.exe from the latest release
cogos.exe serve
```
