package main

import (
	"fmt"
	"os"

	"gioui.org/app"

	"github.com/xlfe/zka/internal/launcher"
)

func main() {
	go func() {
		window := new(app.Window)
		if err := launcher.Run(window, os.Args[1:]...); err != nil {
			fmt.Fprintf(os.Stderr, "zka-launch: %v\n", err)
			os.Exit(1)
		}
		os.Exit(0)
	}()
	app.Main()
}
