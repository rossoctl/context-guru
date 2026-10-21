// cg-record-convert turns recorded OTLP spans into claude-record corpus files.
//
// The proxy's --record and --record-otlp export standard OTLP, which Phoenix, Jaeger and other
// collectors read. Inference-Perf's OTel trace replay and the claude-record analysis scripts read
// a different, flat shape: one <session>.otel.json per session, with the human's turns rebuilt as
// derived spans. This command makes those files, offline, from either:
//
//   - the proxy's own --record directory (one *.otlp.jsonl per session), or
//   - an OTel collector's file exporter output (the same OTLP JSON lines).
//
// Usage:
//
//	cg-record-convert -out <dir> <file-or-dir> [...]
//
// A directory input is searched for *.otlp.jsonl and *.json files. Sessions are grouped by
// session.id across all inputs, so a session split over two files (a proxy restart) becomes one
// corpus file. Existing output files are replaced: conversion is repeatable, and the input is the
// record of truth.
package main

import (
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rossoctl/context-guru/internal/record"
)

func main() {
	out := flag.String("out", "", "output directory for <session>.otel.json corpus files (required)")
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "usage: cg-record-convert -out <dir> <file-or-dir> [...]\n")
		flag.PrintDefaults()
	}
	flag.Parse()
	if *out == "" || flag.NArg() == 0 {
		flag.Usage()
		os.Exit(2)
	}
	if err := run(*out, flag.Args(), time.Now()); err != nil {
		fmt.Fprintln(os.Stderr, "cg-record-convert:", err)
		os.Exit(1)
	}
}

func run(outDir string, inputs []string, now time.Time) error {
	files, err := inputFiles(inputs)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return fmt.Errorf("no *.otlp.jsonl or *.json files in %v", inputs)
	}
	var spans []record.ReadSpan
	for _, f := range files {
		r, err := os.Open(f)
		if err != nil {
			return err
		}
		got, err := record.ReadOTLP(r)
		r.Close()
		if err != nil {
			return fmt.Errorf("%s: %w", f, err)
		}
		spans = append(spans, got...)
	}
	if err := os.MkdirAll(outDir, 0o700); err != nil {
		return err
	}
	sessions := record.GroupSessions(spans)
	for _, sess := range sessions {
		dir := outDir
		if sess.TenantID != "" {
			dir = filepath.Join(outDir, record.SafeFileName(sess.TenantID))
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return err
			}
		}
		b, err := record.ConvertSession(sess, now).Marshal()
		if err != nil {
			return err
		}
		path := filepath.Join(dir, record.SafeFileName(sess.ID)+".otel.json")
		if err := os.WriteFile(path, b, 0o600); err != nil {
			return err
		}
		fmt.Printf("%s  %d spans\n", path, len(sess.Spans))
	}
	fmt.Printf("%d sessions from %d spans in %d files\n", len(sessions), len(spans), len(files))
	return nil
}

func inputFiles(inputs []string) ([]string, error) {
	var out []string
	for _, in := range inputs {
		st, err := os.Stat(in)
		if err != nil {
			return nil, err
		}
		if !st.IsDir() {
			out = append(out, in)
			continue
		}
		err = filepath.WalkDir(in, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.IsDir() && (strings.HasSuffix(p, ".otlp.jsonl") || strings.HasSuffix(p, ".json")) &&
				!strings.HasSuffix(p, ".otel.json") {
				out = append(out, p)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}
