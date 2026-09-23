package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/rand/v2"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"github.com/tidwall/gjson"

	"wherobots/cli/internal/config"
	"wherobots/cli/internal/executor"
	"wherobots/cli/internal/files"
	"wherobots/cli/internal/spec"
)

type filesRunner struct {
	cfg             config.Config
	creds           executor.Credentials
	runtime         *spec.RuntimeSpec
	client          *http.Client
	transfer        *http.Client
	flags           *GlobalFlags
	ops             files.Operations
	getOrganization *spec.Operation
}

// openDrive resolves the drive a verb works on (for my-files: the region).
type openDrive func(cmd *cobra.Command) (*files.DriveClient, error)

func addFilesCommands(root *cobra.Command, cfg config.Config, creds executor.Credentials, runtimeSpec *spec.RuntimeSpec, client *http.Client, flags *GlobalFlags) {
	if runtimeSpec == nil {
		return
	}
	r := &filesRunner{
		cfg:     cfg,
		creds:   creds,
		runtime: runtimeSpec,
		client:  client,
		// Storage transfers have no overall timeout so large files are not cut off.
		transfer: &http.Client{},
		flags:    flags,
		ops: files.Operations{
			ListDirectory:   findOperation(runtimeSpec, "GET", "/storage/{storage_id}/directories/{path}"),
			CreateDirectory: findOperation(runtimeSpec, "PUT", "/storage/{storage_id}/directories/{path}"),
			DeleteDirectory: findOperation(runtimeSpec, "DELETE", "/storage/{storage_id}/directories/{path}"),
			CreateUploadURL: findOperation(runtimeSpec, "POST", "/storage/{storage_id}/file-upload-url/{path}"),
			DownloadFile:    findOperation(runtimeSpec, "GET", "/storage/{storage_id}/files/{path}"),
			DeleteFile:      findOperation(runtimeSpec, "DELETE", "/storage/{storage_id}/files/{path}"),
			RenameFile:      findOperation(runtimeSpec, "POST", "/storage/{storage_id}/file-rename/{path}"),
		},
		getOrganization: findOperation(runtimeSpec, "GET", "/organization"),
	}
	if !r.ops.Complete() {
		return
	}

	filesCmd := &cobra.Command{
		Use:           "files",
		Short:         "Work with files in Wherobots Files",
		SilenceUsage:  true,
		SilenceErrors: true,
		// Stamp the invoked name (e.g. files.my-files.ls) for X-Wherobots-Client,
		// as job-runs does, since these reuse the api-tree storage operations.
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SetContext(executor.WithCommand(cmd.Context(), curatedCommandName(cmd)))
			return nil
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	filesCmd.AddCommand(r.newMyFilesCommand())
	root.AddCommand(filesCmd)
}

func (r *filesRunner) newMyFilesCommand() *cobra.Command {
	var region string
	cmd := &cobra.Command{
		Use:   "my-files",
		Short: "Your personal files area",
		Long: `Your personal files area, one per region.

The region is --region when given, otherwise your organization's default region.
Remote paths are relative to the area's root; a leading "/" is ignored, and a
trailing "/" names a folder.`,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	cmd.PersistentFlags().StringVar(&region, "region", "", "region of the files area (default: your organization's default region)")
	_ = cmd.RegisterFlagCompletionFunc("region", r.completeRegions)

	open := func(cmd *cobra.Command) (*files.DriveClient, error) {
		resolved, err := r.resolveRegion(cmd.Context(), region)
		if err != nil {
			return nil, err
		}
		return r.service(cmd).Open(files.MyFiles(resolved))
	}
	r.addDriveVerbs(cmd, open)
	return cmd
}

// addDriveVerbs attaches every file verb to a drive command. A future shared
// drive reuses this with its own openDrive.
func (r *filesRunner) addDriveVerbs(parent *cobra.Command, open openDrive) {
	parent.AddCommand(
		r.newLsCommand(open),
		r.newMkdirCommand(open),
		r.newUploadCommand(open),
		r.newDownloadCommand(open),
		r.newCatCommand(open),
		r.newMvCommand(open),
		r.newRmCommand(open),
		r.newRmdirCommand(open),
	)
}

func (r *filesRunner) service(cmd *cobra.Command) *files.Service {
	svc := &files.Service{
		Runtime:  r.runtime,
		Creds:    r.creds,
		API:      r.client,
		Transfer: r.transfer,
		Ops:      r.ops,
	}
	if r.flags != nil && r.flags.DryRun {
		svc.DryRun = cmd.OutOrStdout()
	}
	return svc
}

// resolveRegion returns --region, or the organization's defaultRegion.
func (r *filesRunner) resolveRegion(ctx context.Context, flagValue string) (string, error) {
	if region := strings.TrimSpace(flagValue); region != "" {
		return region, nil
	}
	if r.getOrganization == nil {
		return "", fmt.Errorf("no region given: pass --region <region>")
	}
	body, err := r.fetchOrganization(ctx)
	if err != nil {
		var httpErr *executor.HTTPError
		if errors.As(err, &httpErr) && httpErr.StatusCode == http.StatusUnauthorized {
			return "", &files.NotSignedInError{Err: err}
		}
		return "", fmt.Errorf("looking up your organization's default region: %w", err)
	}
	region := strings.TrimSpace(gjson.GetBytes(body, "defaultRegion").String())
	if region == "" {
		return "", fmt.Errorf("your organization has no default region: pass --region <region>")
	}
	return region, nil
}

func (r *filesRunner) fetchOrganization(ctx context.Context) ([]byte, error) {
	req, err := executor.BuildRequest(ctx, r.creds, r.runtime, r.getOrganization, nil, nil, "")
	if err != nil {
		return nil, err
	}
	return executor.DoWithReauth(r.client, req, r.creds)
}

func (r *filesRunner) completeRegions(cmd *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
	if r.getOrganization == nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	body, err := r.fetchOrganization(ctx)
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	var values []string
	gjson.GetBytes(body, "allowedRegions").ForEach(func(_, item gjson.Result) bool {
		if v := strings.TrimSpace(item.String()); v != "" {
			values = append(values, v)
		}
		return true
	})
	return values, cobra.ShellCompDirectiveNoFileComp
}

// filesArgs checks the positional count and explains the expected form.
func filesArgs(minArgs, maxArgs int, example string) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if len(args) >= minArgs && len(args) <= maxArgs {
			return nil
		}
		message := fmt.Sprintf("expected %d argument(s), received %d", minArgs, len(args))
		if minArgs != maxArgs {
			message = fmt.Sprintf("expected %d to %d arguments, received %d", minArgs, maxArgs, len(args))
		}
		return fmt.Errorf("%s\n\nUsage: %s\nExample: %s %s\nRun '%s --help' for options.",
			message, cmd.UseLine(), cmd.CommandPath(), example, cmd.CommandPath())
	}
}

