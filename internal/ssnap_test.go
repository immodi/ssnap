package ssnap

import (
	"bytes"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestMain lets the test binary re-exec itself as the real CLI so that crash
// scenarios can be exercised by killing a child process.
func TestMain(m *testing.M) {
	if os.Getenv("SSNAP_TEST_CHILD") == "1" {
		os.Exit(Run(os.Args[1:]))
	}
	os.Exit(m.Run())
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func setHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	return home
}

func storeDir(home string) string { return filepath.Join(home, ".ssnap") }

// capture runs fn with stderr redirected and returns its exit code and output.
func capture(fn func() int) (int, string) {
	old := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w
	code := fn()
	w.Close()
	os.Stderr = old
	out, _ := io.ReadAll(r)
	r.Close()
	return code, string(out)
}

// captureOut runs fn with stdout redirected and returns its exit code and
// output.
func captureOut(fn func() int) (int, string) {
	var buf bytes.Buffer
	old := stdout
	stdout = &buf
	code := fn()
	stdout = old
	return code, buf.String()
}

func mustRun(t *testing.T, args ...string) {
	t.Helper()
	code, out := capture(func() int { return Run(args) })
	if code != 0 {
		t.Fatalf("ssnap %v: exit %d, output: %s", args, code, out)
	}
}

func wantErr(t *testing.T, args ...string) string {
	t.Helper()
	code, out := capture(func() int { return Run(args) })
	if code != 1 {
		t.Fatalf("ssnap %v: want exit 1, got %d (%s)", args, code, out)
	}
	return out
}

func writeFile(t *testing.T, path, content string, perm os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), perm); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, perm); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func mustSymlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

// snapshotDirs lists the snapshot identifiers present in the store.
func snapshotDirs(t *testing.T, store string) []string {
	t.Helper()
	entries, err := os.ReadDir(store)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), ".") {
			ids = append(ids, e.Name())
		}
	}
	return ids
}

// runChildCrash re-execs the test binary as the CLI with a crash hook enabled,
// and returns the child's exit code.
func runChildCrash(t *testing.T, phase string, args ...string) int {
	t.Helper()
	cmd := exec.Command(os.Args[0], args...)
	cmd.Env = append(os.Environ(), "SSNAP_TEST_CHILD=1", "SSNAP_TEST_CRASH="+phase)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("child %v did not crash (output: %s)", args, out)
	}
	ee, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("child failed to run: %v (output: %s)", err, out)
	}
	return ee.ExitCode()
}

// ---------------------------------------------------------------------------
// 1. save, modify a file, restore -> reverted
// ---------------------------------------------------------------------------

func TestRestoreRevertsModifiedFile(t *testing.T) {
	setHome(t)
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.txt"), "one", 0o644)

	mustRun(t, "save", dir)
	writeFile(t, filepath.Join(dir, "a.txt"), "two", 0o644)
	mustRun(t, "restore", dir)

	if got := readFile(t, filepath.Join(dir, "a.txt")); got != "one" {
		t.Fatalf("want %q, got %q", "one", got)
	}
}

// ---------------------------------------------------------------------------
// 2. save, delete a file and a subdirectory, restore -> both come back
// ---------------------------------------------------------------------------

func TestRestoreRecreatesDeletedFileAndDir(t *testing.T) {
	setHome(t)
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "keep.txt"), "keep", 0o644)
	writeFile(t, filepath.Join(dir, "sub", "deep.txt"), "deep", 0o644)

	mustRun(t, "save", dir)
	if err := os.Remove(filepath.Join(dir, "keep.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(dir, "sub")); err != nil {
		t.Fatal(err)
	}
	mustRun(t, "restore", dir)

	if got := readFile(t, filepath.Join(dir, "keep.txt")); got != "keep" {
		t.Fatalf("keep.txt: want %q, got %q", "keep", got)
	}
	if got := readFile(t, filepath.Join(dir, "sub", "deep.txt")); got != "deep" {
		t.Fatalf("sub/deep.txt: want %q, got %q", "deep", got)
	}
}

// ---------------------------------------------------------------------------
// 3. save, add new files and directories, restore -> they are gone
// ---------------------------------------------------------------------------

func TestRestoreRemovesAddedEntries(t *testing.T) {
	setHome(t)
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "orig.txt"), "orig", 0o644)

	mustRun(t, "save", dir)
	writeFile(t, filepath.Join(dir, "added.txt"), "added", 0o644)
	writeFile(t, filepath.Join(dir, "newdir", "nested.txt"), "nested", 0o644)
	mustRun(t, "restore", dir)

	if exists(filepath.Join(dir, "added.txt")) {
		t.Fatal("added.txt should have been removed")
	}
	if exists(filepath.Join(dir, "newdir")) {
		t.Fatal("newdir should have been removed")
	}
	if !exists(filepath.Join(dir, "orig.txt")) {
		t.Fatal("orig.txt should have survived")
	}
}

