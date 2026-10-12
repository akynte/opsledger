// Command ledger is opsledger: it measures a running service from its
// reverse proxy's request stream, publishes daily operational reports that
// anyone can recompute, and verifies signed releases and reports.
//
//	ledger serve                          run the collector and its read-only API (configured by environment)
//	ledger healthcheck                    probe the local admin /readyz (container health check)
//	ledger version                        print build information
//	ledger probe        [flags]           check the public site from outside and write one probe-run record
//	ledger assemble     [flags]           combine an origin day and probe runs into a daily report
//	ledger summary      [flags]           build the rolling summary from daily reports
//	ledger recompute    DIR               verify every report under DIR: schemas, consistency, recomputed values
//	ledger verify-bundle [flags]          verify a Sigstore bundle over a file, and its GitHub signer
//	ledger verify-release [flags]         verify a signed release manifest against the images being deployed
//	ledger day          [flags]           write the origin report of one day from a database (offline)
//
// Exit codes: 0 success, 1 failure or discrepancy, 2 usage or configuration error.
package main

import (
	"fmt"
	"io"
	"os"
	"runtime"
)

// Set with -ldflags "-X main.version=... -X main.commit=...".
var (
	version = "dev"
	commit  = "unknown"
)

const (
	exitOK      = 0
	exitFailure = 1
	exitUsage   = 2
)

func main() {
	os.Exit(run(os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}

func run(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return exitUsage
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "serve":
		return serve(rest, getenv, stdout, stderr)
	case "healthcheck":
		return healthcheck(getenv, stderr)
	case "version", "-version", "--version":
		fmt.Fprintf(stdout, "opsledger %s (commit %s, %s)\n", version, commit, runtime.Version())
		return exitOK
	case "probe":
		return probeCmd(rest, getenv, stdout, stderr)
	case "assemble":
		return assembleCmd(rest, stdout, stderr)
	case "summary":
		return summaryCmd(rest, stdout, stderr)
	case "recompute":
		return recomputeCmd(rest, stdout, stderr)
	case "verify-bundle":
		return verifyBundleCmd(rest, stdout, stderr)
	case "verify-release":
		return verifyReleaseCmd(rest, stdout, stderr)
	case "day":
		return dayCmd(rest, stdout, stderr)
	case "help", "-h", "--help":
		usage(stdout)
		return exitOK
	}
	fmt.Fprintf(stderr, "ledger: unknown command %q\n", cmd)
	usage(stderr)
	return exitUsage
}

func usage(w io.Writer) {
	fmt.Fprint(w, `usage: ledger <command> [flags]

commands:
  serve            run the collector and its read-only API (environment configuration)
  healthcheck      exit 0 if the local service is ready
  version          print build information
  probe            check the public site from outside; write a probe-run record
  assemble         combine an origin day and probe runs into a daily report
  summary          build the rolling summary from daily reports
  recompute DIR    verify every report under DIR
  verify-bundle    verify a Sigstore bundle over a file and its GitHub signer
  verify-release   verify a signed release manifest against the images being deployed
  day              write one day's origin report from a database (offline)

Run "ledger <command> -h" for a command's flags.
`)
}
