// Package ssnap snapshots the state of a directory or a regular file and
// restores it later.
//
// It implements the ssnap commands:
//
//	save <path>      Record the current state of <path> as its snapshot
//	restore <path>   Return <path> to the state recorded by save
//	drop <path>      Delete the snapshot for <path>
//	ls               List every path that has a snapshot
//	inspect <path>   Show what is recorded inside the snapshot of <path>
//	help             Show usage information
//
// Snapshots are stored under ~/.ssnap/<id>, where <id> is a hash of the
// absolute, symlink-resolved path of <path>. Every operation is written to be
// atomic: a crash never leaves a half-written snapshot or a half-restored
// directory behind.
package ssnap

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	storeName = ".ssnap"
	metaFile  = "meta.json"
	treeDir   = "tree" // recorded tree of a directory snapshot
	fileData  = "file" // recorded contents of a file snapshot

	// Snapshot kinds. An empty kind in meta.json means a directory snapshot,
	// for compatibility with snapshots written before files were supported.
	kindDir  = "dir"
	kindFile = "file"

	// defaultPageSize is the number of ls rows or inspect entries per page.
	defaultPageSize = 20

	// Prefixes for the transient directories that live inside the store.
	tmpPrefix  = ".tmp-"  // a snapshot being built
	oldPrefix  = ".old-"  // a snapshot moved aside during replacement
	dropPrefix = ".drop-" // a snapshot moved aside during drop

	// Prefix for the transient sibling directory used to rebuild a tree
	// during restore. It lives next to <dir>, not in the store.
	restorePrefix = ".ssnap-restore-"
	// Prefix for the previous <dir> moved aside during a non-atomic swap.
	swapPrefix = ".ssnap-old-"
)

// errNoExchange is returned by exchangeDirs when atomic directory exchange is
// unavailable, so callers can fall back to a sequence of renames.
var errNoExchange = errors.New("atomic directory exchange not supported")

// forceNoExchange is a test switch that forces the portable rename-based
// fallback even where atomic exchange is supported.
var forceNoExchange bool

// errReflinkUnsupported is reported by cloneFile on platforms without reflink
// support, so copyFile falls back to a byte-for-byte copy.
var errReflinkUnsupported = errors.New("reflink not supported on this platform")

// reflinkDisabled is a test switch that forces the byte-copy fallback.
var reflinkDisabled bool

// freeBytes reports the space available on the filesystem holding path. It is
// a variable so tests can simulate a full disk.
var freeBytes = statfsFree

// stdout is where successful commands write their confirmation. It is a
// variable so tests can capture it.
var stdout io.Writer = os.Stdout

// exchangeDirs swaps the directories a and b atomically when the platform and
// filesystem support it, otherwise reporting errNoExchange.
func exchangeDirs(a, b string) error {
	if forceNoExchange {
		return errNoExchange
	}
	return exchangeDirsImpl(a, b)
}

// testCrash is a hook used by the test suite. When it equals "save" or
// "restore" the process aborts partway through the corresponding copy,
// simulating a crash. It is empty during normal operation.
var testCrash = os.Getenv("SSNAP_TEST_CRASH")
var crashCount int

const usage = `usage:
  ssnap save <path>              Record <path> (a directory or regular file)
  ssnap restore <path>           Return <path> to the state recorded by save
  ssnap drop <path>              Delete the snapshot for <path>
  ssnap ls [-p page]             List every path that has a snapshot
  ssnap inspect <path> [-p page] Show what is recorded inside the snapshot
  ssnap help                     Show this help

options:
  -p, --page N        Show page N (default 1)
      --page-size N   Entries per page (default 20)`

const helpText = usage + `

Snapshots record a whole directory tree or a single regular file. save replaces
any previous snapshot for the same path. restore leaves the snapshot in place
and works even if <path> has been deleted. drop only removes the snapshot; the
original path is not touched. ls and inspect are read-only.

Pagination applies to ls and inspect. Pages are numbered from 1; a page past
the end is an error.

Run "ssnap <command>" with an invalid number of arguments to see this usage.`

// meta describes a snapshot. It is written inside every snapshot directory.
type meta struct {
	Path      string `json:"path"`           // absolute, symlink-resolved path of the target
	Kind      string `json:"kind,omitempty"` // kindDir, kindFile, or empty (legacy directory)
	SavedAt   string `json:"saved_at"`       // when the snapshot was taken (RFC 3339)
	SizeBytes uint64 `json:"size_bytes"`     // total size of the recorded regular files
}