// ---------------------------------------------------------------------------
// 4. empty dirs, symlinks (incl. dangling), permissions, mtimes survive
// ---------------------------------------------------------------------------

func TestRoundTripPreservesMetadata(t *testing.T) {
	setHome(t)
	dir := t.TempDir()

	if err := os.Mkdir(filepath.Join(dir, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "script.sh"), "#!/bin/sh\n", 0o755)
	writeFile(t, filepath.Join(dir, "data.bin"), "payload", 0o640)
	if err := os.Mkdir(filepath.Join(dir, "restricted"), 0o750); err != nil {
		t.Fatal(err)
	}
	mustSymlink(t, "data.bin", filepath.Join(dir, "goodlink"))
	mustSymlink(t, "does-not-exist", filepath.Join(dir, "dangling"))

	mt := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := os.Chtimes(filepath.Join(dir, "data.bin"), mt, mt); err != nil {
		t.Fatal(err)
	}

	mustRun(t, "save", dir)

	// Wreck the directory before restoring.
	if err := os.RemoveAll(filepath.Join(dir, "empty")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "data.bin"), "clobbered", 0o600)
	if err := os.Remove(filepath.Join(dir, "goodlink")); err != nil {
		t.Fatal(err)
	}

	mustRun(t, "restore", dir)

	if fi, err := os.Stat(filepath.Join(dir, "empty")); err != nil || !fi.IsDir() {
		t.Fatalf("empty directory not restored: %v", err)
	}

	for _, tc := range []struct {
		path string
		perm os.FileMode
	}{
		{filepath.Join(dir, "script.sh"), 0o755},
		{filepath.Join(dir, "data.bin"), 0o640},
		{filepath.Join(dir, "restricted"), 0o750},
	} {
		fi, err := os.Stat(tc.path)
		if err != nil {
			t.Fatalf("%s: %v", tc.path, err)
		}
		if fi.Mode().Perm() != tc.perm {
			t.Fatalf("%s: perm = %o, want %o", tc.path, fi.Mode().Perm(), tc.perm)
		}
	}

	for link, target := range map[string]string{
		"goodlink": "data.bin",
		"dangling": "does-not-exist",
	} {
		got, err := os.Readlink(filepath.Join(dir, link))
		if err != nil {
			t.Fatalf("%s: %v", link, err)
		}
		if got != target {
			t.Fatalf("%s: target = %q, want %q", link, got, target)
		}
	}

	fi, err := os.Stat(filepath.Join(dir, "data.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if !fi.ModTime().Equal(mt) {
		t.Fatalf("mtime = %v, want %v", fi.ModTime(), mt)
	}
}

// ---------------------------------------------------------------------------
// 5. save twice -> the second snapshot replaces the first
// ---------------------------------------------------------------------------

func TestSaveReplacesPreviousSnapshot(t *testing.T) {
	home := setHome(t)
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "f"), "v1", 0o644)

	mustRun(t, "save", dir)
	writeFile(t, filepath.Join(dir, "f"), "v2", 0o644)
	mustRun(t, "save", dir)

	if ids := snapshotDirs(t, storeDir(home)); len(ids) != 1 {
		t.Fatalf("want exactly one snapshot, got %v", ids)
	}

	writeFile(t, filepath.Join(dir, "f"), "v3", 0o644)
	mustRun(t, "restore", dir)
	if got := readFile(t, filepath.Join(dir, "f")); got != "v2" {
		t.Fatalf("want %q, got %q", "v2", got)
	}
}

// ---------------------------------------------------------------------------
// 6. restore / drop with no snapshot -> clear error, exit code 1
// ---------------------------------------------------------------------------

func TestRestoreWithoutSnapshotFails(t *testing.T) {
	setHome(t)
	dir := t.TempDir()
	out := wantErr(t, "restore", dir)
	if !strings.Contains(out, "no snapshot") {
		t.Fatalf("unexpected message: %q", out)
	}
}

func TestDropWithoutSnapshotFails(t *testing.T) {
	setHome(t)
	dir := t.TempDir()
	out := wantErr(t, "drop", dir)
	if !strings.Contains(out, "no snapshot") {
		t.Fatalf("unexpected message: %q", out)
	}
}

