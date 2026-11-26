# rex-glance

Jump onto another machine's screen from a [Rex](https://www.superlogical.com/rex) pane, fast, to do something by hand.

## What it's for

The moments when a terminal isn't enough and you need the screen for a minute: a stuck dialog to click, a login window, a setting that only has a GUI, a quick look at what's showing. You open it, do the thing, and close it.

It is **not** a remote desktop to work in all day, and not for video. For that, use a real screen-sharing app ([rootshell](https://github.com/kitknox/rootshell), Screen Sharing.app).

## What it values

**Latency over looks.** Clicks and keys go out the moment you make them, and only the parts of the screen that really changed are sent. There's no HEVC, no audio and no smoothing. On a slow link the picture drops to a lower resolution rather than falling behind.

**Nothing to install on the far side.** rex-glance connects to the screen-sharing server the machine already runs:

- **macOS:** Screen Sharing, built in.
- **Linux:** wayvnc (Wayland) or x11vnc (X11).

**It goes where Rex goes.** It's a program that runs in a Rex pane and draws with the kitty graphics protocol, so any Rex app shows it, on a Mac or an iPad. No change to Rex is needed.

## Install

You need Go and Rex. rex-glance runs on the machine that runs your Rex server.

```sh
go build -o ~/.local/bin/rex-glance .
ln -s "$PWD/glance.lua" ~/.config/rex/glance.lua
```

Then add this to `~/.config/rex/init.lua`, naming your machines:

```lua
pcall(function()
  require("glance").setup{
    machines = { { name = "This Mac", host = "localhost" }, "linuxbox" },  -- ssh or tailnet names
  }
end)
```

Run `rex config reload`. **Glance at…** is now in Rex's palette.

## Setting up the machines

**A Mac:** turn on System Settings → General → Sharing → Screen Sharing. rex-glance logs in with your macOS username and password.

**Linux with wayvnc:** wayvnc must allow logins with your account password. In `~/.config/wayvnc/config`:

```ini
enable_auth=true
enable_pam=true
```

Then restart wayvnc. Without `enable_pam`, wayvnc checks the `username` and `password` in its own config instead of your account's. On Debian, wayvnc needs no PAM file of its own: the default `other` service uses your normal login.

wayvnc is often told to listen only on the tailnet address (`address=100.x.y.z`). That's fine: rex-glance tries an SSH tunnel to the machine's localhost first, and falls back to connecting directly over the tailnet, which WireGuard already encrypts.

`rex-glance reach <machine>` checks a machine is reachable, without logging in.

## Use

In Rex, **Glance at…** picks a machine and opens its screen:

- **Usually** in a floating window over the tab you're in.
- **In tr3x's T3 Code session**, or anywhere a floater can't go (a session with no tabs), in a session of its own, which closes itself when you quit.
- **On the iPad,** Rex 0.1.1 or later asks which machine in the palette, like the Mac. Rex asks palette questions on every connected app and the first answer wins, so for an app that can't show them, list its platform in `ask_in_window` (such as `{ ios = true }`) and the window opens straight away and asks there.

The first time, it asks for the password with typing hidden, and offers to keep it in the Keychain. It saves the password only after a login works, so a typo is never stored.

### Keys

| Keys | |
| --- | --- |
| Typing, Ctrl-C, arrows, Tab, Escape… | Go to the machine as typed |
| **Ctrl+Option+key** | On a Mac, that key with ⌘: Ctrl+Option+Space is Spotlight, Ctrl+Option+C is copy. Rex keeps ⌘ itself for its own shortcuts. On Linux, Ctrl+Alt stays Ctrl+Alt |
| **Ctrl-]** | Quit, the way telnet does |

The mouse works as you'd expect: clicks, drags, right-click, the scroll wheel and hover. Rex reports the pointer per pixel, so a click lands where you point.

### From the command line

```sh
rex-glance look <machine>               # the screen in this pane
rex-glance look <machine> --fast        # draw at 1×: less sharp, about a quarter of the data
rex-glance look <machine> --ask         # ask for the password even if one is saved
rex-glance look <machine> --snapshot out.png
rex-glance look --choose Name=host …    # ask in the window which machine
rex-glance reach <machine>              # can its screen sharing be reached, and how
rex-glance bench <machine>              # first frame, encodings, input-to-update time
rex-glance bench <machine> --rects      # watch every rectangle the server sends
rex-glance probe                        # what this terminal can do
```

## How it works

- **The route:** RFB (VNC), through an SSH tunnel to the machine's localhost (`ssh -W`, no ports forwarded), or directly over the tailnet when the server listens only there.
- **The login:** Apple Remote Desktop's Diffie-Hellman scheme (security type 30). macOS Screen Sharing requires it and wayvnc offers it too, so one login covers Macs and Linux.
- **The picture:** the screen is decoded (ZRLE, Zlib, CopyRect, Raw) into memory and drawn with kitty graphics, at twice the pane's point size and fitted to its cells, so text stays sharp on Retina screens. Each drawn pixel averages the screen pixels it covers, so thin strokes survive the shrinking.

## How it stays fast

- **The server's stream never waits for the screen.** Each update is applied and the next one requested at once. Drawing runs on its own and always paints the newest picture, so a slow link shows fewer frames, never older ones.
- **Only real change is sent.** Changes are tracked on 32-pixel tiles, and a tile is redrawn only if its pixels differ from what was last drawn. This matters for macOS Screen Sharing, which reports most of a window as changed for a typed letter: on a 1920×1080 Mac it sent two-thirds of the screen about six times a second while someone typed, which was 2–3 seconds of lag on an iPad until this check. Now typing on a Mac is as quick as on Linux.
- **It adapts.** If drawing keeps taking more than a quarter of a second, it drops from 2× to 1×, and the status line says so.

## When something's wrong

- **Every session is logged** to `~/.local/state/rex-glance/last.log`: update sizes and timings, slow draws, stalls and errors. Read it first.
- **"login refused"** with a saved password: run `rex-glance look <machine> --ask` to type it again. On Linux, check `enable_pam=true` (above).
- **"can't reach … screen sharing"**: `rex-glance reach <machine>` shows what each route said.
- **Slow on one machine:** `rex-glance bench <machine> --rects` shows what its server is sending.
- **Glance at… missing from the palette:** `glance.lua` didn't load, and Rex loads it inside `pcall`, which hides the error. Running `go test ./...` also checks that it compiles.
- **A new build doesn't seem to change anything:** an open Glance keeps running the old program. Quit it and start a new one.

## Plan

- ✓ Look, poke, and Glance at… from the palette, on a Mac and an iPad
- ✓ ⌘ for Macs (Ctrl+Option)
- ✓ Fast on macOS Screen Sharing (only real change is sent)
- **Next:** zoom around the pointer for small screens; 16-bit colour for slow links; Windows machines

## Credit

The RFB and Apple Screen Sharing details follow [rootshell-vnc](https://github.com/kitknox/rootshell-vnc) by Kit Knox (MIT), the VNC client inside rootshell, which is also where the Ctrl+Option-as-⌘ idea comes from.

## License

MIT. See [LICENSE](LICENSE).