// isFile reports whether m describes a single-file snapshot.
func (m meta) isFile() bool { return m.Kind == kindFile }

// Run executes a single command line (the arguments after the program name)
// and returns the process exit code.
func Run(args []string) int {
	if len(args) == 0 {
		return fail(usage)
	}
	if args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprintln(stdout, helpText)
		return 0
	}
	for _, a := range args[1:] {
		if a == "--" {
			break
		}
		if a == "-h" || a == "--help" {
			fmt.Fprintln(stdout, helpText)
			return 0
		}
	}

	store, err := storePath()
	if err != nil {
		return fail(err.Error())
	}

	switch args[0] {
	case "ls":
		rest, opts, err := parsePageArgs(args[1:])
		if err != nil {
			return fail(err.Error())
		}
		if len(rest) != 0 {
			return fail(usage)
		}
		if err := ls(store, opts); err != nil {
			return fail(err.Error())
		}
		return 0
	case "save", "restore", "drop", "inspect":
		cmd := args[0]
		rest, opts, err := parsePageArgs(args[1:])
		if err != nil {
			return fail(err.Error())
		}
		if len(rest) != 1 {
			return fail(usage)
		}
		target := rest[0]
		if target == "" {
			return fail("path argument is empty")
		}
		if cmd != "inspect" && (opts.pageSet || opts.sizeSet) {
			return fail("pagination is only supported by ls and inspect")
		}

		var msg string
		switch cmd {
		case "save":
			err = save(store, target)
			msg = fmt.Sprintf("saved snapshot of %s", target)
		case "restore":
			err = restore(store, target)
			msg = fmt.Sprintf("restored %s", target)
		case "drop":
			err = drop(store, target)
			msg = fmt.Sprintf("dropped snapshot of %s", target)
		case "inspect":
			// inspect prints its own report; there is no confirmation line.
			err = inspect(store, target, opts)
		}
		if err != nil {
			return fail(err.Error())
		}
		if msg != "" {
			fmt.Fprintln(stdout, msg)
		}
		return 0
	default:
		return fail("unknown command: " + args[0])
	}
}

func fail(msg string) int {
	fmt.Fprintln(os.Stderr, "ssnap: "+msg)
	return 1
}

// ---------------------------------------------------------------------------
// pagination
// ---------------------------------------------------------------------------

// pageOpts describes how output should be paged. A zero pageOpts (paginated
// false) prints everything.
type pageOpts struct {
	page      int
	size      int
	paginated bool
	pageSet   bool
	sizeSet   bool
}

// parsePageArgs separates -p/--page and --page-size options from positional
// arguments. Flags may appear before or after the path. A page value may also
// carry an explicit size, as in "-p 2:10".
func parsePageArgs(args []string) ([]string, pageOpts, error) {
	opts := pageOpts{page: 1, size: defaultPageSize}
	var positional []string

	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--":
			positional = append(positional, args[i+1:]...)
			return positional, opts, nil
		case arg == "-p" || arg == "--page":
			// A bare -p means the first page; a value selects another page.
			if i+1 >= len(args) || strings.HasPrefix(args[i+1], "-") {
				opts.page, opts.pageSet, opts.paginated = 1, true, true
				continue
			}
			i++
			p, size, hasSize, err := parsePageValue(args[i])
			if err != nil {
				return nil, opts, err
			}
			opts.page, opts.pageSet, opts.paginated = p, true, true
			if hasSize {
				opts.size, opts.sizeSet = size, true
			}
		case strings.HasPrefix(arg, "--page="):
			p, size, hasSize, err := parsePageValue(strings.TrimPrefix(arg, "--page="))
			if err != nil {
				return nil, opts, err
			}
			opts.page, opts.pageSet, opts.paginated = p, true, true
			if hasSize {
				opts.size, opts.sizeSet = size, true
			}
		case strings.HasPrefix(arg, "-p="):
			p, size, hasSize, err := parsePageValue(strings.TrimPrefix(arg, "-p="))
			if err != nil {
				return nil, opts, err
			}
			opts.page, opts.pageSet, opts.paginated = p, true, true
			if hasSize {
				opts.size, opts.sizeSet = size, true
			}
		case len(arg) > 2 && strings.HasPrefix(arg, "-p") && !strings.HasPrefix(arg, "--"):
			p, size, hasSize, err := parsePageValue(arg[2:])
			if err != nil {
				return nil, opts, err
			}
			opts.page, opts.pageSet, opts.paginated = p, true, true
			if hasSize {
				opts.size, opts.sizeSet = size, true
			}
		case arg == "--page-size" || arg == "-n" || arg == "-s" || arg == "--per-page":
			if i+1 >= len(args) {
				return nil, opts, fmt.Errorf("%s requires a page size", arg)
			}
			i++
			size, err := parsePositiveInt(args[i], "page size")
			if err != nil {
				return nil, opts, err
			}
			opts.size, opts.sizeSet, opts.paginated = size, true, true
		case strings.HasPrefix(arg, "--page-size="):
			size, err := parsePositiveInt(strings.TrimPrefix(arg, "--page-size="), "page size")
			if err != nil {
				return nil, opts, err
			}
			opts.size, opts.sizeSet, opts.paginated = size, true, true
		case strings.HasPrefix(arg, "--per-page="):
			size, err := parsePositiveInt(strings.TrimPrefix(arg, "--per-page="), "page size")
			if err != nil {
				return nil, opts, err
			}
			opts.size, opts.sizeSet, opts.paginated = size, true, true
		case strings.HasPrefix(arg, "-n=") || strings.HasPrefix(arg, "-s="):
			size, err := parsePositiveInt(arg[3:], "page size")
			if err != nil {
				return nil, opts, err
			}
			opts.size, opts.sizeSet, opts.paginated = size, true, true
		case arg == "-" || !strings.HasPrefix(arg, "-"):
			positional = append(positional, arg)
		default:
			return nil, opts, fmt.Errorf("unknown option: %s", arg)
		}
	}
	return positional, opts, nil
}

