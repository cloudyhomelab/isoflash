// Command isoflash writes an ISO image to a USB drive from a terminal UI.
package main

import (
	"fmt"
	"os"

	tea "github.com/charmbracelet/bubbletea"
)

func main() {
	if os.Geteuid() != 0 {
		fmt.Fprintf(os.Stderr, "isoflash writes to raw drives and must run as root:\n  sudo %s\n", os.Args[0])
		os.Exit(1)
	}
	initial := newModel()
	final, err := tea.NewProgram(initial).Run()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if final.(model).step == stepFailed {
		os.Exit(1)
	}
}
