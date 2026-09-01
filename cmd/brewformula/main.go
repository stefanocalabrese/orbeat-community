// Command brewformula renders the Homebrew formula that installs orbeat-sync
// (see internal/brewformula for the rendering logic and the empirical
// findings behind its shape). It writes the formula to stdout by default.
// Unlike marketplacegen, this generator's output does not live in this repo
// at a committed path: the formula belongs in the separate tap repo
// stefanocalabrese/homebrew-orbeat, so there is no default file location to
// assume. Pass -out to write to a path instead (e.g. the tap's
// Formula/orbeat-sync.rb after cloning it).
package main

import (
	"flag"
	"log/slog"
	"os"

	"github.com/stefanocalabrese/orbeat-community/internal/brewformula"
	"github.com/stefanocalabrese/orbeat-community/internal/logging"
)

func main() {
	// Logs go to stderr, not stdout: stdout carries the rendered formula
	// itself when -out is unset, and mixing log lines into that would corrupt
	// the piped output.
	slog.SetDefault(logging.New(os.Stderr, "text", "info"))

	version := flag.String("version", "", "release version, e.g. v1.26.0 or 1.26.0 (required)")
	checksumsPath := flag.String("checksums", "", "path to a checksums.txt in sha256sum output format (required)")
	baseURL := flag.String("base-url", "", "release-assets download root (default: internal/brewformula.DefaultBaseURL)")
	out := flag.String("out", "", "output path for the rendered formula (default: stdout)")
	flag.Parse()

	// A stray positional argument (this command takes none) must not be
	// silently ignored: v1.26.0 made orbeat-sync itself reject exactly this
	// shape of mistake as BREAKING, for the same reason - a command that
	// quietly does something other than what was typed is worse than one
	// that refuses to run.
	if flag.NArg() > 0 {
		slog.Error("generate brew formula", "err", "unexpected positional argument(s)", "args", flag.Args())
		os.Exit(1)
	}

	if *version == "" {
		slog.Error("generate brew formula", "err", "-version is required")
		os.Exit(1)
	}
	if *checksumsPath == "" {
		slog.Error("generate brew formula", "err", "-checksums is required")
		os.Exit(1)
	}

	data, err := os.ReadFile(*checksumsPath)
	if err != nil {
		slog.Error("read checksums", "path", *checksumsPath, "err", err)
		os.Exit(1)
	}

	checksums, err := brewformula.ParseChecksums(data)
	if err != nil {
		slog.Error("parse checksums", "path", *checksumsPath, "err", err)
		os.Exit(1)
	}

	formula, err := brewformula.Render(brewformula.Options{
		Version:   *version,
		BaseURL:   *baseURL,
		Checksums: checksums,
	})
	if err != nil {
		slog.Error("render formula", "err", err)
		os.Exit(1)
	}

	if *out == "" {
		if _, err := os.Stdout.WriteString(formula); err != nil {
			slog.Error("write formula", "err", err)
			os.Exit(1)
		}
		return
	}

	if err := os.WriteFile(*out, []byte(formula), 0o644); err != nil {
		slog.Error("write formula", "out", *out, "err", err)
		os.Exit(1)
	}
	slog.Info("wrote formula", "out", *out, "version", *version)
}
