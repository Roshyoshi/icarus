package main

import (
	"errors"
	"fmt"
	"os"

	"icarus.local/icarus/internal/launcher"
)

func main() {
	if err := launcher.Run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "icarus:", err)
		var usage *launcher.UsageError
		if errors.As(err, &usage) {
			os.Exit(2)
		}
		os.Exit(1)
	}
}
