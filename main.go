// rex-glance: jump onto another machine's screen from a Rex pane, fast, to do
// something by hand. See README.md.
package main

import (
	"fmt"
	"os"
)

const usage = `rex-glance: another machine's screen in a Rex pane.

  rex-glance look <machine> [--user U] [--ask] [--snapshot out.png]
                       show that machine's screen here (q quits)
  rex-glance reach <machine>
                       can its screen sharing be reached, and how (no login)
  rex-glance probe     what this terminal can do: sizes, in-place redraws, mouse
`

func main() {
	if len(os.Args) < 2 {
		fmt.Print(usage)
		return
	}
	var err error
	switch os.Args[1] {
	case "look":
		err = lookCmd(os.Args[2:])
	case "reach":
		err = reachCmd(os.Args[2:])
	case "probe":
		err = probe()
	case "rawprobe":
		err = rawprobe()
	default:
		fmt.Print(usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "rex-glance:", err)
		os.Exit(1)
	}
}
