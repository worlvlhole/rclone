//+build windows

package cmd

import (
	"os"
	"sync"

	ansiterm "github.com/Azure/go-ansiterm"
	"github.com/Azure/go-ansiterm/winterm"
)

var (
	initAnsiParser sync.Once
	ansiParser     *ansiterm.AnsiParser
)

func writeToTerminal(b []byte) {
	initAnsiParser.Do(func() {
		winEventHandler := winterm.CreateWinEventHandler(os.Stdout.Fd(), os.Stdout)
		ansiParser = ansiterm.CreateParser("Ground", winEventHandler)
	})
	_, err := ansiParser.Parse(b)
	if err != nil {
		// Ignore errors - can't log them and printing them spams the user
		// _, _ = fmt.Fprintf(os.Stderr, "\n*** Error from ANSI parser: %v\n", err)
	}
}
