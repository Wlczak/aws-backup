package main

import (
	"flag"
	"fmt"
	"os"
)

var version = "0.0.0-dev"

func main() {
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return
	}

	fmt.Fprintln(os.Stderr, "aws-backup: no command specified; try --version")
	os.Exit(2)
}
