# tclone: every recurring command is a target here. Run `make` for the list.
#
# Recorded command lines use repo-relative paths and ~, never the home folder's name.
#
# Each target's output shows live on the terminal and is saved by the
# project's log tool (log= in .treeflow/project-type; runlog) to
# _logs/<target>-<UTC stamp>.log at the repo root; say kk when it finishes.
# _logs must be the link into ~/jorp/tclone/logs (make -C ~/jorp proj-tclone).
#
# Nothing here prints private paths: the tests and the demo use generated
# files, and tclone itself never prints a file name to stdout (names go to
# the terminal's /dev/tty only).

SHELL := /bin/bash
ROOT  := $(patsubst %/,%,$(dir $(abspath $(firstword $(MAKEFILE_LIST)))))
# Go is a Homebrew install; the log tools are in ~/bin (treeflow).
export PATH := /opt/homebrew/bin:$(HOME)/bin:$(PATH)
LOG    := $(shell sed -n 's/^log=//p' $(ROOT)/.treeflow/project-type)
LOGGED  = $(LOG) -n $@ --
DEMO   := build/demo
.DEFAULT_GOAL := help
.PHONY: help build test demo demo-fixed install

help:
	@grep -E '^## ' $(firstword $(MAKEFILE_LIST)) | sed 's/^## /  /'

## build: build build/tclone (and the demo-data generator)
build:
	@cd $(ROOT) && $(LOGGED) sh -c 'go build -o build/tclone ./cmd/tclone && go build -o build/mkdemo ./cmd/mkdemo && echo built build/tclone'

## test: go vet, then every test with the race detector (includes copies against a simulated
##   degraded SSD; about 40 seconds)
test:
	@cd $(ROOT) && $(LOGGED) sh -c 'go vet ./... && go test -race -count=1 ./...'

## demo: copy about 1 GB of generated files with --simulate degraded, so you can watch the
##   display and the tuner at work on a pretend failing SSD (about a minute). The tuning
##   display draws on the terminal; the log gets a status line every 10 s.
demo:
	@cd $(ROOT) && $(LOGGED) sh -c 'go build -o build/tclone ./cmd/tclone && go build -o build/mkdemo ./cmd/mkdemo && build/mkdemo $(DEMO)/src && rm -rf $(DEMO)/dst && build/tclone --simulate degraded --errors $(DEMO)/errors.txt $(DEMO)/src/ $(DEMO)/dst'

## demo-fixed: the same copy at rclone's default of 4 readers, never tuned, to compare
demo-fixed:
	@cd $(ROOT) && $(LOGGED) sh -c 'go build -o build/tclone ./cmd/tclone && go build -o build/mkdemo ./cmd/mkdemo && build/mkdemo $(DEMO)/src && rm -rf $(DEMO)/dst && build/tclone --simulate degraded --fixed --readers 4 --chunk 1M --errors $(DEMO)/errors.txt $(DEMO)/src/ $(DEMO)/dst'

## install: build, then copy tclone to ~/bin/tclone
install:
	@cd $(ROOT) && $(LOGGED) sh -c 'go build -o build/tclone ./cmd/tclone && install -m 755 build/tclone ~/bin/tclone && echo installed ~/bin/tclone'