func (r *filesRunner) newLsCommand(open openDrive) *cobra.Command {
	var output string
	cmd := &cobra.Command{
		Use:           "ls [remote-folder]",
		Short:         "List a folder",
		Args:          filesArgs(0, 1, "reports/2026"),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if output != outputText && output != outputJSON {
				return fmt.Errorf("invalid --output %q (expected text|json)", output)
			}
			remote := ""
			if len(args) == 1 {
				remote = args[0]
			}
			drive, err := open(cmd)
			if err != nil {
				return err
			}
			entries, err := drive.List(cmd.Context(), remote)
			if err != nil || entries == nil {
				return err
			}
			if output == outputJSON {
				encoded, err := json.MarshalIndent(entries, "", "  ")
				if err != nil {
					return err
				}
				_, err = fmt.Fprintln(cmd.OutOrStdout(), string(encoded))
				return err
			}
			return writeEntryTable(cmd.OutOrStdout(), entries)
		},
	}
	cmd.Flags().StringVar(&output, "output", outputText, "output format: text|json")
	return cmd
}

func writeEntryTable(out io.Writer, entries []files.Entry) error {
	if len(entries) == 0 {
		_, err := fmt.Fprintln(out, "No files or folders.")
		return err
	}
	tw := tabwriter.NewWriter(out, 2, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "TYPE\tSIZE\tMODIFIED\tNAME"); err != nil {
		return err
	}
	for _, entry := range entries {
		kind, size, name := "file", formatBytes(float64(entry.Size)), entry.Name
		if entry.IsFolder() {
			kind, size = "folder", "-"
			if !strings.HasSuffix(name, "/") {
				name += "/"
			}
		}
		modified := entry.LastModified
		if modified == "" {
			modified = "-"
		}
		if _, err := fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", kind, size, modified, name); err != nil {
			return err
		}
	}
	return tw.Flush()
}