// parsePageValue parses "N" or "N:SIZE" (also N/SIZE and N,SIZE).
func parsePageValue(v string) (page, size int, hasSize bool, err error) {
	if i := strings.IndexAny(v, ":/,"); i >= 0 {
		page, err = parsePositiveInt(v[:i], "page")
		if err != nil {
			return 0, 0, false, err
		}
		size, err = parsePositiveInt(v[i+1:], "page size")
		if err != nil {
			return 0, 0, false, err
		}
		return page, size, true, nil
	}
	page, err = parsePositiveInt(v, "page")
	return page, 0, false, err
}

func parsePositiveInt(v, what string) (int, error) {
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("invalid %s %q: must be a positive integer", what, v)
	}
	return n, nil
}

// pageRange returns the half-open range of items on the selected page. It
// returns an error when the requested page does not exist.
func pageRange(n int, opts pageOpts) (int, int, error) {
	if !opts.paginated {
		return 0, n, nil
	}
	if n == 0 {
		if opts.page == 1 {
			return 0, 0, nil
		}
		return 0, 0, fmt.Errorf("page %d is out of range: there is nothing to show", opts.page)
	}
	pages := (n + opts.size - 1) / opts.size
	if opts.page > pages {
		return 0, 0, fmt.Errorf("page %d is out of range: only %d page(s) of %d entries",
			opts.page, pages, opts.size)
	}
	start := (opts.page - 1) * opts.size
	end := start + opts.size
	if end > n {
		end = n
	}
	return start, end, nil
}

// storePath returns the directory that holds all snapshots.
func storePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot determine home directory: %w", err)
	}
	return filepath.Join(home, storeName), nil
}

// ---------------------------------------------------------------------------
// save
// ---------------------------------------------------------------------------

