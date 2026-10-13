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

## Fresh data flows while old data grinds

Every file is cut into chunks (256 KiB; `--chunk` changes it), and a pool of
readers takes chunks as they come free. So a big file is read by several
readers at once, and small files are copied side by side.

Each 32 MiB of a file is a **region**, judged on its own by its reads. A read
of a healthy SSD takes about a millisecond; old data on a degraded one takes
hundreds of milliseconds, or seconds. A region is **unknown** until a read of
it comes back, **fresh** while its reads come back within 50 ms, and **slow**
(quarantined) once one doesn't. Data written together sits together and ages
together, so one read is a good guess for a region, while a big file (a disk
image, a database) can hold old and fresh regions side by side.

Three numbers govern what's in flight:

- **Q, the queue:** the most reads in flight in all, so nothing piles up
  waiting in macOS. It's measured before copying starts: qclone reads 4 KiB
  pieces of the first fresh file it finds at 1, 2, 4 ... 64 at a time, and Q is
  where more stops adding reads per second. A drive reports its own queue
  length, but an enclosure's bridge can take fewer, or pass commands on one at
  a time; measuring covers every layer at once. With no fresh data found
  within 5 seconds, Q is 32, a SATA drive's queue. `--queue N` sets it.
- **R, the slow lane:** how many reads of slow regions go on at once while
  there's fresh work too. It's measured like the flying reads below, on slow
  reads, from 4, and is at most Q - 4, so a few slots are always left for fresh
  data. `--slow-lane N` sets it.
- **W, the flying reads:** reads of fresh regions (below).

A reader that comes free goes, in this order:

1. **The slow lane:** while fewer than R slow reads are out, a slow region's
   next chunk. These R slots are kept for slow regions whenever they have
   work.
2. **Flying reads:** while fewer than W are out, a fresh region's next chunk.
   While fresh work waits, nothing else goes out ahead of it, so slow reads
   back off to R as they finish (no read is ever canceled).
3. **Tries,** when there's no fresh work: the first read of a region nobody
   has read yet, breadth first. Every open file's first region, then a new
   file's, then a level deeper in the open files: their middles, then their
   quarters, and so on. So a few tries sample whole files before any part is
   looked at closely.
4. **More slow reads,** in every slot left, but only while nothing fresh is
   known and no try that might turn out fresh is still out. Until fresh data
   turns up, a full queue costs nothing and spares the drive waiting on qclone
   between reads.

When everything left is slow, qclone rides the full queue to the end.
`--no-lanes` turns all of this off and reads the way rclone does, to compare:
`--readers` files at once, one read each, in order.

Files go in hash order by default: of the next 1,024 files the walk has found,
the one whose path hashes lowest goes next. Data written together usually sits
together in folders, so this mixes old and new data, and tries find fresh work
sooner. `--order dfs` copies depth first with names sorted; `--order dirent`
copies depth first as the disk lists them.

W and R tune themselves the same way. Each starts at 4 and adds 4 at a time
while each step makes its own reads at least 5% faster; when one doesn't, it
goes back. Small steps stop close to where more reads stop paying. Every so
often (30 s for W, 60 s for R) each tries 4 more and 4 fewer, against a fresh
measurement of the current number, and moves only to one that is at least 5%
faster. W is judged only on flying reads, which all read fresh data at about
the same speed, so a plain before-and-after comparison holds; throughput
counting every read would mostly measure which slow files happened to be open.
R is judged on the slow lane's reads, and only while it's in force (fresh and
slow work both waiting). Slow reads finish in lumps seconds apart, so each of
its measurements takes at least 3 seconds and 20 reads.

Whether more reads in flight speed up a degraded drive's old data depends on
its controller. If error correction runs on each flash die separately, more
reads overlap it, and R climbs. If the controller recovers one read at a time,
R stays low, and fresh reads keep most of the queue.

## The display

A full-screen ASCII display, redrawn four times a second:

- an overall progress bar, counts and ETA
- a log-scale speed chart (2 MB/s and 500 MB/s on one chart), colored by how
  slow each moment was, with the reads in flight charted beneath it
- the reads in flight in each lane (flying, slow, trying), the queue and how
  it was found, the slow regions waiting, and each tuner's test in progress or
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
    --slow-lane N    keep N reads of slow regions going (default: measured, from
                     4, at most the queue less 4); 0: no slow lane
    --queue N        most reads in flight in all (default: measured at the
                     start on fresh data, else 32)
    --no-lanes       read every region alike, files in order, --readers at a
                     time with one read each (rclone's way, to compare)
    --chunk SIZE     bytes per read, e.g. 256K, 4M (default 256K)
    --order ORDER    hash (default), dfs or dirent: the order files go in
    --fixed          don't tune: keep --readers and --slow-lane as given
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
make demo-fixed  # the same copy the way rclone does it: 4 files at once, one 1M read each
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
