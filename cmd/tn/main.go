// Command tn is a minimal CLI wrapper for the Obsidian TaskNotes HTTP API,
// plus the `tn serve` bridge daemon. All logic lives in internal/tn; this
// file is only the entrypoint.
package main

import (
	"os"

	"tasknotes-cli/internal/tn"
)

func main() {
	os.Exit(tn.Run(os.Args[1:]))
}