// ---------------------------------------------------------------------------
// 7. interrupted save / restore leave prior state intact
// ---------------------------------------------------------------------------

func TestInterruptedSaveKeepsPreviousSnapshot(t *testing.T) {
	setHome(t)
	dir := t.TempDir()
	for _, name := range []string{"a", "b", "c"} {
		writeFile(t, filepath.Join(dir, name), "v1", 0o644)
	}
	mustRun(t, "save", dir)

	for _, name := range []string{"a", "b", "c"} {
		writeFile(t, filepath.Join(dir, name), "v2", 0o644)
	}

	if code := runChildCrash(t, "save", "save", dir); code == 0 {
		t.Fatalf("interrupted save unexpectedly succeeded")
	}

	// The previous snapshot must be intact and the directory untouched.
	if got := readFile(t, filepath.Join(dir, "a")); got != "v2" {
		t.Fatalf("save must not modify the directory: got %q", got)
	}
	mustRun(t, "restore", dir)
	for _, name := range []string{"a", "b", "c"} {
		if got := readFile(t, filepath.Join(dir, name)); got != "v1" {
			t.Fatalf("%s: want %q, got %q", name, "v1", got)
		}
	}

	assertNoTempDirs(t, dir)
}

func TestInterruptedRestoreLeavesDirectoryIntact(t *testing.T) {
	setHome(t)
	dir := t.TempDir()
	for _, name := range []string{"a", "b", "c"} {
		writeFile(t, filepath.Join(dir, name), "v1", 0o644)
	}
	mustRun(t, "save", dir)

	for _, name := range []string{"a", "b", "c"} {
		writeFile(t, filepath.Join(dir, name), "v2", 0o644)
	}

	if code := runChildCrash(t, "restore", "restore", dir); code == 0 {
		t.Fatalf("interrupted restore unexpectedly succeeded")
	}

	// The directory must be exactly as it was before the interrupted restore.
	for _, name := range []string{"a", "b", "c"} {
		if got := readFile(t, filepath.Join(dir, name)); got != "v2" {
			t.Fatalf("%s: want %q, got %q", name, "v2", got)
		}
	}

	// The snapshot is intact: a later restore still works.
	mustRun(t, "restore", dir)
	for _, name := range []string{"a", "b", "c"} {
		if got := readFile(t, filepath.Join(dir, name)); got != "v1" {
			t.Fatalf("%s: want %q, got %q", name, "v1", got)
		}
	}

	assertNoTempDirs(t, dir)
}

// assertNoTempDirs checks that no scratch directories remain in the store,
// including the parent of the target directory.
func assertNoTempDirs(t *testing.T, dir string) {
	t.Helper()
	home, _ := os.UserHomeDir()
	store := filepath.Join(home, ".ssnap")
	if entries, err := os.ReadDir(store); err == nil {
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), ".") {
				t.Fatalf("stale temp entry in store: %s", e.Name())
			}
		}
	}
	if entries, err := os.ReadDir(filepath.Dir(dir)); err == nil {
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), restorePrefix) || strings.HasPrefix(e.Name(), swapPrefix) {
				t.Fatalf("stale restore scratch dir: %s", e.Name())
			}
		}
	}
}

// ---------------------------------------------------------------------------
// extra behavior
// ---------------------------------------------------------------------------
func TestCommandsReportSuccess(t *testing.T) {
	setHome(t)
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "f"), "v", 0o644)

	code, out := captureOut(func() int { return Run([]string{"save", dir}) })
	if code != 0 {
		t.Fatalf("save exit %d", code)
	}
	if !strings.Contains(out, "saved") || !strings.Contains(out, dir) {
		t.Fatalf("save output = %q", out)
	}

	code, out = captureOut(func() int { return Run([]string{"restore", dir}) })
	if code != 0 {
		t.Fatalf("restore exit %d", code)
	}
	if !strings.Contains(out, "restored") || !strings.Contains(out, dir) {
		t.Fatalf("restore output = %q", out)
	}

	code, out = captureOut(func() int { return Run([]string{"drop", dir}) })
	if code != 0 {
		t.Fatalf("drop exit %d", code)
	}
	if !strings.Contains(out, "dropped") || !strings.Contains(out, dir) {
		t.Fatalf("drop output = %q", out)
	}

	// Errors go to stderr, not stdout.
	code, out = captureOut(func() int { return Run([]string{"restore", dir}) })
	if code != 1 {
		t.Fatalf("restore (no snapshot) exit %d", code)
	}
	if out != "" {
		t.Fatalf("error wrote to stdout: %q", out)
	}
}

