package main

import (
	"fmt"
	"os"

	"github.com/xlfe/zka/internal/zka"
)

func main() {
	code, err := zka.Run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "zka: %v\n", err)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}