func save(store, target string) error {
	if err := ensureStore(store); err != nil {
		return err
	}
	cleanupStore(store)

	resolved, err := resolveDir(target)
	if err != nil {
		return fmt.Errorf("cannot resolve %q: %w", target, err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return fmt.Errorf("cannot access %q: %w", target, err)
	}

	kind := ""
	switch {
	case info.IsDir():
		kind = kindDir
	case info.Mode().IsRegular():
		kind = kindFile
	default:
		return fmt.Errorf("%q is not a directory or regular file", target)
	}
	if err := refusePaths(resolved, store); err != nil {
		return err
	}

	// Guard against running out of space before writing anything.
	need := uint64(info.Size())
	if kind == kindDir {
		need, err = treeSize(resolved)
		if err != nil {
			return fmt.Errorf("cannot size %q: %w", target, err)
		}
	}
	if err := requireSpace(store, need); err != nil {
		return err
	}

	id := snapshotID(resolved)
	final := filepath.Join(store, id)

	// Build the new snapshot in a temporary directory inside the store.
	tmp, err := os.MkdirTemp(store, tmpPrefix)
	if err != nil {
		return err
	}
	if err := writeMeta(tmp, meta{
		Path:      resolved,
		Kind:      kind,
		SavedAt:   time.Now().Format(time.RFC3339),
		SizeBytes: need,
	}); err != nil {
		os.RemoveAll(tmp)
		return err
	}

	var copyErr error
	if kind == kindFile {
		// Two hook calls keep single-file saves testable with the same crash
		// mechanism used for trees (which visit at least a root and an entry).
		maybeCrash("save")
		copyErr = copyFile(resolved, filepath.Join(tmp, fileData), info)
		maybeCrash("save")
	} else {
		copyErr = copyTree(resolved, filepath.Join(tmp, treeDir), "save")
	}
	if copyErr != nil {
		// Leave nothing half-written: the previous snapshot is untouched.
		os.RemoveAll(tmp)
		return fmt.Errorf("save aborted: %w", copyErr)
	}

	// Swap the finished snapshot into place as the final step.
	if err := installDir(tmp, final); err != nil {
		os.RemoveAll(tmp)
		return err
	}
	return nil
}

// ---------------------------------------------------------------------------
// restore
// ---------------------------------------------------------------------------

func restore(store, target string) error {
	if err := ensureStore(store); err != nil {
		return err
	}
	cleanupStore(store)

	// <path> may already be gone; the snapshot id is derived from its path with
	// symlinks resolved in whatever part of it still exists.
	resolved, err := resolveExisting(target)
	if err != nil {
		return fmt.Errorf("cannot resolve %q: %w", target, err)
	}
	if err := refusePaths(resolved, store); err != nil {
		return err
	}

	snap := filepath.Join(store, snapshotID(resolved))
	m, err := readMeta(snap)
	if err != nil || m.Path == "" {
		return fmt.Errorf("no snapshot for %q", target)
	}

	if m.isFile() {
		return restoreFile(target, resolved, snap)
	}
	return restoreDir(target, resolved, snap)
}

// restoreDir restores a directory snapshot into the given resolved path.
func restoreDir(target, resolved, snap string) error {
	// <path> may have been deleted entirely. If anything is at the path it must
	// be a directory; otherwise fail and leave it untouched.
	var dirExists bool
	switch fi, err := os.Lstat(resolved); {
	case err == nil:
		if !fi.IsDir() {
			return fmt.Errorf("%q exists and is not a directory", target)
		}
		dirExists = true
	case errors.Is(err, os.ErrNotExist):
		// Recreated below.
	default:
		return fmt.Errorf("cannot access %q: %w", target, err)
	}

	// Guard against running out of space before building the temporary tree.
	need, err := treeSize(filepath.Join(snap, treeDir))
	if err != nil {
		return fmt.Errorf("cannot size snapshot: %w", err)
	}
	spaceAt := resolved
	if !dirExists {
		spaceAt = existingAncestor(resolved)
	}
	if err := requireSpace(spaceAt, need); err != nil {
		return err
	}

	parent := filepath.Dir(resolved)

	// If <path> is missing, its parents may be missing too. Recreate them with
	// mode 0755, remembering which ones we made so they can be removed again if
	// the restore fails.
	created, err := createParents(parent)
	if err != nil {
		return err
	}
	abort := func(err error) error {
		removeEmpty(created)
		return err
	}

	cleanupSiblings(parent)

	// Build the restored tree in a temporary sibling of <path>, so that the
	// final swap is a rename on the same filesystem.
	tmp, err := os.MkdirTemp(parent, restorePrefix)
	if err != nil {
		return abort(err)
	}
	if err := copyDirInto(filepath.Join(snap, treeDir), tmp); err != nil {
		os.RemoveAll(tmp)
		return abort(fmt.Errorf("restore aborted: %w", err))
	}

	if !dirExists {
		// <path> is missing: simply rename the finished tree into place.
		if err := os.Rename(tmp, resolved); err != nil {
			os.RemoveAll(tmp)
			return abort(err)
		}
		return nil
	}

	if err := swapDirs(tmp, resolved); err != nil {
		os.RemoveAll(tmp)
		return abort(err)
	}
	// After a successful swap, tmp holds the previous contents of <path>.
	os.RemoveAll(tmp)
	return nil
}

// restoreFile restores a single-file snapshot into the given resolved path.
// The new contents are built next to the target and renamed into place, so a
// crash never leaves a half-written file behind.
func restoreFile(target, resolved, snap string) error {
	data := filepath.Join(snap, fileData)
	info, err := os.Lstat(data)
	if err != nil {
		return fmt.Errorf("snapshot for %q is incomplete: %w", target, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("snapshot for %q is incomplete: recorded file is not regular", target)
	}

	// If something is at the path it must not be a directory; otherwise fail
	// and leave it untouched.
	switch fi, err := os.Lstat(resolved); {
	case err == nil:
		if fi.IsDir() {
			return fmt.Errorf("%q exists and is a directory", target)
		}
	case errors.Is(err, os.ErrNotExist):
		// Recreated below.
	default:
		return fmt.Errorf("cannot access %q: %w", target, err)
	}

	if err := requireSpace(existingAncestor(filepath.Dir(resolved)), uint64(info.Size())); err != nil {
		return err
	}

	parent := filepath.Dir(resolved)

	// Recreate missing parents, remembering them so a failed restore can undo
	// them.
	created, err := createParents(parent)
	if err != nil {
		return err
	}
	abort := func(err error) error {
		removeEmpty(created)
		return err
	}

	cleanupSiblings(parent)

	tmp := filepath.Join(parent, randName(restorePrefix))
	maybeCrash("restore")
	if err := copyFile(data, tmp, info); err != nil {
		os.Remove(tmp)
		return abort(fmt.Errorf("restore aborted: %w", err))
	}
	maybeCrash("restore")
	if err := os.Rename(tmp, resolved); err != nil {
		os.Remove(tmp)
		return abort(err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// drop
// ---------------------------------------------------------------------------

func drop(store, dir string) error {
	if err := ensureStore(store); err != nil {
		return err
	}
	cleanupStore(store)

	// <dir> may already be gone; the snapshot id is derived from its path with
	// symlinks resolved in whatever part of it still exists.
	resolved, err := resolveExisting(dir)
	if err != nil {
		return fmt.Errorf("cannot resolve %q: %w", dir, err)
	}

	snap := filepath.Join(store, snapshotID(resolved))
	if _, err := os.Stat(snap); err != nil {
		return fmt.Errorf("no snapshot for %q", dir)
	}

	// Move the snapshot out of place, then delete it.
	trash := filepath.Join(store, randName(dropPrefix))
	if err := os.Rename(snap, trash); err != nil {
		return err
	}
	return os.RemoveAll(trash)
}

// ---------------------------------------------------------------------------
// ls
// ---------------------------------------------------------------------------

// ls prints one line per saved path: the original path, the time it was saved,
// and the total size of the recorded files. It reads only each snapshot's
// meta.json, never the trees, so it stays fast at any size.
func ls(store string, opts pageOpts) error {
	entries, err := os.ReadDir(store)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil // nothing has been saved yet
		}
		return err
	}

	type row struct {
		path  string
		saved string
		size  uint64
	}
	var rows []row
	for _, e := range entries {
		// Stale temp directories (and everything else dot-prefixed) are not
		// snapshots.
		if strings.HasPrefix(e.Name(), ".") || !e.IsDir() {
			continue
		}
		m, err := readMeta(filepath.Join(store, e.Name()))
		if err != nil || m.Path == "" {
			continue // no valid meta.json: not a snapshot
		}
		rows = append(rows, row{path: m.Path, saved: m.SavedAt, size: m.SizeBytes})
	}

	sort.Slice(rows, func(i, j int) bool { return rows[i].path < rows[j].path })
	start, end, err := pageRange(len(rows), opts)
	if err != nil {
		return err
	}
	for _, r := range rows[start:end] {
		fmt.Fprintf(stdout, "%s\t%s\t%s\n", r.path, r.saved, humanSize(r.size))
	}
	return nil
}

// ---------------------------------------------------------------------------
// inspect
// ---------------------------------------------------------------------------

// inspectEntry is one line of an inspect report.
type inspectEntry struct {
	path  string
	mode  os.FileMode
	size  int64
	link  string
	isDir bool
}

// inspect prints what is recorded inside the snapshot of target. It is
// read-only: it touches neither the snapshot nor target, and works even after
// target is gone.
func inspect(store, target string, opts pageOpts) error {
	resolved, err := resolveExisting(target)
	if err != nil {
		return fmt.Errorf("cannot resolve %q: %w", target, err)
	}

	snap := filepath.Join(store, snapshotID(resolved))
	if si, err := os.Stat(snap); err != nil || !si.IsDir() {
		return fmt.Errorf("no snapshot for %q", target)
	}
	m, err := readMeta(snap)
	if err != nil {
		return fmt.Errorf("no snapshot for %q", target)
	}

	var entries []inspectEntry
	var files, dirs, links int
	var total uint64

	if m.isFile() {
		info, err := os.Lstat(filepath.Join(snap, fileData))
		if err != nil {
			return fmt.Errorf("snapshot for %q is incomplete: %w", target, err)
		}
		files = 1
		total = uint64(info.Size())
		entries = append(entries, inspectEntry{
			path: filepath.Base(m.Path),
			mode: info.Mode(),
			size: info.Size(),
		})
	} else {
		tree := filepath.Join(snap, treeDir)
		err = filepath.WalkDir(tree, func(p string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if p == tree {
				return nil // the root itself is not listed
			}
			rel, err := filepath.Rel(tree, p)
			if err != nil {
				return err
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			e := inspectEntry{path: filepath.ToSlash(rel), mode: info.Mode()}
			switch {
			case info.Mode()&os.ModeSymlink != 0:
				links++
				if e.link, err = os.Readlink(p); err != nil {
					return err
				}
			case info.IsDir():
				dirs++
				e.isDir = true
			case info.Mode().IsRegular():
				files++
				e.size = info.Size()
				total += uint64(info.Size())
			}
			entries = append(entries, e)
			return nil
		})
		if err != nil {
			return err
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].path < entries[j].path })

	start, end, err := pageRange(len(entries), opts)
	if err != nil {
		return err
	}

	fmt.Fprintf(stdout, "path:   %s\n", m.Path)
	fmt.Fprintf(stdout, "saved:  %s\n", m.SavedAt)
	fmt.Fprintf(stdout, "totals: files %d, dirs %d, symlinks %d, %s\n", files, dirs, links, humanSize(total))
	fmt.Fprintln(stdout)
	for _, e := range entries[start:end] {
		size := "-"
		if e.mode.IsRegular() {
			size = strconv.FormatInt(e.size, 10)
		}
		name := e.path
		if e.isDir {
			name += "/"
		}
		if e.mode&os.ModeSymlink != 0 {
			name += " -> " + e.link
		}
		fmt.Fprintf(stdout, "%s%10s  %s\n", modeString(e.mode), size, name)
	}
	return nil
}

// ---------------------------------------------------------------------------
// paths
// ---------------------------------------------------------------------------

// resolveDir returns the absolute, symlink-resolved form of dir. It fails if
// dir does not exist.
func resolveDir(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(filepath.Clean(abs))
}

// resolveExisting returns the absolute form of dir with symlinks resolved in
// whatever part of the path exists. Unlike resolveDir it does not fail when the
// path (or some of its parents) is missing, so it can derive the snapshot id of
// a directory that has since been deleted.
func resolveExisting(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	abs = filepath.Clean(abs)

	// Walk up until we reach a component that exists, then resolve that prefix
	// and re-attach the missing tail.
	rest := ""
	cur := abs
	for {
		if r, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(r, rest), nil
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return abs, nil // reached the root without resolving anything
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}

// snapshotID derives the stable snapshot identifier for a resolved path.
func snapshotID(resolved string) string {
	sum := sha256.Sum256([]byte(resolved))
	return hex.EncodeToString(sum[:])
}

// refusePaths rejects dangerous targets: the root, the home directory, and any
// relationship between dir and the snapshot store that would cause recursion.
func refusePaths(resolved, store string) error {
	if resolved == string(filepath.Separator) {
		return errors.New("refusing to operate on the root directory")
	}

	if home, err := os.UserHomeDir(); err == nil {
		if hr, err := resolveExisting(home); err == nil && hr == resolved {
			return errors.New("refusing to operate on the home directory")
		}
	}

	storeResolved, err := resolveExisting(store)
	if err != nil {
		storeResolved = filepath.Clean(store)
	}
	if resolved == storeResolved {
		return errors.New("refusing to operate on the snapshot store")
	}
	if isWithin(resolved, storeResolved) {
		return errors.New("refusing to operate on a path inside the snapshot store")
	}
	if isWithin(storeResolved, resolved) {
		return errors.New("refusing to snapshot a path that contains the snapshot store")
	}
	return nil
}

// isWithin reports whether child is parent itself or lies inside parent.
func isWithin(parent, child string) bool {
	if parent == child {
		return true
	}
	prefix := parent
	if !strings.HasSuffix(prefix, string(filepath.Separator)) {
		prefix += string(filepath.Separator)
	}
	return strings.HasPrefix(child, prefix)
}

// ---------------------------------------------------------------------------
// store housekeeping
// ---------------------------------------------------------------------------

func ensureStore(store string) error {
	if err := os.MkdirAll(store, 0o700); err != nil {
		return fmt.Errorf("cannot create snapshot store: %w", err)
	}
	return nil
}

// cleanupStore removes stale temporary directories and recovers a snapshot
// that was moved aside by an interrupted replacement.
func cleanupStore(store string) {
	entries, err := os.ReadDir(store)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, ".") {
			continue
		}
		full := filepath.Join(store, name)
		switch {
		case strings.HasPrefix(name, oldPrefix):
			// A previous snapshot may have been moved aside just before a
			// crash. Put it back if its final home is now empty.
			if m, err := readMeta(full); err == nil && m.Path != "" {
				path, err := resolveExisting(m.Path)
				if err != nil {
					path = filepath.Clean(m.Path)
				}
				dest := filepath.Join(store, snapshotID(path))
				if _, err := os.Stat(dest); os.IsNotExist(err) {
					if err := os.Rename(full, dest); err == nil {
						continue
					}
				}
			}
			os.RemoveAll(full)
		case strings.HasPrefix(name, tmpPrefix), strings.HasPrefix(name, dropPrefix):
			os.RemoveAll(full)
		}
	}
}

// cleanupSiblings removes stale restore scratch directories left next to a
// target directory by an interrupted restore.
func cleanupSiblings(parent string) {
	entries, err := os.ReadDir(parent)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, restorePrefix) || strings.HasPrefix(name, swapPrefix) {
			os.RemoveAll(filepath.Join(parent, name))
		}
	}
}

