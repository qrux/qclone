# qclone

A local copy tool in the spirit of `rclone copy`, built for a failing SSD: a
drive whose old, never-rewritten data needs so much error correction that some
regions read at 2 MB/s while freshly written ones still read at 500 MB/s.

```
qclone [flags] SRC [SRC...] DST
```

- **Unix source convention.** `qclone /path/to/source DST` copies the directory
  itself, to `DST/source/...`. `qclone /path/to/source/ DST` copies what's
  inside it, to `DST/...`. A trailing `/.` or a source of `.` also means
  contents. A file source lands in `DST` under its own name; a symlink source
  is copied as a link, unless it has a trailing slash.
- **Many sources.** Each one is resolved by the rule above. If two of them would
  write the same file, the second one is skipped and logged. On a destination
  that ignores letter case (most Mac volumes), `Docs/x` and `docs/x` count as
  the same file.
- **No copying into itself.** A source whose destination is inside it, or that is
  itself inside its destination, is refused. The check compares the folders
  themselves, not their names, so case, Unicode forms and links can't slip
  past it.
- **Like `rclone copy`:** files already at the destination with the same size and
  modification time (within 1 s) are skipped, so a rerun picks up where the last
  one stopped. Each file is written to a hidden `.qclone-*.part` file next to its
  destination, synced to disk, and only then renamed into place. A crash can't
  leave a file that looks complete (and would be skipped next time) but isn't.
  Modes, modification times, symlinks and empty directories are copied.
  Directories get their modes at the end, so a read-only source folder doesn't
  stop a rerun from retrying a file that failed in it. A symlink found where
  the destination needs a folder is never followed: that folder is skipped and
  logged.
- **Errors never go to the terminal.** They go to `<timestamp>-qclone-errors.txt`
  in the current directory (or `--errors FILE`). The file is created only when
  the first error happens. Exit status: 0 done, 1 some errors, 2 bad arguments,
  130 stopped.

## Interrupted copies resume; unreadable spots are mapped

This works like ddrescue's mapfile. While a file copies, qclone keeps a
`.qclone-<hash>.map` beside its `.part` file, recording which byte ranges are
written. The map is updated every 2 seconds, only after an `fsync`, so it never
claims more than the disk has. It is updated once more when the run stops.
Rerun the same command after Ctrl-C, a crash or a `kill`, and each file carries
on from where it stopped, if its source still has the same size and
modification time. A map for an older version of the source is thrown away.

A chunk that still can't be read after 3 tries is split in halves, and the
halves that fail are split again, down to 4 KiB sectors. Everything readable is
kept, and the unreadable ranges are recorded exactly. A single bad sector in a
1 MiB chunk costs about a dozen reads. A dying drive can take seconds per
failed read, so bisection of one chunk stops after 64 failed reads, and
whatever is still unresolved is recorded as one coarser range.

- **Without `--rescue`**, a file with unreadable spots is not installed. Its part
  and map stay, and the error file lists each spot ("4096 bytes at 2105344").
  A rerun reads only those spots, since drives sometimes succeed on a later try.
- **With `--rescue`**, the spots are filled with zeros and the file is installed.
  A `.qclone-<hash>.bad` marker next to it lists the spots. The marker matters:
  the file has the source's size and time, so without it every later run would
  skip it as unchanged, holes and all. With it, a later run clones the
  installed file (instant on APFS, a plain copy elsewhere), reads only the
  spots, and installs the result. When no spots are left, the marker goes away.

The display and the end-of-run summary show the bytes carried over from
earlier runs and the unreadable bytes, spots and files (counts only, never
names). The usual way to get everything off a failing drive is two passes: one
plain pass to get all the good data, then `--rescue` once nothing more comes
back.

## The rate never reads zero

A meter that counts bytes in the last second shows 0 B/s whenever one read
takes longer than a second, and on a degraded drive single reads can take
several. qclone measures from a baseline instead: the window reaches back at
least 4 seconds, and further until it holds at least 4 completed reads. While
a read is stuck the window keeps growing, so the rate decays toward the true
average rather than dropping to zero. If nothing has landed for a while, the
display says so ("no read finished for 12s") next to that decaying rate. The
per-disk numbers (read and write rate, operations per second, time per read,
queue depth) work the same way: they reach back up to two minutes to find
movement, and show "idle" only if there was none.

Reads and writes also bypass the macOS file cache (`F_NOCACHE`, `--cached` to
turn that off). That way the rate shown is the disk's own, not a burst into
memory followed by a stall while it flushes.

## Stuck reads don't hold up the rest

Every file is cut into chunks (256 KiB; `--chunk` changes it), and a pool of
readers takes chunks as they come free. So a big file is read by several
readers at once, and small files are copied side by side.

A read that has been in flight longer than 50 ms is **stuck**. A healthy SSD
reads a chunk in about a millisecond; old data on a degraded one takes hundreds
of milliseconds, or seconds. Every other read is **flying**. qclone keeps

```
reads in flight = flying + min(stuck, 16)
```

so stuck reads never use up the flying ones' room. Stuck reads are what's
capped: once 16 are stuck (`--max-stuck`), nothing more goes to slow files.
Flying reads have no fixed cap; the tuner adds them only while they pay. A
reader that comes free goes, in this order:

1. While fewer than 16 reads are stuck, to a slow file's next chunk. A file is
   slow while it has a stuck read, or if its last read was slow. Reads sent to
   a slow file count as stuck from the start, so slow files carry on in the
   stuck reads' room and take nothing from the flying ones.
