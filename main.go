package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"wherobots/cli/internal/auth"
	"wherobots/cli/internal/commands"
	"wherobots/cli/internal/config"
	"wherobots/cli/internal/executor"
	"wherobots/cli/internal/spec"
	"wherobots/cli/internal/version"
)

var (
	buildVersion = "dev"
	commit       = "none"
	date         = "unknown"
)

func main() {
	if err := run(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	// Surface the build version to the executor so outgoing requests carry the
	// advisory X-Wherobots-Client header. Set here (not via ldflags directly on
	// the executor var) to keep version wiring in one place.
	executor.Version = buildVersion

	// Start a background update check early so it runs in parallel with setup.
	updateCh := version.CheckInBackground(ctx, buildVersion)

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	creds := auth.NewResolver(cfg)

	versionString := fmt.Sprintf("%s (commit %s, built %s)", buildVersion, commit, date)
	decorate := func(root *cobra.Command) {
		root.Version = versionString
		root.Short = fmt.Sprintf("Wherobots CLI %s", buildVersion)
	}

	// Spec-free commands (auth, upgrade) run off a bare root so they work on
	// a fresh machine: no credentials, no cached spec, no API connectivity.
	bare := commands.BuildBareRootCommand(cfg)
	decorate(bare)
	commands.AddAuthCommand(bare, cfg, creds)
	commands.AddUpgradeCommand(bare, buildVersion)

	// Suppress the "update available" notice when the user is already running
	// `upgrade` — the check races with the upgrade itself and would otherwise
	// nag about the very version that just got installed.
	suppressNotice := commands.IsUpgradeInvocation(bare, os.Args[1:])

	var execErr error
	if commands.IsSpecFreeInvocation(bare, os.Args[1:]) {
		execErr = bare.ExecuteContext(ctx)
	} else {
		execErr = runWithSpec(ctx, cfg, creds, decorate)
	}

	if !suppressNotice {
		if result := version.Collect(updateCh); result != nil {
			fmt.Fprintln(os.Stderr, "")
			printUpdateNotice(os.Stderr, result)
			if execErr != nil {
				fmt.Fprintln(os.Stderr, "Note: your CLI is out of date. Run `wherobots upgrade` to update — it may resolve this issue.")
			}
		}
	}

	return execErr
}

// runWithSpec loads the OpenAPI spec and dispatches through the full
// dynamically-generated command tree.
func runWithSpec(ctx context.Context, cfg config.Config, creds *auth.Resolver, decorate func(*cobra.Command)) error {
	loader := spec.NewLoader(cfg, creds)
	rawSpec, err := loader.Load(ctx)
	if err != nil {
		return err
	}

	runtimeSpec, err := spec.Parse(rawSpec, cfg.OpenAPIURL)
	if err != nil {
		return err
	}

	root := commands.BuildRootCommand(cfg, creds, runtimeSpec)
	decorate(root)
	// Also present on the full tree so help and --tree list them.
	commands.AddAuthCommand(root, cfg, creds)
	commands.AddUpgradeCommand(root, buildVersion)

	return root.ExecuteContext(ctx)
}

func printUpdateNotice(w io.Writer, r *version.Result) {
	notice := version.FormatNotice(r)
	if isTTY(w) {
		notice = "\033[1;33m" + notice + "\033[0m"
	}
	fmt.Fprintln(w, notice)
}

// isTTY reports whether w is an *os.File pointing at a terminal. Stdlib only;
// avoids pulling in a tty-detection dependency for a single notice.
func isTTY(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}
