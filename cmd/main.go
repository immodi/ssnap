// Command ssnap snapshots a directory or regular file and restores it later.
//
// Usage:
//
//	ssnap save <path>              Record the current state of <path> as its snapshot
//	ssnap restore <path>           Return <path> to the state recorded by save
//	ssnap drop <path>              Delete the snapshot for <path>
//	ssnap ls [-p page]             List every path that has a snapshot
//	ssnap inspect <path> [-p page] Show what is recorded inside the snapshot of <path>
//	ssnap help                     Show usage information
package main

import (
	"os"

	"ssnap/internal"
)

func main() {
	os.Exit(ssnap.Run(os.Args[1:]))
}
