# qclone: every recurring command is a target here. Run `make` for the list.
#
# Recorded command lines use repo-relative paths and ~, never the home folder's name.
#
# Each target's output shows live on the terminal and is saved by the
# project's log tool (log= in .treeflow/project-type; runlog) to
# _logs/<target>-<UTC stamp>.log at the repo root; say kk when it finishes.
# _logs must be the link into ~/jorp/qclone/logs (make -C ~/jorp proj-qclone).
#
# Nothing here prints private paths: the tests and the demo use generated
# files, and qclone itself never prints a file name to stdout (names go to
# the terminal's /dev/tty only).

SHELL := /bin/bash
ROOT  := $(patsubst %/,%,$(dir $(abspath $(firstword $(MAKEFILE_LIST)))))
# Go is a Homebrew install; the log tools are in ~/bin (treeflow).
export PATH := /opt/homebrew/bin:$(HOME)/bin:$(PATH)
LOG    := $(shell sed -n 's/^log=//p' $(ROOT)/.treeflow/project-type)
LOGGED  = $(LOG) -n $@ --
DEMO   := build/demo
# -trimpath and -s -w keep this machine's paths (and so the home folder's name)
# out of the binaries: no source paths, no symbol or debug tables.
GOBUILD = go build -trimpath -ldflags "-s -w"
.DEFAULT_GOAL := help
.PHONY: help build test demo demo-fixed demo-rescue install license-probe

help:
	@grep -E '^## ' $(firstword $(MAKEFILE_LIST)) | sed 's/^## /  /'

## build: build build/qclone (and the demo-data generator)
build:
	@cd $(ROOT) && $(LOGGED) sh -c '$(GOBUILD) -o build/qclone ./cmd/qclone && $(GOBUILD) -o build/mkdemo ./cmd/mkdemo && echo built build/qclone'

## test: go vet, then every test with the race detector (includes copies against a simulated
##   degraded SSD; about 40 seconds)
test:
	@cd $(ROOT) && $(LOGGED) sh -c 'go vet ./... && go test -race -count=1 ./...'

## demo: copy about 1 GB of generated files with --simulate degraded, so you can watch the
##   display and the tuner at work on a pretend failing SSD (about a minute). The tuning
##   display draws on the terminal; the log gets a status line every 10 s.
demo:
	@cd $(ROOT) && $(LOGGED) sh -c '$(GOBUILD) -o build/qclone ./cmd/qclone && $(GOBUILD) -o build/mkdemo ./cmd/mkdemo && build/mkdemo $(DEMO)/src && rm -rf $(DEMO)/dst && build/qclone --simulate degraded --errors $(DEMO)/errors.txt $(DEMO)/src/ $(DEMO)/dst'

## demo-fixed: the same copy at rclone's default of 4 readers, never tuned, to compare
demo-fixed:
	@cd $(ROOT) && $(LOGGED) sh -c '$(GOBUILD) -o build/qclone ./cmd/qclone && $(GOBUILD) -o build/mkdemo ./cmd/mkdemo && build/mkdemo $(DEMO)/src && rm -rf $(DEMO)/dst && build/qclone --simulate degraded --fixed --readers 4 --chunk 1M --errors $(DEMO)/errors.txt $(DEMO)/src/ $(DEMO)/dst'

## demo-rescue: the copy from a simulated failing SSD that has some unreadable sectors: once
##   plain (unreadable spots mapped to 4 KiB, those files kept unfinished), then again with
##   --rescue (only the spots are read again; the files are installed with zeros there). Both
##   runs end with status 1, as any run with unreadable spots does; that is expected here.
demo-rescue:
	@cd $(ROOT) && $(LOGGED) sh -c '$(GOBUILD) -o build/qclone ./cmd/qclone && $(GOBUILD) -o build/mkdemo ./cmd/mkdemo && build/mkdemo $(DEMO)/src && rm -rf $(DEMO)/dst-rescue && { build/qclone --simulate failing --errors $(DEMO)/errors-1.txt $(DEMO)/src/ $(DEMO)/dst-rescue || test $$? -eq 1; } && { build/qclone --simulate failing --rescue --errors $(DEMO)/errors-2.txt $(DEMO)/src/ $(DEMO)/dst-rescue || test $$? -eq 1; }'

## install: build, then copy qclone to ~/bin/qclone
install:
	@cd $(ROOT) && $(LOGGED) sh -c '$(GOBUILD) -o build/qclone ./cmd/qclone && install -m 755 build/qclone ~/bin/qclone && echo installed ~/bin/qclone'

## license-probe: which licenses Homebrew's tools use (all, and Go-built), qclone's third-party
##   Go modules, and each candidate license's length; writes probe/licenses/out/ (network)
license-probe:
	@cd $(ROOT) && $(LOGGED) python3 -I probe/licenses/probe.py
