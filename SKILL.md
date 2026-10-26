---
name: browse
description: A real browser driven from the terminal. Use it to read and use websites, including ones the person is signed in to. It can also show a page to the person in a pane beside the conversation.
---

`browse` drives one Chrome that keeps running between commands. Logins
persist, so sites the person signed in to stay signed in for you.

## The loop

1. `browse open <url>` opens a new tab; `browse goto <url>` loads one in the current tab.
2. `browse snapshot` lists the page's elements, one per line, such as
   `@e5 textbox "Email" value=""`.
3. Act on refs: `browse click e5`, `browse fill e5 "text"`, `browse select e7 "Large"`.
   `browse press Enter` and `browse type "text"` act where the focus is.
4. Each action waits for the page to settle and prints the tab, title, and URL.
   Snapshot again after anything that changes the page.

Read with `browse text` (or `browse text e12` for one element), `browse shot page.png`,
or `browse eval "<js>"`.

## When the page answers

- A tab the page opens becomes the active tab, and the action says so.
- A confirm or prompt stays open until you run `browse dialog accept` or
  `browse dialog dismiss`. Ask the person before accepting anything destructive.
- `browse wait "Order placed"` waits for text to appear.

## The person

`browse show` opens the browser in a pane beside the conversation, where the
person can click and type. If the terminal cannot be split, ask them to split it
and run `browse view`.

The person may be browsing too, which changes the active tab. Pass the tab
`browse open` printed with `--tab` on every command, as in `browse --tab t2 snapshot`.

Run `browse help` for every command.