2. Otherwise, to a file with nothing stuck. A file none of whose reads has
   finished yet gets one read at a time, so finding out that a new file is
   slow costs one read, not all of them. When no open file has a chunk to
   give, the next file is opened.

Otherwise it waits for fresh work. So when everything is slow, reads in flight
settle at 16, all stuck, and the flying readers wait for fresh data. With
`--max-stuck 0`, slow files take flying readers' places instead, as every read
does in rclone.

Files go in hash order by default: of the next 1,024 files the walk has found,
the one whose path hashes lowest goes next. Data written together usually sits
together in folders, so this mixes old and new data, and there's usually fresh
work for the flying reads while slow files grind. `--order dfs` copies depth
first with names sorted; `--order dirent` copies depth first as the disk lists
them.

The number of flying reads tunes itself. It starts at 4 (`--readers`) and
adds 4 at a time while each step makes the flying reads at least 5% faster;
when one doesn't, it goes back. Small steps stop close to where more reads stop
paying, and every flying read too many is one more read that can get stuck
when old data turns up. Every 30 seconds it tries 4 more and 4 fewer, each
against a fresh measurement of the current number, and moves only to one that
is at least 5% faster. A measurement lasts at least a second and 20
flying reads. Only flying reads are measured. They all read fresh data at
about the same speed, so a plain before-and-after comparison holds. Throughput
counting every read would mostly measure which slow files happened to be open.

Whether more reads in flight speed up a degraded drive's slow regions depends
on its controller. If error correction runs on each flash die separately, more
reads overlap it, and the room for 16 stuck reads pays off. If the controller
recovers one read at a time, they gain nothing, and fast reads wait behind them
inside the drive (and then count as stuck too). A USB bridge that doesn't queue
commands allows one read at a time, whatever qclone asks for.

## The display

A full-screen ASCII display, redrawn four times a second:

- an overall progress bar, counts and ETA
- a log-scale speed chart (2 MB/s and 500 MB/s on one chart), colored by how
  slow each moment was, with the reads in flight charted beneath it
- the reads in flight, flying and stuck, and the tuner's test in progress or
  its last verdict
- the source and destination disks (model, interface, rates, time per read,
  queue depth, the disk's own error and retry counters)
- one progress bar per file in flight
- the error count, the error file, and the last error

When stdout isn't a terminal (a pipe, a log), the display goes to `/dev/tty`,
and stdout gets a status line of counts and rates every 10 seconds. File names
and paths are never written to stdout, so a log of a run holds none.

## Flags

```
-j, --readers N      flying reads at the start (default 4); raised 4 at a time
                     while that makes them faster
    --max-readers N  most flying reads, if you want a limit (default: none;
                     they're raised only while that pays; 256 at most)
    --max-stuck N    the most reads stuck over 50 ms, in room of their own
                     beside the flying ones (default 16); 0: no room of their
                     own, they take flying reads' places, as in rclone
    --chunk SIZE     bytes per read, e.g. 256K, 4M (default 256K)
    --order ORDER    hash (default), dfs or dirent: the order files go in
    --fixed          don't tune: keep --readers as given
    --errors FILE    where errors go (default: <timestamp>-qclone-errors.txt here)
    --rescue         install files with unreadable spots, zeros there (listed in the
                     error file; a later run tries those spots again)
    --cached         read and write through the OS file cache
    --plain          no full-screen display: a status line every 10 seconds
    --no-color       no colors (NO_COLOR is honored too)
    --simulate degraded|failing
                     pretend the source is a degraded SSD (for a demo or a test);
                     "failing" adds some sectors that can't be read
```

Flags may go anywhere on the line, as with rclone. Ctrl-C stops cleanly: reads
already in flight finish, and partial files are removed. A second Ctrl-C quits
at once.

## Installing

```
brew install qrux/tap/qclone
```

Homebrew builds it from the release's source on your Mac (it needs Go only to
build). Releases are published with `make release VERSION=x.y.z`; see
`packaging/homebrew/`.

## Building and trying it

```
make build       # build/qclone
make test        # vet and all tests, with the race detector
make demo        # ~1 GB copy on a simulated degraded SSD, to watch the tuner work
make demo-fixed  # the same copy the way rclone does it: 4 reads x 1M, no room for stuck ones
make demo-rescue # a failing drive: a plain pass, then a --rescue pass
make install     # copy qclone to ~/bin
make release VERSION=x.y.z   # tag, and publish the formula to the Homebrew tap
make brew-check  # install from the tap exactly as a user would
```

The simulated drive (`--simulate degraded`) treats any path component starting
with "old" as slow (0.6 MB/s per read, 8 MB/s for the whole drive), along with
about 30% of the 32 MiB regions inside big files. Everything else is fast (500
MB/s). Data really is copied; only the timing is simulated. `--simulate failing`
also makes about one 4 KiB sector in 6000 of the slow regions unreadable (each
failed read takes 200 ms, as on a real drive).

## Limits

- Local to local only: no cloud remotes.
- macOS first. The disk counters use IOKit; elsewhere qclone runs without them.
- Not copied: extended attributes, ACLs, resource forks, Finder tags, file
  flags, ownership, hard links (each link becomes its own file), and symlink
  times.
- A destination file with unreadable spots stays unfinished until you add
  `--rescue`. The hidden `.part`, `.map` and `.bad` files stay in the
  destination until the file is whole.

## License

Copyright © 2026 Qrux. qclone was created by Qrux.

Licensed under the Apache License, Version 2.0 ([LICENSE](LICENSE)). Anyone who
distributes qclone, or a version changed from it, must include the attribution in
[NOTICE](NOTICE) (section 4(d) of the license).
