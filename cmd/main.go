// Command ssnap snapshots a directory and restores it later.
//
// Usage:
//
//	ssnap save <dir>      Record the current state of <dir> as its snapshot
//	ssnap restore <dir>   Return <dir> to the state recorded by save
//	ssnap drop <dir>      Delete the snapshot for <dir>
//	ssnap ls              List every directory that has a snapshot
//	ssnap inspect <dir>   Show what is recorded inside the snapshot of <dir>
package main

import (
	"os"

	"ssnap/internal"
)

func main() {
	os.Exit(ssnap.Run(os.Args[1:]))
}
