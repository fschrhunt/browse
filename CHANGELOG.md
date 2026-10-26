# browse releases

## Unreleased

- Releases go through Actions: `scripts/release.sh` names the changelog section, then asks
  Actions to tag. Each release has archives for Linux and macOS with checksums and build
  provenance, and the install and uninstall scripts.
- The installer checks the archive against the release's checksums, and takes
  `--version vX.Y.Z`. It installs with `curl -fsSL https://fschrhunt.com/browse/install.sh | sh`.
- `browse version` prints the version.

## v0.1.0 · 2026-10-26

- The first release: a browser in the terminal for people and agents, with tabs, an address
  box, persistent logins, agent commands, and `browse show` for a pane beside the conversation.
