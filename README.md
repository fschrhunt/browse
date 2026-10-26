# browse

A browser in your terminal, for people and agents.

Type `browse` and a real Chrome opens in the terminal: tabs, back and forward,
an address box that searches or goes. Agents drive the same browser with
commands (`browse open`, `browse snapshot`, `browse click e5`) and can bring a
page up beside the conversation with `browse show`. Logins persist, so a site
you signed in to is signed in for your agents too.

It runs on Linux and macOS, in terminals that show images with the kitty
graphics protocol: Ghostty, kitty, and WezTerm, and tmux inside them. Over
SSH it sends only what changed on the page, and copying puts text on your
own clipboard.

## Install

    curl -fsSL https://fschrhunt.com/browse/install.sh | sh

This puts `browse` in `~/.local/bin`, Chrome's headless shell in
`~/.local/share/browse`, and the agent skill in each agent's skills folder.
`browse skill <folder>` adds the skill to an agent browse does not know.

## Uninstall

    curl -fsSL https://fschrhunt.com/browse/uninstall.sh | sh

This removes everything browse installed or made, including its logins.

## Use

    browse                 open the browser
    browse example.com     open a page
    browse help            every command and key

## Develop

The bar and the address box are `ui/index.html`, drawn by Chrome in hidden
tabs and shown over the page as pictures. `go test ./...` drives a real
Chrome. `scripts/screens.sh` takes pictures of the browser in kitty on a
virtual display, through the scenarios a person goes through, into
`dist/screens/sheet.png`; CI attaches that sheet to every run. Look at it
after any change to how the browser looks or responds.
