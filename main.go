package main

import (
	"fmt"
	"os"

	"github.com/pti/wraptool/cmd"
)

func main() {
	if err := cmd.NewRootCmd().Execute(); err != nil {
		if msg := err.Error(); msg != "" {
			fmt.Fprintln(os.Stderr, msg)
		}
		os.Exit(cmd.ExitCode(err))
	}
}
