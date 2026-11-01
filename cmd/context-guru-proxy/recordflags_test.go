package main

import (
	"bytes"
	"flag"
	"log/slog"
	"strings"
	"testing"

	"github.com/rossoctl/context-guru/proxy"
)

func captureWarnings(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

func TestWarnIfRecorderMissing(t *testing.T) {
	t.Run("no recorder and --record set: one warning", func(t *testing.T) {
		buf := captureWarnings(t)
		if !warnIfRecorderMissing(false, "/tmp/x", "", nil) {
			t.Fatal("want a warning")
		}
		if n := strings.Count(buf.String(), noRecorderWarning); n != 1 {
			t.Fatalf("want exactly 1 warning, got %d: %s", n, buf.String())
		}
	})
	t.Run("no recorder and --record-otlp set", func(t *testing.T) {
		captureWarnings(t)
		if !warnIfRecorderMissing(false, "", "http://localhost:6006/v1/traces", nil) {
			t.Fatal("want a warning")
		}
	})
	t.Run("no recorder and only --record-queue given", func(t *testing.T) {
		captureWarnings(t)
		fs := flag.NewFlagSet("x", flag.ContinueOnError)
		fs.Int("record-queue", 256, "")
		if err := fs.Parse([]string{"-record-queue", "8"}); err != nil {
			t.Fatal(err)
		}
		if !warnIfRecorderMissing(false, "", "", fs) {
			t.Fatal("want a warning")
		}
	})
	t.Run("no record flag: silent", func(t *testing.T) {
		buf := captureWarnings(t)
		if warnIfRecorderMissing(false, "", "", flag.NewFlagSet("x", flag.ContinueOnError)) || buf.Len() != 0 {
			t.Fatalf("want silence, got %q", buf.String())
		}
	})
	t.Run("recorder built in: silent", func(t *testing.T) {
		buf := captureWarnings(t)
		if warnIfRecorderMissing(true, "/tmp/x", "http://c", nil) || buf.Len() != 0 {
			t.Fatalf("want silence, got %q", buf.String())
		}
	})
}

// The build in use must agree with the tag: this is the stub's contract with main.
func TestUntaggedBuildStartsWithRecordSet(t *testing.T) {
	if proxy.RecorderBuiltIn {
		t.Skip("recorder build")
	}
	captureWarnings(t)
	r, err := proxy.NewSessionRecorder("/tmp/x", "http://c", 0, 0, 0, slog.Default())
	if r != nil || err != nil {
		t.Fatalf("stub must return nil, nil; got %v, %v", r, err)
	}
	if !warnIfRecorderMissing(proxy.RecorderBuiltIn, "/tmp/x", "", nil) {
		t.Fatal("want a warning")
	}
}
