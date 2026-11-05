package main

import (
	"fmt"
	"os"

	"github.com/GregoryGost/gost-restreamer/internal/processowner"
)

func main() {
	if err := processowner.RunHelper(os.Args[1:]); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
