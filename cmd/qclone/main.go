// qclone copies directories like rclone copy, but tunes its own parallelism
// by experiment, shows a rate that never reads zero while data is moving, and
// keeps errors out of the terminal.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"

	"qclone/internal/copier"
	"qclone/internal/plan"
	"qclone/internal/sim"
	"qclone/internal/tuner"
	"qclone/internal/ui"
)

const usage = `usage: qclone [flags] SRC [SRC...] DST

Copies each SRC into the directory DST (created if missing):
  qclone /path/to/source  DST    copies the directory: DST/source/...
  qclone /path/to/source/ DST    copies what's inside it: DST/...
Files already at DST with the same size and modification time are skipped.

flags:
  -j, --readers N      reads in flight at the start (default 4)
      --max-readers N  most reads in flight the tuner may use (default 32)
      --chunk SIZE     bytes per read, e.g. 256K, 4M; fixes it (default: tuned, from 256K)
      --fixed          don't tune: keep --readers and --chunk as given
      --errors FILE    where errors go (default: <timestamp>-qclone-errors.txt here)
      --rescue         install files that have unreadable spots, with zeros there
                       (each spot is listed in the error file; a later run
                       tries those spots again)
      --cached         read and write through the OS file cache
      --plain          no full-screen display: a status line every 10 seconds
      --no-color       no colors
      --simulate degraded|failing
                       pretend the source is a degraded SSD (for a demo or a
                       test); "failing" adds some sectors that can't be read
      --version        print the version and exit
  -h, --help           this help

An interrupted copy resumes: rerun the same command and each file carries on
from where it stopped. A spot the source can't read is narrowed down to 4 KiB;
without --rescue that file isn't installed, and a rerun retries just its
unreadable spots.

Errors never go to the terminal; they go to the error file, which is only
created if there is an error. Exit status: 0 done, 1 some errors, 2 bad
arguments, 130 stopped.
`

func main() { os.Exit(run(os.Args[1:])) }

// version is set at build time: -ldflags "-X main.version=0.1.0" (the
// Makefile and the Homebrew formula both do). A plain go build leaves "dev".
var version = "dev"

// versionString is "qclone 0.1.0", or for an unreleased build "qclone dev
// (545c42a)" with the commit it was built from, when Go recorded one.
func versionString() string {
	v := "qclone " + version
	if version != "dev" {
		return v
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		rev, dirty := "", false
		for _, kv := range bi.Settings {
			switch kv.Key {
			case "vcs.revision":
				rev = kv.Value
			case "vcs.modified":
				dirty = kv.Value == "true"
			}
		}
		if len(rev) > 7 {
			rev = rev[:7]
		}
		if rev != "" {
			if dirty {
				rev += ", modified"
			}
			v += " (" + rev + ")"
		}
	}
	return v
}

func parseSize(s string) (int, error) {
	t := strings.ToUpper(strings.TrimSpace(s))
	t = strings.TrimSuffix(strings.TrimSuffix(t, "IB"), "B")
	mult := 1
	switch {
	case strings.HasSuffix(t, "K"):
		mult, t = 1<<10, strings.TrimSuffix(t, "K")
	case strings.HasSuffix(t, "M"):
		mult, t = 1<<20, strings.TrimSuffix(t, "M")
	}
	n, err := strconv.Atoi(t)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("bad size %q", s)
	}
	return n * mult, nil
}