// ---------------------------------------------------------------------------
// atomic placement
// ---------------------------------------------------------------------------

// installDir moves src onto dst, replacing any existing directory. It uses an
// atomic exchange when available so a crash leaves either the old or the new
// tree in place, never neither.
func installDir(src, dst string) error {
	if _, err := os.Lstat(dst); err != nil {
		return os.Rename(src, dst)
	}
	if err := exchangeDirs(src, dst); err == nil {
		return os.RemoveAll(src) // src now holds the previous tree
	} else if !errors.Is(err, errNoExchange) {
		return err
	}

	// Fallback: move the old tree aside, then move the new one in. A crash in
	// the small window between the two renames is recovered by cleanupStore.
	old := filepath.Join(filepath.Dir(dst), randName(oldPrefix))
	if err := os.Rename(dst, old); err != nil {
		return err
	}
	if err := os.Rename(src, dst); err != nil {
		os.Rename(old, dst) // best effort rollback
		return err
	}
	return os.RemoveAll(old)
}

// swapDirs exchanges the contents of a and b. On success b holds what a held
// and vice versa; the caller deletes the scratch directory afterwards.
func swapDirs(a, b string) error {
	if err := exchangeDirs(a, b); err == nil {
		return nil
	} else if !errors.Is(err, errNoExchange) {
		return err
	}

	parent := filepath.Dir(b)
	old := filepath.Join(parent, randName(swapPrefix))
	if err := os.Rename(b, old); err != nil {
		return err
	}
	if err := os.Rename(a, b); err != nil {
		os.Rename(old, b)
		return err
	}
	return os.RemoveAll(old)
}

