// Package ssnap snapshots the state of a directory and restores it later.
//
// It implements the ssnap commands:
//
//	save <dir>      Record the current state of <dir> as its snapshot
//	restore <dir>   Return <dir> to the state recorded by save
//	drop <dir>      Delete the snapshot for <dir>
//	ls              List every directory that has a snapshot
//	inspect <dir>   Show what is recorded inside the snapshot of <dir>
//
// Snapshots are stored under ~/.ssnap/<id>, where <id> is a hash of the
// absolute, symlink-resolved path of <dir>. Every operation is written to be
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
	treeDir   = "tree"

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

const usage = "usage: ssnap <save|restore|drop|inspect> <dir>\n       ssnap ls"

// meta describes a snapshot. It is written inside every snapshot directory.
type meta struct {
	Path      string `json:"path"`       // absolute, symlink-resolved path of the target
	SavedAt   string `json:"saved_at"`   // when the snapshot was taken (RFC 3339)
	SizeBytes uint64 `json:"size_bytes"` // total size of the recorded regular files
}

// Run executes a single command line (the arguments after the program name)
// and returns the process exit code.
func Run(args []string) int {
	if len(args) == 0 {
		return fail(usage)
	}

	store, err := storePath()
	if err != nil {
		return fail(err.Error())
	}

	switch args[0] {
	case "ls":
		if len(args) != 1 {
			return fail(usage)
		}
		if err := ls(store); err != nil {
			return fail(err.Error())
		}
		return 0
	case "save", "restore", "drop", "inspect":
		if len(args) != 2 {
			return fail(usage)
		}
		cmd, dir := args[0], args[1]
		if dir == "" {
			return fail("directory argument is empty")
		}

		var msg string
		switch cmd {
		case "save":
			err = save(store, dir)
			msg = fmt.Sprintf("saved snapshot of %s", dir)
		case "restore":
			err = restore(store, dir)
			msg = fmt.Sprintf("restored %s", dir)
		case "drop":
			err = drop(store, dir)
			msg = fmt.Sprintf("dropped snapshot of %s", dir)
		case "inspect":
			// inspect prints its own report; there is no confirmation line.
			err = inspect(store, dir)
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

func save(store, dir string) error {
	if err := ensureStore(store); err != nil {
		return err
	}
	cleanupStore(store)

	resolved, err := resolveDir(dir)
	if err != nil {
		return fmt.Errorf("cannot resolve %q: %w", dir, err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return fmt.Errorf("cannot access %q: %w", dir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%q is not a directory", dir)
	}
	if err := refusePaths(resolved, store); err != nil {
		return err
	}

	// Guard against running out of space before writing anything.
	need, err := treeSize(resolved)
	if err != nil {
		return fmt.Errorf("cannot size %q: %w", dir, err)
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
		SavedAt:   time.Now().Format(time.RFC3339),
		SizeBytes: need,
	}); err != nil {
		os.RemoveAll(tmp)
		return err
	}
	if err := copyTree(resolved, filepath.Join(tmp, treeDir), "save"); err != nil {
		// Leave nothing half-written: the previous snapshot is untouched.
		os.RemoveAll(tmp)
		return fmt.Errorf("save aborted: %w", err)
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

func restore(store, dir string) error {
	if err := ensureStore(store); err != nil {
		return err
	}
	cleanupStore(store)

	resolved, err := resolveExisting(dir)
	if err != nil {
		return fmt.Errorf("cannot resolve %q: %w", dir, err)
	}
	if err := refusePaths(resolved, store); err != nil {
		return err
	}

	// <dir> may have been deleted entirely. If anything is at the path it must
	// be a directory; otherwise fail and leave it untouched.
	var dirExists bool
	switch fi, err := os.Lstat(resolved); {
	case err == nil:
		if !fi.IsDir() {
			return fmt.Errorf("%q exists and is not a directory", dir)
		}
		dirExists = true
	case errors.Is(err, os.ErrNotExist):
		// Recreated below.
	default:
		return fmt.Errorf("cannot access %q: %w", dir, err)
	}

	id := snapshotID(resolved)
	snap := filepath.Join(store, id)
	if si, err := os.Stat(snap); err != nil || !si.IsDir() {
		return fmt.Errorf("no snapshot for %q", dir)
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

	// If <dir> is missing, its parents may be missing too. Recreate them with
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

	// Build the restored tree in a temporary sibling of <dir>, so that the
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
		// <dir> is missing: simply rename the finished tree into place.
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
	// After a successful swap, tmp holds the previous contents of <dir>.
	os.RemoveAll(tmp)
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

// ls prints one line per saved directory: the original path, the time it was
// saved, and the total size of the recorded files. It reads only each
// snapshot's meta.json, never the trees, so it stays fast at any size.
func ls(store string) error {
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
	for _, r := range rows {
		fmt.Fprintf(stdout, "%s\t%s\t%s\n", r.path, r.saved, humanSize(r.size))
	}
	return nil
}

// ---------------------------------------------------------------------------
// inspect
// ---------------------------------------------------------------------------

// inspect prints what is recorded inside the snapshot of dir. It is read-only:
// it touches neither the snapshot nor dir, and works even after dir is gone.
func inspect(store, dir string) error {
	resolved, err := resolveExisting(dir)
	if err != nil {
		return fmt.Errorf("cannot resolve %q: %w", dir, err)
	}

	snap := filepath.Join(store, snapshotID(resolved))
	if si, err := os.Stat(snap); err != nil || !si.IsDir() {
		return fmt.Errorf("no snapshot for %q", dir)
	}
	m, err := readMeta(snap)
	if err != nil {
		return fmt.Errorf("no snapshot for %q", dir)
	}

	type entry struct {
		path  string
		mode  os.FileMode
		size  int64
		link  string
		isDir bool
	}

	tree := filepath.Join(snap, treeDir)
	var entries []entry
	var files, dirs, links int
	var total uint64
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
		e := entry{path: filepath.ToSlash(rel), mode: info.Mode()}
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
	sort.Slice(entries, func(i, j int) bool { return entries[i].path < entries[j].path })

	fmt.Fprintf(stdout, "path:   %s\n", m.Path)
	fmt.Fprintf(stdout, "saved:  %s\n", m.SavedAt)
	fmt.Fprintf(stdout, "totals: files %d, dirs %d, symlinks %d, %s\n", files, dirs, links, humanSize(total))
	fmt.Fprintln(stdout)
	for _, e := range entries {
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
		return errors.New("refusing to operate on a directory inside the snapshot store")
	}
	if isWithin(storeResolved, resolved) {
		return errors.New("refusing to snapshot a directory that contains the snapshot store")
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