func run(args []string) (code int) {
	started := time.Now()
	fs := flag.NewFlagSet("qclone", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var readers int
	fs.IntVar(&readers, "readers", 4, "")
	fs.IntVar(&readers, "j", 4, "")
	maxReaders := fs.Int("max-readers", 32, "")
	chunkFlag := fs.String("chunk", "", "")
	fixed := fs.Bool("fixed", false, "")
	errPath := fs.String("errors", "", "")
	cached := fs.Bool("cached", false, "")
	rescue := fs.Bool("rescue", false, "")
	plainOnly := fs.Bool("plain", false, "")
	noColor := fs.Bool("no-color", false, "")
	simulate := fs.String("simulate", "", "")
	var help bool
	showVersion := fs.Bool("version", false, "")
	fs.BoolVar(&help, "help", false, "")
	fs.BoolVar(&help, "h", false, "")

	// Flags may come anywhere, as with rclone; "--" ends them.
	var pos []string
	rest := args
	for len(rest) > 0 {
		if err := fs.Parse(rest); err != nil {
			fmt.Fprintf(os.Stderr, "qclone: %v\n\n%s", err, usage)
			return 2
		}
		rest = fs.Args()
		if len(rest) == 0 {
			break
		}
		if rest[0] == "--" {
			pos = append(pos, rest[1:]...)
			break
		}
		pos = append(pos, rest[0])
		rest = rest[1:]
	}
	if *showVersion {
		fmt.Println(versionString())
		return 0
	}
	if help {
		fmt.Print(usage)
		return 0
	}
	bad := func(format string, a ...interface{}) int {
		fmt.Fprintf(os.Stderr, "qclone: "+format+"\n", a...)
		return 2
	}
	if *maxReaders < 1 || *maxReaders > 256 {
		return bad("--max-readers must be 1 to 256")
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	if !set["readers"] && !set["j"] && readers > *maxReaders {
		readers = *maxReaders // only the default was over the cap
	}
	if readers < 1 || readers > *maxReaders {
		return bad("--readers must be 1 to --max-readers (%d)", *maxReaders)
	}
	cfg := tuner.DefaultConfig()
	cfg.MaxReaders = *maxReaders
	start := tuner.Setting{Readers: readers, Chunk: 256 << 10}
	if *chunkFlag != "" {
		n, err := parseSize(*chunkFlag)
		if err != nil {
			return bad("%v", err)
		}
		if n < 4<<10 || n > 64<<20 {
			return bad("--chunk must be 4K to 64M")
		}
		start.Chunk = n
		cfg.TuneChunk = false
		cfg.MinChunk, cfg.MaxChunk = n, n
	}
	var dev *sim.Device
	switch *simulate {
	case "":
	case "degraded":
		dev = sim.NewDevice(sim.Degraded())
	case "failing":
		dev = sim.NewDevice(sim.Failing())
	default:
		return bad("--simulate knows \"degraded\" and \"failing\"")
	}
	p, err := plan.Resolve(pos)
	if err != nil {
		fmt.Fprintf(os.Stderr, "qclone: %v\n\n%s", err, usage)
		return 2
	}
	if *errPath == "" {
		*errPath = copier.DefaultErrorPath(started)
	}
	errLog := copier.NewErrorLog(*errPath, args, started)
	defer errLog.Close()

	// Where the display goes: the terminal if stdout is one; otherwise
	// /dev/tty (so file names show on screen but never reach a pipe or log),
	// with a plain status line on stdout.
	var tty *os.File
	if !*plainOnly {
		if ui.IsTerminal(os.Stdout) {
			tty = os.Stdout
		} else if f, err := os.OpenFile("/dev/tty", os.O_WRONLY, 0); err == nil {
			if ui.IsTerminal(f) {
				tty = f
			} else {
				f.Close()
			}
		}
	}
	var plainOut *ui.Plain
	if tty != os.Stdout {
		plainOut = ui.NewPlain(os.Stdout, 10*time.Second)
	}
	color := tty != nil && !*noColor && os.Getenv("NO_COLOR") == "" && os.Getenv("TERM") != "dumb"

	eng := copier.New(p, copier.Options{
		Start:        start,
		Tuner:        cfg,
		Fixed:        *fixed,
		Cached:       *cached,
		ModifyWindow: time.Second,
		Retries:      3,
		Errors:       errLog,
		Sim:          dev,
		Rescue:       *rescue,
	})
	hist := ui.NewHistory(eng.Snapshot().Start)
	var tui *ui.TUI
	// A bug in the display must not leave the terminal unusable or dump a
	// trace on the screen: restore the terminal and put the trace in the
	// error file.
	defer func() {
		if r := recover(); r != nil {
			if tui != nil {
				tui.Restore()
			}
			errLog.Log("panic", "qclone", fmt.Errorf("%v\n%s", r, debug.Stack()))
			errLog.Close()
			fmt.Printf("qclone: stopped by an internal error; details in %s\n", filepath.Base(errLog.Path()))
			code = 1
		}
	}()
	if tty != nil {
		var srcs []string
		for _, r := range p.Roots {
			srcs = append(srcs, r.Arg)
		}
		tui = ui.NewTUI(tty, ui.Frame{Color: color, Sources: srcs, Dst: p.Dst}, hist)
		tui.Start()
		defer tui.Restore()
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		<-sigs
		cancel()
		<-sigs
		if tui != nil {
			tui.Restore()
		}
		errLog.Close()
		os.Exit(130)
	}()

	done := make(chan error, 1)
	go func() { done <- eng.Run(ctx) }()
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	var runErr error
loop:
	for {
		select {
		case runErr = <-done:
			break loop
		case <-tick.C:
			s := eng.Snapshot()
			hist.Record(s.Now, s.Rate, s.RateOK, s.Tuner.Applied.Readers)
			if tui != nil {
				tui.Draw(s)
			}
			if plainOut != nil {
				plainOut.Maybe(s)
			}
		}
	}
	final := eng.Snapshot()
	hist.Record(final.Now, final.Rate, final.RateOK, final.Tuner.Applied.Readers)
	if runErr != nil {
		errLog.Log("start", p.Dst, runErr)
		final = eng.Snapshot()
	}
	if tui != nil {
		tui.Stop(final)
	}
	errLog.Close()
	for _, l := range ui.Summary(final) {
		fmt.Println(l)
	}
	if w := errLog.WriteErr(); w != nil && final.Errors > 0 {
		fmt.Printf("qclone: could not write the error file: %v\n", w)
	}
	switch {
	case final.Canceled:
		return 130
	case runErr != nil || final.Errors > 0:
		return 1
	}
	return 0
}