// randName returns a random directory name with the given prefix.
func randName(prefix string) string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return prefix + "fallback"
	}
	return prefix + hex.EncodeToString(b[:])
}

// ---------------------------------------------------------------------------
// copying
// ---------------------------------------------------------------------------

// copyTree recursively copies src to dst, preserving permissions, mtimes and
// symlinks. Symlinks are never followed. Special files are skipped with a
// warning. Any read error aborts the copy.
func copyTree(src, dst, phase string) error {
	maybeCrash(phase)

	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	mode := info.Mode()

	switch {
	case mode&os.ModeSymlink != 0:
		target, err := os.Readlink(src)
		if err != nil {
			return err
		}
		return os.Symlink(target, dst)

	case mode.IsDir():
		if err := os.Mkdir(dst, mode.Perm()); err != nil {
			return err
		}
		entries, err := os.ReadDir(src)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if err := copyTree(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name()), phase); err != nil {
				return err
			}
		}
		// Restore the directory's own metadata after its children were written.
		if err := os.Chmod(dst, mode.Perm()); err != nil {
			return err
		}
		return setTimes(dst, info)

	case mode.IsRegular():
		return copyFile(src, dst, info)

	default:
		fmt.Fprintf(os.Stderr, "ssnap: warning: skipping special file %s\n", src)
		return nil
	}
}

