package main

import "github.com/mayencenouvelle/mayencenouvelle-cli/cmd"

var (
	version   = "dev"
	buildDate = "unknown"
)

func main() {
	cmd.SetVersion(version, buildDate)
	cmd.Execute()
}
