package main

import (
	"fmt"
	"io"
	"os"
)

var version = "dev"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 1 && (args[0] == "version" || args[0] == "--version" || args[0] == "-v") {
		fmt.Fprintf(stdout, "taskcircuit %s\n", version)
		return 0
	}

	fmt.Fprintln(stderr, "TaskCircuit Go migration is in progress. Available command: version")
	return 2
}
