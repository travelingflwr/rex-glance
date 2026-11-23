# rex-glance

Jump onto another machine's screen from a [Rex](https://www.superlogical.com/rex) pane, fast, to do something by hand.

## What it's for

The moments when a terminal isn't enough and you need the screen for a minute: a stuck dialog to click, a login window, a setting that only has a GUI, a quick look at what's showing. You open it, do the thing, and close it.

It is **not** a remote desktop to work in all day, and not for video. For that, use a real screen-sharing app ([rootshell](https://github.com/kitknox/rootshell), Screen Sharing.app).

## What it values

**Latency over looks.**

- Colours are reduced (8- or 16-bit) and the screen is scaled down when that helps.
- Only the parts of the screen that changed are sent and redrawn.
- Clicks and keys go out the moment you make them.
- No HEVC, no audio, no smoothing.

**Nothing to install on the far side.** rex-glance connects to the screen-sharing server the machine already runs:

- **macOS:** Screen Sharing, built in (System Settings → General → Sharing).
- **Linux:** wayvnc (Wayland) or x11vnc (X11).

**It goes where Rex goes.** It's a program that runs in a Rex pane and draws with the kitty graphics protocol, so any Rex client shows it, the phone included. No change to Rex is needed.

## Where it's used

For now, the two machines here with a desktop: a Mac running Screen Sharing, and a Linux machine running wayvnc. Headless machines have no screen to glance at. Windows comes later, for work machines, and isn't part of this first version.

## How it connects

- **The route:** RFB (VNC) through an SSH tunnel over your tailnet, so the screen-sharing port never has to be open on a network.
- **The login:** Apple Remote Desktop's Diffie-Hellman scheme (security type 30), which macOS Screen Sharing requires and wayvnc offers too, so one login covers Macs and Linux. It uses the far machine's username and password.
- **The password:** kept in the system keychain, or asked for each time. Never in a file.

## Use

```sh
rex-glance look <machine>          # the screen here: click, scroll and type on it; Ctrl-] quits
rex-glance look <machine> --fast   # half the resolution, about a quarter of the data
rex-glance look <machine> --snapshot out.png
rex-glance reach <machine>         # can its screen sharing be reached, and how (no login)
```

In Rex, **Glance at…** (from `glance.lua`) picks a machine and opens it in a floating window. In a session another tool manages, such as tr3x's T3 Code session, it opens in a session of its own. The first time, it asks for the password with typing hidden, and offers to keep it in the Keychain once the login works.

Rex asks palette questions on every connected app, and the first answer wins. So when Glance is run from an app that can't show them yet (iOS, by default: `ask_in_window`), it skips the palette and asks which machine in the window itself.

The picture fills the pane. It's drawn at twice the pane's point size and fitted to its cells, so text stays sharp on Retina screens, and each drawn pixel averages the screen pixels it covers, so thin strokes survive the shrinking. `--fast` draws at the point size instead.

Keys reach the machine as typed, Ctrl-C included, so Ctrl-] quits, the way telnet does. ⌘ stays with Rex for its own shortcuts, so ⌘-shortcuts don't reach a Mac yet.

## Plan

1. ✓ **Probes.** Rex replaces image rectangles in place (about 10,000 64×64 tiles a second) and reports the mouse per pixel.
2. ✓ **Look.** Connect, log in, and show the screen, redrawing only the rectangles that change.
3. ✓ **Poke.** Clicks, drags, scrolling and keys, mapped from the pane to the remote screen.
4. ✓ **From the palette.** "Glance at…" picks a machine and opens it.
5. **Next:** a way to send ⌘ to a Mac; ask in the window when Rex's palette can't (older Rex apps); 16-bit colour for slow links.

## Credit

The RFB and Apple Screen Sharing details follow [rootshell-vnc](https://github.com/kitknox/rootshell-vnc) by Kit Knox (MIT), the VNC client inside rootshell.

## License

MIT. See [LICENSE](LICENSE).