func TestRestoreIsRepeatable(t *testing.T) {
	setHome(t)
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "f"), "v", 0o644)
	mustRun(t, "save", dir)

	mustRun(t, "restore", dir)
	writeFile(t, filepath.Join(dir, "f"), "changed", 0o644)
	mustRun(t, "restore", dir)

	if got := readFile(t, filepath.Join(dir, "f")); got != "v" {
		t.Fatalf("want %q, got %q", "v", got)
	}
}

func TestDropKeepsDirectory(t *testing.T) {
	home := setHome(t)
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "f"), "v", 0o644)
	mustRun(t, "save", dir)

	mustRun(t, "drop", dir)
	if got := readFile(t, filepath.Join(dir, "f")); got != "v" {
		t.Fatalf("drop must not touch the directory: got %q", got)
	}
	if ids := snapshotDirs(t, storeDir(home)); len(ids) != 0 {
		t.Fatalf("snapshot not dropped: %v", ids)
	}
	wantErr(t, "restore", dir)
}

func TestSnapshotIDIgnoresPathForm(t *testing.T) {
	home := setHome(t)
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "f"), "v", 0o644)
	mustRun(t, "save", dir)

	link := filepath.Join(t.TempDir(), "link")
	mustSymlink(t, dir, link)

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(cwd, dir)
	if err != nil {
		t.Fatal(err)
	}

	// All of these must resolve to the same snapshot.
	mustRun(t, "restore", link)
	mustRun(t, "restore", dir+string(filepath.Separator))
	mustRun(t, "restore", rel)
	mustRun(t, "save", link)
	mustRun(t, "save", rel)

	if ids := snapshotDirs(t, storeDir(home)); len(ids) != 1 {
		t.Fatalf("want one snapshot, got %v", ids)
	}
}

func TestSpecialFilesAreSkipped(t *testing.T) {
	setHome(t)
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "keep"), "v", 0o644)

	fifo := filepath.Join(dir, "pipe")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}

	code, out := capture(func() int { return Run([]string{"save", dir}) })
	if code != 0 {
		t.Fatalf("save failed: %s", out)
	}
	if !strings.Contains(out, "skipping special file") {
		t.Fatalf("expected a special-file warning, got %q", out)
	}

	mustRun(t, "restore", dir)
	if exists(fifo) {
		t.Fatal("special file should not be present after restore")
	}
	if !exists(filepath.Join(dir, "keep")) {
		t.Fatal("regular file lost")
	}
}

func TestSaveUnreadableFileAborts(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores permission bits")
	}
	setHome(t)
	dir := t.TempDir()
	secret := filepath.Join(dir, "secret")
	writeFile(t, secret, "v1", 0o644)
	mustRun(t, "save", dir)

	writeFile(t, secret, "v2", 0o644)
	if err := os.Chmod(secret, 0o000); err != nil {
		t.Fatal(err)
	}
	out := wantErr(t, "save", dir)
	if !strings.Contains(out, "save aborted") {
		t.Fatalf("unexpected message: %q", out)
	}
	if err := os.Chmod(secret, 0o644); err != nil {
		t.Fatal(err)
	}

	// The previous snapshot must be intact.
	mustRun(t, "restore", dir)
	if got := readFile(t, secret); got != "v1" {
		t.Fatalf("want %q, got %q", "v1", got)
	}
}

func TestInvalidTargets(t *testing.T) {
	home := setHome(t)
	dir := t.TempDir()

	wantErr(t, "save", filepath.Join(dir, "missing"))
	wantErr(t, "restore", filepath.Join(dir, "missing"))
	wantErr(t, "save", "")
	wantErr(t, "bogus", dir)
	wantErr(t, "save")

	// The root directory.
	wantErr(t, "save", string(filepath.Separator))

	// The home directory.
	wantErr(t, "save", home)

	// The snapshot store itself, and a directory inside it.
	mustRun(t, "save", dir) // ensure the store exists
	wantErr(t, "save", storeDir(home))
	inside := filepath.Join(storeDir(home), "sub")
	if err := os.Mkdir(inside, 0o755); err != nil {
		t.Fatal(err)
	}
	wantErr(t, "save", inside)

	// A directory that contains the snapshot store.
	wantErr(t, "save", filepath.Dir(home))
}