// copyDirInto copies the entries of the directory src into the existing
// directory dst, then applies src's own permissions and mtime to dst.
func copyDirInto(src, dst string) error {
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", src)
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := copyTree(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name()), "restore"); err != nil {
			return err
		}
	}
	if err := os.Chmod(dst, info.Mode().Perm()); err != nil {
		return err
	}
	return setTimes(dst, info)
}

func copyFile(src, dst string, info os.FileInfo) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm())
	if err != nil {
		return err
	}

	if !reflinkDisabled {
		if err := cloneFile(out, in); err == nil {
			// Reflink succeeded: contents are already in place.
			if err := out.Close(); err != nil {
				return err
			}
			if err := os.Chmod(dst, info.Mode().Perm()); err != nil {
				return err
			}
			return setTimes(dst, info)
		}
		// Fall back to a byte-for-byte copy of this file.
		if err := out.Truncate(0); err != nil {
			out.Close()
			return err
		}
		if _, err := out.Seek(0, io.SeekStart); err != nil {
			out.Close()
			return err
		}
	}

	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	if err := os.Chmod(dst, info.Mode().Perm()); err != nil {
		return err
	}
	return setTimes(dst, info)
}

// treeSize sums the sizes of all regular files under root, never following
// symlinks.
func treeSize(root string) (uint64, error) {
	var total uint64
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			info, err := d.Info()
			if err != nil {
				return err
			}
			total += uint64(info.Size())
		}
		return nil
	})
	return total, err
}

