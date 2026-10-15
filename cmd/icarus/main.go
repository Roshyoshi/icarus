package main

import (
	"fmt"
	"os"

	"icarus.local/icarus/internal/launcher"
)

func main() {
	if err := launcher.Run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "icarus:", err)
		os.Exit(1)
	}
}