func TestFallbackRenameSwap(t *testing.T) {
	// Exercise the portable path that avoids renameat2 exchange.
	forceNoExchange = true
	defer func() { forceNoExchange = false }()

	setHome(t)
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "f"), "v1", 0o644)
	writeFile(t, filepath.Join(dir, "sub", "g"), "g1", 0o644)

	mustRun(t, "save", dir)
	writeFile(t, filepath.Join(dir, "f"), "v2", 0o644)
	if err := os.RemoveAll(filepath.Join(dir, "sub")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "extra"), "x", 0o644)

	mustRun(t, "restore", dir)
	if got := readFile(t, filepath.Join(dir, "f")); got != "v1" {
		t.Fatalf("f: want %q, got %q", "v1", got)
	}
	if got := readFile(t, filepath.Join(dir, "sub", "g")); got != "g1" {
		t.Fatalf("sub/g: want %q, got %q", "g1", got)
	}
	if exists(filepath.Join(dir, "extra")) {
		t.Fatal("extra should have been removed")
	}

	// Replacing an existing snapshot must also work via the fallback.
	mustRun(t, "save", dir)
	writeFile(t, filepath.Join(dir, "f"), "v3", 0o644)
	mustRun(t, "restore", dir)
	if got := readFile(t, filepath.Join(dir, "f")); got != "v1" {
		t.Fatalf("f: want %q, got %q", "v1", got)
	}
}

// ---------------------------------------------------------------------------
// 8. reflink / byte-copy strategies produce an identical tree
// ---------------------------------------------------------------------------

type treeEntry struct {
	mode    os.FileMode
	link    string
	content string
}

