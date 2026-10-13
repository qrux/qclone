package main

import (
	"strings"
	"testing"
)

func TestVersionFlag(t *testing.T) {
	if code := run([]string{"--version"}); code != 0 {
		t.Fatalf("--version exit status %d", code)
	}
}

func TestVersionString(t *testing.T) {
	saved := version
	defer func() { version = saved }()
	version = "0.1.0"
	if got := versionString(); got != "qclone 0.1.0" {
		t.Fatalf("versionString() = %q", got)
	}
	version = "dev"
	if got := versionString(); !strings.HasPrefix(got, "qclone dev") {
		t.Fatalf("versionString() = %q", got)
	}
}
