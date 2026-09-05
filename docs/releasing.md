# Release process

Official releases run in GitHub Actions through the shared OpenClaw release workflow. The workflow freezes the protected `main` commit, creates the annotated version tag, builds every archive, signs and notarizes the macOS binaries with organization credentials, verifies the unpublished draft on both macOS architectures, publishes the verified bytes, and dispatches the Homebrew update.

## Dispatch

The changelog section for the version must be dated before dispatch:

```bash
gh workflow run release-unified.yml \
  --repo openclaw/wacrawl \
  --ref main \
  -f version=X.Y.Z
```

Tags, Developer ID and App Store Connect credentials, draft publication, artifact verification, and Homebrew handoff belong to the workflow. `make release VERSION=vX.Y.Z` deliberately refuses local publication and prints the official command.

## Local verification

Local builds and snapshots require no signing credentials:

```bash
make build
make snapshot
make verify-release VERSION=vX.Y.Z
```

They are suitable for development and cross-platform CI, but they are not official signed macOS artifacts.

The release publishes archives for:

```text
darwin/amd64
darwin/arm64
linux/amd64
linux/arm64
windows/amd64
windows/arm64
```

Starting with v0.3.10, macOS binaries require macOS 13 or newer. v0.3.9 is the last release supporting macOS 12.

The Homebrew formula lives in the [`openclaw/homebrew-tap`](https://github.com/openclaw/homebrew-tap/blob/main/Formula/wacrawl.rb) repository.