func collectTree(t *testing.T, root string) map[string]treeEntry {
	t.Helper()
	out := map[string]treeEntry{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == root {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		entry := treeEntry{mode: info.Mode()}
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			if entry.link, err = os.Readlink(path); err != nil {
				return err
			}
		case info.Mode().IsRegular():
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			entry.content = string(b)
		}
		out[rel] = entry
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func assertTree(t *testing.T, root string, want map[string]treeEntry) {
	t.Helper()
	got := collectTree(t, root)
	if len(got) != len(want) {
		t.Fatalf("tree has %d entries, want %d\ngot:  %v\nwant: %v", len(got), len(want), keys(got), keys(want))
	}
	for rel, w := range want {
		g, ok := got[rel]
		if !ok {
			t.Fatalf("missing %s", rel)
		}
		if w.mode.Type() != g.mode.Type() {
			t.Fatalf("%s: type %v, want %v", rel, g.mode.Type(), w.mode.Type())
		}
		if w.mode&os.ModeSymlink == 0 && w.mode.Perm() != g.mode.Perm() {
			t.Fatalf("%s: perm %o, want %o", rel, g.mode.Perm(), w.mode.Perm())
		}
		if w.link != g.link {
			t.Fatalf("%s: link %q, want %q", rel, g.link, w.link)
		}
		if w.content != g.content {
			t.Fatalf("%s: content differs (%d vs %d bytes)", rel, len(g.content), len(w.content))
		}
	}
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func buildSampleTree(t *testing.T, dir string) {
	t.Helper()
	writeFile(t, filepath.Join(dir, "a.txt"), "alpha", 0o644)
	writeFile(t, filepath.Join(dir, "bin", "run.sh"), "#!/bin/sh\necho hi\n", 0o755)
	writeFile(t, filepath.Join(dir, "deep", "one", "two", "three.bin"), strings.Repeat("x", 4096), 0o640)
	if err := os.Mkdir(filepath.Join(dir, "empty"), 0o750); err != nil {
		t.Fatal(err)
	}
	mustSymlink(t, "a.txt", filepath.Join(dir, "link"))
	mustSymlink(t, "gone", filepath.Join(dir, "dangling"))
}

func TestCopyStrategyProducesIdenticalTree(t *testing.T) {
	for _, fallback := range []bool{false, true} {
		name := "reflink"
		if fallback {
			name = "bytecopy"
		}
		t.Run(name, func(t *testing.T) {
			reflinkDisabled = fallback
			defer func() { reflinkDisabled = false }()

			setHome(t)
			dir := t.TempDir()
			buildSampleTree(t, dir)
			want := collectTree(t, dir)

			mustRun(t, "save", dir)

			// Wreck the directory completely, then restore.
			if err := os.RemoveAll(dir); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			writeFile(t, filepath.Join(dir, "junk"), "junk", 0o644)

			mustRun(t, "restore", dir)
			assertTree(t, dir, want)
		})
	}
}

// TestCloneFileWhenSupported exercises the real FICLONE ioctl. It skips on
// filesystems without reflink support (for example tmpfs or ext4).
func TestCloneFileWhenSupported(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "src")
	dstPath := filepath.Join(dir, "dst")
	writeFile(t, srcPath, "original", 0o644)

	src, err := os.Open(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()

	dst, err := os.OpenFile(dstPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if err := cloneFile(dst, src); err != nil {
		dst.Close()
		t.Skipf("reflink unsupported on this filesystem: %v", err)
	}
	if err := dst.Close(); err != nil {
		t.Fatal(err)
	}

	if got := readFile(t, dstPath); got != "original" {
		t.Fatalf("clone content = %q, want %q", got, "original")
	}
	// A reflink must be copy-on-write: editing the source cannot affect it.
	writeFile(t, srcPath, "modified", 0o644)
	if got := readFile(t, dstPath); got != "original" {
		t.Fatalf("clone changed with source: %q", got)
	}
}

func TestSaveDoesNotHardlink(t *testing.T) {
	home := setHome(t)
	dir := t.TempDir()
	orig := filepath.Join(dir, "f")
	writeFile(t, orig, "data", 0o644)
	mustRun(t, "save", dir)

	ids := snapshotDirs(t, storeDir(home))
	if len(ids) != 1 {
		t.Fatalf("want one snapshot, got %v", ids)
	}
	snapFile := filepath.Join(storeDir(home), ids[0], treeDir, "f")

	origStat, err := os.Stat(orig)
	if err != nil {
		t.Fatal(err)
	}
	snapStat, err := os.Stat(snapFile)
	if err != nil {
		t.Fatal(err)
	}
	origSys := origStat.Sys().(*syscall.Stat_t)
	snapSys := snapStat.Sys().(*syscall.Stat_t)
	if origSys.Ino == snapSys.Ino {
		t.Fatal("snapshot file shares an inode with the original (hardlink)")
	}
	if origSys.Nlink != 1 {
		t.Fatalf("original nlink = %d, want 1", origSys.Nlink)
	}

	// Editing the original must not corrupt the snapshot.
	writeFile(t, orig, "changed", 0o644)
	if got := readFile(t, snapFile); got != "data" {
		t.Fatalf("snapshot content changed to %q", got)
	}
}

// ---------------------------------------------------------------------------
// 9. insufficient free space aborts before writing
// ---------------------------------------------------------------------------

func stubNoSpace() func() {
	freeBytes = func(string) (uint64, error) { return 0, nil }
	return func() { freeBytes = statfsFree }
}

func TestSaveAbortsWhenLowOnSpace(t *testing.T) {
	home := setHome(t)
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "f"), "v1", 0o644)
	mustRun(t, "save", dir)

	writeFile(t, filepath.Join(dir, "f"), "v2", 0o644)

	reset := stubNoSpace()
	out := wantErr(t, "save", dir)
	reset()

	if !strings.Contains(out, "not enough free space") {
		t.Fatalf("unexpected message: %q", out)
	}

	// Nothing may have been written: no new snapshot, no temp entries.
	if ids := snapshotDirs(t, storeDir(home)); len(ids) != 1 {
		t.Fatalf("want the previous snapshot only, got %v", ids)
	}
	assertNoTempDirs(t, dir)

	// The previous snapshot must be intact.
	mustRun(t, "restore", dir)
	if got := readFile(t, filepath.Join(dir, "f")); got != "v1" {
		t.Fatalf("want %q, got %q", "v1", got)
	}
}

func TestRestoreAbortsWhenLowOnSpace(t *testing.T) {
	setHome(t)
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "f"), "v1", 0o644)
	mustRun(t, "save", dir)

	writeFile(t, filepath.Join(dir, "f"), "v2", 0o644)

	reset := stubNoSpace()
	out := wantErr(t, "restore", dir)
	reset()

	if !strings.Contains(out, "not enough free space") {
		t.Fatalf("unexpected message: %q", out)
	}

	// The directory must be untouched.
	if got := readFile(t, filepath.Join(dir, "f")); got != "v2" {
		t.Fatalf("want %q, got %q", "v2", got)
	}
	assertNoTempDirs(t, dir)

	// Once space is available the snapshot still restores.
	mustRun(t, "restore", dir)
	if got := readFile(t, filepath.Join(dir, "f")); got != "v1" {
		t.Fatalf("want %q, got %q", "v1", got)
	}
}

// ---------------------------------------------------------------------------
// 10. ls
// ---------------------------------------------------------------------------