// requireSpace fails when the filesystem holding path has less free space than
// need. Callers assume a full copy is required even if reflinks may make it
// cheaper.
func requireSpace(path string, need uint64) error {
	avail, err := freeBytes(path)
	if err != nil {
		return fmt.Errorf("cannot determine free space on %s: %w", path, err)
	}
	if need > avail {
		return fmt.Errorf("not enough free space on %s: need %d bytes, only %d available", path, need, avail)
	}
	return nil
}

func setTimes(path string, info os.FileInfo) error {
	mt := info.ModTime()
	return os.Chtimes(path, mt, mt)
}

// maybeCrash aborts the process partway through a copy when the test hook is
// active, simulating an interrupted run.
func maybeCrash(phase string) {
	if testCrash != phase {
		return
	}
	crashCount++
	if crashCount >= 2 {
		os.Exit(70)
	}
}

// ---------------------------------------------------------------------------
// metadata
// ---------------------------------------------------------------------------

func writeMeta(dir string, m meta) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, metaFile), b, 0o600)
}

func readMeta(dir string) (meta, error) {
	b, err := os.ReadFile(filepath.Join(dir, metaFile))
	if err != nil {
		return meta{}, err
	}
	var m meta
	if err := json.Unmarshal(b, &m); err != nil {
		return meta{}, err
	}
	return m, nil
}

// ---------------------------------------------------------------------------
// restore helpers
// ---------------------------------------------------------------------------

// createParents creates any missing directories from the nearest existing
// ancestor down to dir (mode 0755). It returns the directories it created, from
// shallowest to deepest, so the caller can undo them.
func createParents(dir string) ([]string, error) {
	var missing []string
	for cur := dir; ; {
		if _, err := os.Lstat(cur); err == nil {
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		missing = append(missing, cur)
		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}
		cur = parent
	}

	var created []string
	for i := len(missing) - 1; i >= 0; i-- {
		if err := os.Mkdir(missing[i], 0o755); err != nil {
			removeEmpty(created)
			return nil, err
		}
		created = append(created, missing[i])
	}
	return created, nil
}

// removeEmpty removes the given directories deepest-first, ignoring any that
// are not empty or no longer exist.
func removeEmpty(dirs []string) {
	for i := len(dirs) - 1; i >= 0; i-- {
		os.Remove(dirs[i])
	}
}

// existingAncestor returns path if it exists, otherwise its nearest existing
// ancestor.
func existingAncestor(path string) string {
	for {
		if _, err := os.Lstat(path); err == nil {
			return path
		}
		parent := filepath.Dir(path)
		if parent == path {
			return path
		}
		path = parent
	}
}

// ---------------------------------------------------------------------------
// display helpers
// ---------------------------------------------------------------------------

// humanSize formats a byte count in binary units, e.g. "2.3 KiB".
func humanSize(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	units := []string{"B", "KiB", "MiB", "GiB", "TiB", "PiB", "EiB"}
	f := float64(n)
	i := 0
	for f >= unit && i < len(units)-1 {
		f /= unit
		i++
	}
	return fmt.Sprintf("%.1f %s", f, units[i])
}

// modeString renders a file mode the way ls -l does, e.g. "-rw-r--r--". Go
// spells symlinks with an uppercase "L"; ls uses a lowercase "l".
func modeString(m os.FileMode) string {
	s := m.String()
	if len(s) > 0 && s[0] == 'L' {
		s = "l" + s[1:]
	}
	return s
}
