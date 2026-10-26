# browse releases

## Unreleased

- The first release: a browser in the terminal for people and agents. Tabs sit in the middle of
  the bar, with back, forward and reload on the left; addresses and searches are typed in one
  eye-level box; a blue band under the bar shows a page loading. Logins persist, agents drive
  the same browser with commands, and `browse show` opens it in a pane beside the conversation.
- Installs with `curl -fsSL https://fschrhunt.com/browse/install.sh | sh` on Linux and macOS,
  from archives checked against the release's checksums. `browse version` prints the version.
- Updating restarts the running browser on its own, keeping logins.