func TestLSListsSnapshots(t *testing.T) {
	home := setHome(t)

	// Nothing saved yet: prints nothing and exits 0.
	code, out := captureOut(func() int { return Run([]string{"ls"}) })
	if code != 0 {
		t.Fatalf("ls (empty store) exit %d", code)
	}
	if out != "" {
		t.Fatalf("ls (empty store) printed %q, want nothing", out)
	}

	base := t.TempDir()
	a := filepath.Join(base, "a")
	b := filepath.Join(base, "b")
	writeFile(t, filepath.Join(a, "one.txt"), "hello", 0o644)
	writeFile(t, filepath.Join(b, "two.txt"), "hello world", 0o644)
	mustRun(t, "save", a)
	mustRun(t, "save", b)

	// Entries that are not valid snapshots are ignored: a stale temp directory
	// and a directory without a meta.json.
	if err := os.Mkdir(filepath.Join(storeDir(home), tmpPrefix+"stale"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(storeDir(home), "no-meta"), 0o755); err != nil {
		t.Fatal(err)
	}

	// A snapshot is listed even when its original directory is gone.
	if err := os.RemoveAll(b); err != nil {
		t.Fatal(err)
	}

	code, out = captureOut(func() int { return Run([]string{"ls"}) })
	if code != 0 {
		t.Fatalf("ls exit %d: %s", code, out)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 lines, got %d: %q", len(lines), out)
	}

	// Sorted by path, with three tab-separated columns.
	if !strings.HasPrefix(lines[0], a+"\t") {
		t.Fatalf("first line = %q, want it to start with %q", lines[0], a)
	}
	cols := strings.Split(lines[0], "\t")
	if len(cols) != 3 {
		t.Fatalf("want 3 columns, got %d: %q", len(cols), lines[0])
	}
	if _, err := time.Parse(time.RFC3339, cols[1]); err != nil {
		t.Fatalf("saved_at %q is not RFC 3339: %v", cols[1], err)
	}
	if cols[2] != "5 B" {
		t.Fatalf("size column = %q, want %q", cols[2], "5 B")
	}
	if got := strings.Split(lines[1], "\t")[0]; got != b {
		t.Fatalf("second path = %q, want %q", got, b)
	}

	// A dropped snapshot disappears from the listing.
	mustRun(t, "drop", a)
	code, out = captureOut(func() int { return Run([]string{"ls"}) })
	if code != 0 {
		t.Fatalf("ls exit %d: %s", code, out)
	}
	if !strings.HasPrefix(out, b+"\t") || strings.Count(out, "\n") != 1 {
		t.Fatalf("after drop, ls = %q, want a single line for %q", out, b)
	}
}

func TestSaveWritesMeta(t *testing.T) {
	home := setHome(t)
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "f"), "0123456789", 0o644) // 10 bytes

	mustRun(t, "save", dir)

	ids := snapshotDirs(t, storeDir(home))
	if len(ids) != 1 {
		t.Fatalf("want one snapshot, got %v", ids)
	}
	m, err := readMeta(filepath.Join(storeDir(home), ids[0]))
	if err != nil {
		t.Fatal(err)
	}
	if m.SizeBytes != 10 {
		t.Fatalf("size_bytes = %d, want 10", m.SizeBytes)
	}
	if _, err := time.Parse(time.RFC3339, m.SavedAt); err != nil {
		t.Fatalf("saved_at %q is not RFC 3339: %v", m.SavedAt, err)
	}
	if m.Path == "" {
		t.Fatal("path is empty")
	}
}

// ---------------------------------------------------------------------------
// 11. inspect
// ---------------------------------------------------------------------------