func (r *filesRunner) newMkdirCommand(open openDrive) *cobra.Command {
	return &cobra.Command{
		Use:           "mkdir <remote-folder>",
		Short:         "Create a folder, and any missing parent folders",
		Args:          filesArgs(1, 1, "reports/2026/q3"),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			drive, err := open(cmd)
			if err != nil {
				return err
			}
			if err := drive.Mkdir(cmd.Context(), args[0]); err != nil {
				return err
			}
			return r.report(cmd, "Created folder %s\n", args[0])
		},
	}
}

func (r *filesRunner) newUploadCommand(open openDrive) *cobra.Command {
	return &cobra.Command{
		Use:           "upload <local-file> <remote-path>",
		Short:         "Upload one local file",
		Long:          "Upload one local file (up to 500 MB). When <remote-path> ends in \"/\", the local file's name is added to it.",
		Args:          filesArgs(2, 2, "./q3.csv reports/q3.csv"),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			drive, err := open(cmd)
			if err != nil {
				return err
			}
			local, remote := args[0], args[1]
			ctx, stop := interruptContext(cmd.Context())
			defer stop()
			target, err := drive.Upload(ctx, remote, local)
			if err != nil {
				return err
			}
			return r.report(cmd, "Uploaded %s to %s\n", local, target)
		},
	}
}

func (r *filesRunner) newDownloadCommand(open openDrive) *cobra.Command {
	return &cobra.Command{
		Use:           "download <remote-file> [local-path]",
		Short:         "Download one file",
		Long:          "Download one file. Without [local-path] it is saved under its own name in the current folder; when [local-path] is a folder, it is saved inside it.",
		Args:          filesArgs(1, 2, "reports/q3.csv ./q3.csv"),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			name, err := files.BaseName(args[0])
			if err != nil {
				return err
			}
			dest := name
			if len(args) == 2 {
				dest = args[1]
				if info, statErr := os.Stat(dest); statErr == nil && info.IsDir() {
					dest = filepath.Join(dest, name)
				}
			}
			drive, err := open(cmd)
			if err != nil {
				return err
			}
			if r.dryRun() {
				return drive.Download(cmd.Context(), args[0], io.Discard)
			}
			ctx, stop := interruptContext(cmd.Context())
			defer stop()
			if err := writeFileAtomically(dest, func(w io.Writer) error {
				return drive.Download(ctx, args[0], w)
			}); err != nil {
				return err
			}
			return r.report(cmd, "Downloaded %s to %s\n", args[0], dest)
		},
	}
}

func (r *filesRunner) newCatCommand(open openDrive) *cobra.Command {
	return &cobra.Command{
		Use:           "cat <remote-file>",
		Short:         "Print one file to stdout",
		Args:          filesArgs(1, 1, "reports/q3.csv"),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			drive, err := open(cmd)
			if err != nil {
				return err
			}
			if r.dryRun() {
				return drive.Download(cmd.Context(), args[0], io.Discard)
			}
			// Buffer in a temp file so a failed transfer prints nothing partial.
			tmp, err := os.CreateTemp("", "wherobots-cat-*")
			if err != nil {
				return fmt.Errorf("create temporary file: %w", err)
			}
			defer func() {
				_ = tmp.Close()
				_ = os.Remove(tmp.Name())
			}()
			ctx, stop := interruptContext(cmd.Context())
			defer stop()
			if err := drive.Download(ctx, args[0], tmp); err != nil {
				return err
			}
			if _, err := tmp.Seek(0, io.SeekStart); err != nil {
				return err
			}
			_, err = io.Copy(cmd.OutOrStdout(), tmp)
			return err
		},
	}
}

