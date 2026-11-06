package main

import (
	"fmt"
	"io"
	"os"

	"github.com/GregoryGost/gost-restreamer/internal/buildinfo"
	"github.com/GregoryGost/gost-restreamer/internal/processowner"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string, stdout io.Writer) error {
	if len(args) == 1 && args[0] == "--version" {
		_, err := fmt.Fprintln(stdout, buildinfo.Current())
		return err
	}
	return processowner.RunHelper(args)
}
