# tclone

A local copy tool in the spirit of `rclone copy`, built for a failing SSD: a
drive whose old, never-rewritten data needs so much error correction that some
regions read at 2 MB/s while freshly written ones still read at 500 MB/s.

```
tclone [flags] SRC [SRC...] DST
```

- **Unix source convention.** `tclone /path/to/source DST` copies the directory
  itself, to `DST/source/...`. `tclone /path/to/source/ DST` copies what's
  inside it, to `DST/...`. A trailing `/.` or a source of `.` also means
  contents. A file source lands in `DST` under its own name; a symlink source
  is copied as a link, unless it has a trailing slash.
- **Many sources.** Each one is resolved by the rule above. If two of them would
  write the same file, the second one is skipped and logged.
- **Like `rclone copy`:** files already at the destination with the same size and
  modification time (within 1 s) are skipped, so a rerun picks up where the last
  one stopped. Each file is written to a hidden `.tclone-*.part` file next to its
  destination and renamed into place only when complete. Modes, modification
  times, symlinks and empty directories are copied.
- **Errors never go to the terminal.** They go to `<timestamp>-tclone-errors.txt`
  in the current directory (or `--errors FILE`). The file is created only when
  the first error happens. Exit status: 0 done, 1 some errors, 2 bad arguments,
  130 stopped.

## The rate never reads zero

A meter that counts bytes in the last second shows 0 B/s whenever one read
takes longer than a second, and on a degraded drive single reads can take
several. tclone measures from a baseline instead: the window reaches back at
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

## It tunes itself

tclone controls two things: how many reads are in flight (readers) and how big
each one is (chunk). Every file is cut into chunks in order, and the readers take
chunks from wherever the queue is. So a big file is read by several readers at
once, and small files are copied side by side, both under the same setting.

The tuner sets both by experiment. The difficulty is that the drive's own speed
swings 250x between regions, which drowns out the effect of any one setting.
So the tuner never compares a setting with the past. Each round runs five short
arms in the order A C A C A, where A is the current setting and C is a
candidate. Each C is scored against the mean of the A arms on either side,
which cancels any steady drift in the drive's speed. A round whose A arms
disagree badly (the copy moved to a different region mid-test) doesn't count.
When A C A alone already shows a 25% difference, the round ends early.

- More readers must buy at least 5% to be kept. Fewer readers are kept if they
  cost no more than 2%: fewer reads in flight is easier on a struggling drive.
  At the start, and after moving into a slower region, the reader count
  doubles each step until a step stops paying, then refines.
- A bigger chunk must buy 3%. A smaller one is kept if it costs no more than 2%.
  The chunk only grows while reads finish in under half a second, because
  bigger reads in a slow region make every reaction slower.
- Throughput in each arm is measured "fluidly": each read+write is spread over
  its own start-to-end interval, and an arm counts only the part inside it.
  Measuring starts two read-generations after a change. With only one, reads
  issued during the switch sit behind a queue that is still growing, and any
  deeper queue looks 15-20% better than it is.
- It remembers regions by speed, and the best setting found in each. On
  re-entering a slow region it jumps straight to what worked there before.
- It reads the source disk's counters straight from IOKit (the same numbers
  `iostat` prints, without spawning it). If the disk read much more than tclone
  did during an arm, something else was using it, and the arm doesn't count.

Whether more reads in flight help a degraded drive depends on its controller.
If error correction runs on each flash die separately, more reads in flight
overlap it and the slow regions speed up. If the controller handles error
recovery one read at a time, nothing helps, and the tuner settles at a low
count. The same happens behind a USB bridge that doesn't queue commands. Either
way it is measured, not assumed.

## The display

A full-screen ASCII display, redrawn four times a second:

- an overall progress bar, counts and ETA
- a log-scale speed chart (2 MB/s and 500 MB/s on one chart), colored by how
  slow each moment was, with the reader count charted beneath it
- the tuner: the setting now, the arms of the test in progress, its last
  verdict, and the regions it has learned
- the source and destination disks (model, interface, rates, time per read,
  queue depth, the disk's own error and retry counters)
- one progress bar per file in flight
- the error count, the error file, and the last error

When stdout isn't a terminal (a pipe, a log), the display goes to `/dev/tty`,
and stdout gets a status line of counts and rates every 10 seconds. File names
and paths are never written to stdout, so a log of a run holds none.

## Flags

```
-j, --readers N      reads in flight at the start (default 4)
    --max-readers N  most reads in flight the tuner may use (default 32)
    --chunk SIZE     bytes per read, e.g. 256K, 4M; fixes it (default: tuned, from 256K)
    --fixed          don't tune: keep --readers and --chunk as given
    --errors FILE    where errors go (default: <timestamp>-tclone-errors.txt here)
    --cached         read and write through the OS file cache
    --plain          no full-screen display: a status line every 10 seconds
    --no-color       no colors (NO_COLOR is honored too)
    --simulate degraded
                     pretend the source is a degraded SSD (for a demo or a test)
```

Flags may go anywhere on the line, as with rclone. Ctrl-C stops cleanly: reads
already in flight finish, and partial files are removed. A second Ctrl-C quits
at once.

## Building and trying it

```
make build       # build/tclone
make test        # vet and all tests, with the race detector
make demo        # ~1 GB copy on a simulated degraded SSD, to watch the tuner work
make demo-fixed  # the same copy at a fixed 4 readers x 1M, for comparison
make install     # copy tclone to ~/bin
```

The simulated drive (`--simulate degraded`) treats any path component starting
with "old" as slow (0.6 MB/s per read, 8 MB/s for the whole drive), along with
about 30% of the 32 MiB regions inside big files. Everything else is fast (500
MB/s). Data really is copied; only the timing is simulated.

## Limits

- Local to local only: no cloud remotes.
- macOS first. The disk counters use IOKit; elsewhere tclone runs without them.
- Not copied: extended attributes, ACLs, resource forks, Finder tags, file
  flags, ownership, hard links (each link becomes its own file), and symlink
  times.
- A file whose read fails after 3 tries fails as a whole: no partial rescue,
  no zero-fill. An interrupted big file starts over on the next run.