func (r *filesRunner) newMvCommand(open openDrive) *cobra.Command {
	return &cobra.Command{
		Use:           "mv <remote-path> <new-name>",
		Short:         "Rename a file within its folder",
		Long:          "Rename a file within its folder. <new-name> is a bare name or a path in the same folder; moving to another folder is not supported.",
		Args:          filesArgs(2, 2, "reports/q3.csv q3-final.csv"),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			newName, err := files.RenameTarget(args[0], args[1])
			if err != nil {
				return err
			}
			drive, err := open(cmd)
			if err != nil {
				return err
			}
			if err := drive.Rename(cmd.Context(), args[0], newName); err != nil {
				return err
			}
			return r.report(cmd, "Renamed %s to %s\n", args[0], newName)
		},
	}
}

func (r *filesRunner) newRmCommand(open openDrive) *cobra.Command {
	return &cobra.Command{
		Use:           "rm <remote-file>",
		Short:         "Delete one file",
		Args:          filesArgs(1, 1, "reports/q3.csv"),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			drive, err := open(cmd)
			if err != nil {
				return err
			}
			if err := drive.DeleteFile(cmd.Context(), args[0]); err != nil {
				return err
			}
			return r.report(cmd, "Deleted %s\n", args[0])
		},
	}
}

func (r *filesRunner) newRmdirCommand(open openDrive) *cobra.Command {
	var recursive bool
	cmd := &cobra.Command{
		Use:           "rmdir <remote-folder>",
		Short:         "Delete a folder",
		Long:          "Delete a folder. A folder that is not empty is refused unless --recursive is given, which deletes the folder and everything in it.",
		Args:          filesArgs(1, 1, "reports/2026/q3"),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			drive, err := open(cmd)
			if err != nil {
				return err
			}
			if err := drive.DeleteDir(cmd.Context(), args[0], recursive); err != nil {
				return err
			}
			return r.report(cmd, "Deleted folder %s\n", args[0])
		},
	}
	cmd.Flags().BoolVarP(&recursive, "recursive", "r", false, "delete the folder even if it is not empty, with everything in it")
	return cmd
}

func (r *filesRunner) dryRun() bool {
	return r.flags != nil && r.flags.DryRun
}

// report prints a confirmation, except in dry-run mode where nothing happened.
func (r *filesRunner) report(cmd *cobra.Command, format string, args ...any) error {
	if r.dryRun() {
		return nil
	}
	_, err := fmt.Fprintf(cmd.OutOrStdout(), format, args...)
	return err
}

// interruptContext is cancelled by the first Ctrl-C or SIGTERM so a transfer
// unwinds through its cleanup; a second signal kills the process as usual.
func interruptContext(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, stop := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-ctx.Done()
		stop()
	}()
	return ctx, stop
}

// writeFileAtomically fills a temporary file beside dest and renames it into
// place only on success, so a failed transfer leaves no half-written file.
// A replaced file keeps its permissions; a new one gets 0666 less the umask.
func writeFileAtomically(dest string, fill func(io.Writer) error) (err error) {
	tmp, err := createPartialFile(dest)
	if err != nil {
		return fmt.Errorf("create temporary file: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()
	if err = fill(tmp); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("write %s: %w", dest, err)
	}
	if info, statErr := os.Stat(dest); statErr == nil && info.Mode().IsRegular() {
		if err = os.Chmod(tmp.Name(), info.Mode().Perm()); err != nil {
			return fmt.Errorf("set permissions on %s: %w", dest, err)
		}
	}
	if err = os.Rename(tmp.Name(), dest); err != nil {
		return fmt.Errorf("move download into place at %s: %w", dest, err)
	}
	return nil
}

// createPartialFile opens a new, uniquely named file beside dest. It asks for
// 0666 so the process umask decides the mode, as for any file the user creates.
func createPartialFile(dest string) (*os.File, error) {
	prefix := filepath.Join(filepath.Dir(dest), "."+filepath.Base(dest)+".partial-")
	for range 100 {
		name := prefix + strconv.FormatUint(uint64(rand.Uint32()), 10)
		f, err := os.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o666)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		return f, err
	}
	return nil, fmt.Errorf("could not pick an unused temporary name beside %s", dest)
}
