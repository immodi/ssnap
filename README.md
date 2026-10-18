# ssnap

A tiny, atomic CLI that snapshots a directory and restores it later.

`ssnap save` records the entire state of a directory. Whatever happens to that
directory afterwards doesn't matter: `ssnap restore` puts it back exactly as it
was — files edited, deleted, or newly created are all reset.

```console
$ echo "original" > notes.txt
$ ssnap save .
$ echo "oops" > notes.txt
$ rm -rf src/
$ ssnap restore .
$ cat notes.txt
original
```

## Features

- **Whole-tree snapshots** — every file and subdirectory, including empty
  directories, with file contents.
- **Faithful restore** — permissions, mtimes, and symlinks (including dangling
  ones) survive a round trip. Symlinks are never followed. A deleted directory
  is recreated, missing parents and all.
- **Inspectable** — `ls` lists every snapshot from its metadata alone, and
  `inspect` shows exactly what a snapshot records.
- **Atomic** — every operation either fully succeeds or leaves things exactly
  as they were. An interrupted run never leaves a half-written snapshot or a
  half-restored directory behind.
- **Cheap on modern filesystems** — regular files are reflinked
  (copy-on-write cloned) where supported, with a transparent per-file fallback
  to a normal copy.
- **Zero dependencies** — pure Go standard library, one static binary, no
  flags and no config.

## Install

Requires Go 1.24 or newer.

```console
# Build a local binary called ssnap:
go build -o ssnap ./cmd

# Or install it (the binary is named "cmd" by default):
go install ./cmd
```

## Usage

```
ssnap save <dir>      Record the current state of <dir> as its snapshot
ssnap restore <dir>   Return <dir> to the state recorded by save
ssnap drop <dir>      Delete the snapshot for <dir>
ssnap ls              List every directory that has a snapshot
ssnap inspect <dir>   Show what is recorded inside the snapshot of <dir>
```

That's the whole interface. There are no flags and no configuration.

- `save` replaces any previous snapshot for the same directory.
- `restore` leaves the snapshot in place, so it can be run any number of times.
  It works even if `<dir>` has been deleted entirely, recreating it (and any
  missing parent directories) with the recorded permissions.
- `drop` deletes the snapshot; the directory itself is not touched.
- `ls` and `inspect` are read-only: they never touch the directory or the
  snapshot.

On success the three mutating commands print a short confirmation to `stdout`:

```console
$ ssnap save ~/project
saved snapshot of /home/you/project
$ ssnap restore ~/project
restored /home/you/project
$ ssnap drop ~/project
dropped snapshot of /home/you/project
```

Errors are printed to `stderr` with exit code `1`.

### Listing and inspecting

`ls` prints one line per saved directory, sorted by path, with the original
absolute path, the time it was saved (RFC 3339) and the total size of the
recorded files. It reads only each snapshot's metadata, so it stays fast no
matter how large the snapshots are, and it lists a snapshot whether or not the
original directory still exists:

```console
$ ssnap ls
/home/you/app	2026-10-18T15:40:12+03:00	2.3 KiB
```

`inspect` shows what is recorded inside a snapshot — a short header followed by
one line per entry, sorted by path, with the mode as `ls -l` renders it:

```console
$ ssnap inspect ~/app
path:   /home/you/app
saved:  2026-10-18T15:40:12+03:00
totals: files 2, dirs 1, symlinks 1, 2.3 KiB

-rw-r--r--       120  README.md
lrwxrwxrwx         -  latest -> src/main.go
drwxr-xr-x         -  src/
-rw-r--r--      2210  src/main.go
```

## How it works

### Snapshot storage

Snapshots live under your home directory:

```
~/.ssnap/<id>/
    meta.json    original path, time saved, total size in bytes
    tree/        the recorded copy of the directory
```

`<id>` is a SHA-256 hash of the absolute, symlink-resolved path of `<dir>`. The
same directory always maps to the same snapshot, no matter how the path was
typed (relative, with a trailing slash, or through a symlink). `meta.json` is a
small JSON object with the fields `path`, `saved_at` (RFC 3339) and
`size_bytes`; it is written together with `tree/`, so `save` always replaces the
two as a unit.

### Atomicity

- **save** copies the tree into a temporary directory inside the store, then
  swaps it into place as the final step, so a crash mid-save leaves the
  previous snapshot intact.
- **restore** builds the restored tree in a temporary sibling of `<dir>` (same
  parent, same filesystem) and swaps it in, so a crash mid-restore leaves
  `<dir>` as it was. If `<dir>` is missing, the finished temporary directory is
  simply renamed into place; missing parents are created along the way.
- **drop** renames the snapshot out of place and then deletes it.

On Linux the swap uses `renameat2(RENAME_EXCHANGE)` for a truly atomic
exchange; elsewhere it falls back to a sequence of renames. Stale temporary
directories left by a crash are ignored by every command and cleaned up at the
start of the next run.

### Copy strategy

For each regular file, `ssnap` first attempts a reflink (a copy-on-write clone,
via the `FICLONE` ioctl on Linux). On filesystems that support it (btrfs, XFS,
APFS) this is near-instant and uses no extra space until either side changes.
If the reflink fails for any reason — unsupported filesystem, cross-device, and
so on — that one file falls back to a normal byte-for-byte copy. Hardlinks are
never used, since an in-place edit would silently change the snapshot.

### Free-space guard

Before writing anything, `save` sums the sizes of all regular files under
`<dir>` and checks that the store's filesystem has at least that much free
space. `restore` does the same against `<dir>`'s filesystem before building its
temporary tree. The guard always assumes a full copy is needed. If space is
short, the command aborts with a clear error and leaves the previous snapshot
untouched.

## Edge cases

- `<dir>` must exist and be a directory for `save`.
- `restore` is the exception: `<dir>` may be missing entirely — it is recreated
  along with any missing parents (mode `0755`). If something other than a
  directory sits at the path, `restore` fails and leaves it untouched. `restore`,
  `drop` and `inspect` all work on a directory that no longer exists, deriving
  the snapshot id from the absolute path with symlinks resolved in whatever part
  of it remains.
- Refuses to operate on `/`, the home directory, or the snapshot store, and
  refuses to snapshot a directory that contains the store (or the reverse),
  to avoid recursive copies.
- Special files (sockets, FIFOs, device nodes) are skipped with a warning on
  `save`.
- If a file is unreadable during `save`, the whole save is aborted and the
  previous snapshot is kept.

## Exit codes

| Code | Meaning                                     |
| ---- | ------------------------------------------- |
| `0`  | success                                     |
| `1`  | any error (message printed to `stderr`)     |

## Development

```console
make build   # build ./ssnap
make test    # go test ./...
make race    # go test -race ./...
make vet     # go vet ./...
make fmt     # gofmt -w .
```

The layout:

```
cmd/          thin main package
internal/     the implementation and tests
```

## License

[MIT](LICENSE)