func TestInspectReport(t *testing.T) {
	setHome(t)
	dir := t.TempDir()

	writeFile(t, filepath.Join(dir, "README.md"), strings.Repeat("a", 120), 0o644)
	if err := os.Mkdir(filepath.Join(dir, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(dir, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "src", "main.go"), strings.Repeat("b", 2210), 0o644)
	mustSymlink(t, "src/main.go", filepath.Join(dir, "goodlink"))
	mustSymlink(t, "nowhere", filepath.Join(dir, "dangling"))

	mustRun(t, "save", dir)

	// The saved column is checked separately since it carries the current time.
	wantEntries := []string{
		"-rw-r--r--       120  README.md",
		"lrwxrwxrwx         -  dangling -> nowhere",
		"lrwxrwxrwx         -  goodlink -> src/main.go",
		"drwxr-xr-x         -  src/",
		"-rw-r--r--      2210  src/main.go",
	}

	code, out := captureOut(func() int { return Run([]string{"inspect", dir}) })
	if code != 0 {
		t.Fatalf("inspect exit %d", code)
	}
	header, entries, ok := strings.Cut(out, "\n\n")
	if !ok {
		t.Fatalf("inspect output has no header/body separator:\n%s", out)
	}
	headerLines := strings.Split(header, "\n")
	if len(headerLines) != 3 {
		t.Fatalf("want 3 header lines, got %d: %q", len(headerLines), header)
	}
	if headerLines[0] != "path:   "+dir {
		t.Fatalf("path header = %q", headerLines[0])
	}
	if !strings.HasPrefix(headerLines[1], "saved:  ") {
		t.Fatalf("saved header = %q", headerLines[1])
	}
	if _, err := time.Parse(time.RFC3339, strings.TrimPrefix(headerLines[1], "saved:  ")); err != nil {
		t.Fatalf("saved header not RFC 3339: %v", err)
	}
	if headerLines[2] != "totals: files 2, dirs 1, symlinks 2, 2.3 KiB" {
		t.Fatalf("totals header = %q", headerLines[2])
	}

	gotEntries := strings.Split(strings.TrimRight(entries, "\n"), "\n")
	if len(gotEntries) != len(wantEntries) {
		t.Fatalf("got %d entries, want %d:\n%s", len(gotEntries), len(wantEntries), entries)
	}
	for i := range wantEntries {
		if gotEntries[i] != wantEntries[i] {
			t.Fatalf("entry %d = %q, want %q", i, gotEntries[i], wantEntries[i])
		}
	}

	// inspect is read-only and still works after the original directory is gone.
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	code, out2 := captureOut(func() int { return Run([]string{"inspect", dir}) })
	if code != 0 || out2 != out {
		t.Fatalf("inspect after delete: exit %d, output differs:\n%q\n%q", code, out, out2)
	}
}

func TestInspectWithoutSnapshot(t *testing.T) {
	setHome(t)
	dir := t.TempDir()
	out := wantErr(t, "inspect", dir)
	if !strings.Contains(out, "no snapshot") {
		t.Fatalf("unexpected message: %q", out)
	}
}

// ---------------------------------------------------------------------------
// 12. restore after the directory (or its parents) were deleted
// ---------------------------------------------------------------------------

func TestRestoreRecreatesDeletedDirectory(t *testing.T) {
	setHome(t)
	base := t.TempDir()
	dir := filepath.Join(base, "work")
	if err := os.Mkdir(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "keep.txt"), "keep", 0o644)
	writeFile(t, filepath.Join(dir, "sub", "deep.txt"), "deep", 0o644)
	mustSymlink(t, "keep.txt", filepath.Join(dir, "link"))

	mustRun(t, "save", dir)
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	mustRun(t, "restore", dir)

	if got := readFile(t, filepath.Join(dir, "keep.txt")); got != "keep" {
		t.Fatalf("keep.txt: got %q, want %q", got, "keep")
	}
	if got := readFile(t, filepath.Join(dir, "sub", "deep.txt")); got != "deep" {
		t.Fatalf("deep.txt: got %q, want %q", got, "deep")
	}
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o750 {
		t.Fatalf("dir perm = %o, want %o", fi.Mode().Perm(), 0o750)
	}
	if target, err := os.Readlink(filepath.Join(dir, "link")); err != nil || target != "keep.txt" {
		t.Fatalf("link = %q, %v", target, err)
	}
}

func TestRestoreRecreatesMissingParents(t *testing.T) {
	setHome(t)
	base := t.TempDir()
	dir := filepath.Join(base, "a", "b", "c")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "f"), "v", 0o644)

	mustRun(t, "save", dir)
	// Wipe the whole path, not just the leaf.
	if err := os.RemoveAll(filepath.Join(base, "a")); err != nil {
		t.Fatal(err)
	}
	mustRun(t, "restore", dir)

	if got := readFile(t, filepath.Join(dir, "f")); got != "v" {
		t.Fatalf("f: got %q, want %q", got, "v")
	}
	for _, p := range []string{filepath.Join(base, "a"), filepath.Join(base, "a", "b"), dir} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		if !fi.IsDir() {
			t.Fatalf("%s is not a directory", p)
		}
		if fi.Mode().Perm() != 0o755 {
			t.Fatalf("%s perm = %o, want 0755", p, fi.Mode().Perm())
		}
	}
}

func TestRestoreRefusesFileAtPath(t *testing.T) {
	setHome(t)
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "f"), "v", 0o644)
	mustRun(t, "save", dir)

	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, "i am a file", 0o644)

	out := wantErr(t, "restore", dir)
	if !strings.Contains(out, "not a directory") {
		t.Fatalf("unexpected message: %q", out)
	}
	if got := readFile(t, dir); got != "i am a file" {
		t.Fatalf("file was modified: %q", got)
	}
}
