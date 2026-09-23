package files

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/tidwall/gjson"

	"wherobots/cli/internal/executor"
	"wherobots/cli/internal/spec"
)

// listPageLimit is sent on every listing: the server treats a missing or zero
// limit as "fetch everything" and ignores the cursor, and refuses > 1000.
const listPageLimit = 1000

// maxListPages stops a listing whose server keeps handing back cursors.
const maxListPages = 10000

// Operations are the seven /storage file-plane routes, found in the runtime
// spec by the caller.
type Operations struct {
	ListDirectory   *spec.Operation // GET    /storage/{storage_id}/directories/{path}
	CreateDirectory *spec.Operation // PUT    /storage/{storage_id}/directories/{path}
	DeleteDirectory *spec.Operation // DELETE /storage/{storage_id}/directories/{path}
	CreateUploadURL *spec.Operation // POST   /storage/{storage_id}/file-upload-url/{path}
	DownloadFile    *spec.Operation // GET    /storage/{storage_id}/files/{path}
	DeleteFile      *spec.Operation // DELETE /storage/{storage_id}/files/{path}
	RenameFile      *spec.Operation // POST   /storage/{storage_id}/file-rename/{path}
}

// Complete reports whether every route was found.
func (o Operations) Complete() bool {
	return o.ListDirectory != nil && o.CreateDirectory != nil && o.DeleteDirectory != nil &&
		o.CreateUploadURL != nil && o.DownloadFile != nil && o.DeleteFile != nil && o.RenameFile != nil
}

// Service carries what every drive needs to reach the API.
type Service struct {
	Runtime *spec.RuntimeSpec
	Creds   executor.Credentials
	// API sends control-plane requests and keeps the normal request timeout.
	API *http.Client
	// Transfer moves file bytes to and from storage; it has no overall timeout.
	Transfer *http.Client
	Ops      Operations
	// DryRun, when non-nil, receives each API request as curl instead of it being sent.
	DryRun io.Writer
}

// DriveClient runs file operations against one drive.
type DriveClient struct {
	svc       *Service
	drive     Drive
	storageID string
}

// Open binds the service to a drive.
func (s *Service) Open(d Drive) (*DriveClient, error) {
	id, err := d.StorageID()
	if err != nil {
		return nil, err
	}
	return &DriveClient{svc: s, drive: d, storageID: id}, nil
}

// Entry is one file or folder in a listing, using the API's field names.
type Entry struct {
	Name         string `json:"name"`
	Path         string `json:"path"`
	Type         string `json:"type"`
	Size         int64  `json:"size"`
	LastModified string `json:"lastModified,omitempty"`
}

// IsFolder reports whether the entry is a folder.
func (e Entry) IsFolder() bool {
	return strings.EqualFold(e.Type, "FOLDER")
}

// NotSignedInError is a 401 from the API.
type NotSignedInError struct {
	Err error
}

func (e *NotSignedInError) Error() string {
	return "not signed in (the API answered 401) — run `wherobots auth login`, or set WHEROBOTS_API_KEY to a valid API key"
}

func (e *NotSignedInError) Unwrap() error { return e.Err }

// NotEnabledError means the drive root itself is missing for this region.
type NotEnabledError struct {
	Drive  string
	Region string
}

func (e *NotEnabledError) Error() string {
	return fmt.Sprintf("Files is not enabled for %s in region %s", e.Drive, e.Region)
}

// NotFoundError means the drive exists but the path does not.
type NotFoundError struct {
	Path string
}

func (e *NotFoundError) Error() string {
	return "no such file or folder: " + e.Path
}

// ---- path checks ----

// parsedPath is a validated remote path relative to the drive root.
type parsedPath struct {
	segments []string
	folder   bool // written with a trailing "/"
}

// parseRemotePath drops one leading "/", notes a trailing "/", and refuses
// empty levels, "." and "..", before any request is sent.
func parseRemotePath(raw string) (parsedPath, error) {
	trimmed := strings.TrimPrefix(raw, "/")
	folder := strings.HasSuffix(trimmed, "/")
	trimmed = strings.TrimSuffix(trimmed, "/")
	if trimmed == "" {
		return parsedPath{folder: true}, nil
	}
	segments := strings.Split(trimmed, "/")
	for _, segment := range segments {
		switch segment {
		case "":
			return parsedPath{}, fmt.Errorf("invalid remote path %q: it contains an empty level", raw)
		case ".", "..":
			return parsedPath{}, fmt.Errorf("invalid remote path %q: '.' and '..' are not allowed", raw)
		}
	}
	return parsedPath{segments: segments, folder: folder}, nil
}

func (p parsedPath) isRoot() bool { return len(p.segments) == 0 }

func (p parsedPath) filePath() string { return strings.Join(p.segments, "/") }

// folderPath is the route spelling of a folder: a trailing "/", or "" for the root.
func (p parsedPath) folderPath() string {
	if p.isRoot() {
		return ""
	}
	return strings.Join(p.segments, "/") + "/"
}

func (p parsedPath) base() string {
	if p.isRoot() {
		return ""
	}
	return p.segments[len(p.segments)-1]
}

func (p parsedPath) parent() string {
	if len(p.segments) <= 1 {
		return ""
	}
	return strings.Join(p.segments[:len(p.segments)-1], "/")
}

func (p parsedPath) requireFile(raw string) error {
	if p.isRoot() {
		return fmt.Errorf("a file path is required, got %q", raw)
	}
	if p.folder {
		return fmt.Errorf("%q ends in \"/\", which names a folder, not a file", raw)
	}
	return nil
}

// BaseName returns the last level of a remote file path.
func BaseName(remote string) (string, error) {
	p, err := parseRemotePath(remote)
	if err != nil {
		return "", err
	}
	if err := p.requireFile(remote); err != nil {
		return "", err
	}
	return p.base(), nil
}

func validateNewName(name string) error {
	if name == "" || name == "." || name == ".." || strings.Contains(name, "/") {
		return fmt.Errorf("invalid new name %q: it must be a single name without \"/\"", name)
	}
	return nil
}

// RenameTarget turns the target of `mv` into the new name. It accepts a bare
// name or a path in the source's folder; any other folder is refused.
func RenameTarget(src, dst string) (string, error) {
	source, err := parseRemotePath(src)
	if err != nil {
		return "", err
	}
	if source.isRoot() {
		return "", fmt.Errorf("cannot rename the drive root")
	}
	if err := source.requireFile(src); err != nil {
		return "", err
	}
	if !strings.Contains(dst, "/") {
		if err := validateNewName(dst); err != nil {
			return "", err
		}
		return dst, nil
	}
	target, err := parseRemotePath(dst)
	if err != nil {
		return "", err
	}
	if target.isRoot() || target.folder {
		return "", fmt.Errorf("invalid target %q: give the new name of the file", dst)
	}
	if target.parent() != source.parent() {
		return "", fmt.Errorf("moves between folders are not supported: mv can only rename %q within its own folder", src)
	}
	return target.base(), nil
}

// ---- requests ----

func (c *DriveClient) dryRun() bool { return c.svc.DryRun != nil }

func (c *DriveClient) build(ctx context.Context, op *spec.Operation, remotePath string, query []executor.QueryPair) (*http.Request, error) {
	args := make([]string, 0, len(op.PathParamOrder))
	for _, name := range op.PathParamOrder {
		switch name {
		case "storage_id":
			args = append(args, c.storageID)
		case "path":
			args = append(args, remotePath)
		default:
			return nil, fmt.Errorf("unexpected path parameter %q in %s %s", name, op.Method, op.Path)
		}
	}
	return executor.BuildRequestMultiSegment(ctx, c.svc.Creds, c.svc.Runtime, op, []string{"path"}, args, query, "")
}

// send runs one API request, or prints it as curl in dry-run mode and
// returns (nil, nil). Errors are returned unmapped.
func (c *DriveClient) send(ctx context.Context, op *spec.Operation, remotePath string, query []executor.QueryPair) ([]byte, error) {
	req, err := c.build(ctx, op, remotePath, query)
	if err != nil {
		return nil, err
	}
	if c.dryRun() {
		_, err := fmt.Fprintln(c.svc.DryRun, executor.RenderCurl(req, ""))
		return nil, err
	}
	return executor.DoWithReauth(c.svc.API, req, c.svc.Creds)
}

// mapError turns a 401 into "not signed in" and a 404 into either "Files is
// not enabled" (the drive root is missing too) or "no such file or folder".
func (c *DriveClient) mapError(ctx context.Context, err error, displayPath string) error {
	var httpErr *executor.HTTPError
	if !errors.As(err, &httpErr) {
		return err
	}
	switch httpErr.StatusCode {
	case http.StatusUnauthorized:
		return &NotSignedInError{Err: err}
	case http.StatusNotFound:
		// An empty displayPath means the failing call was on the root itself.
		if displayPath == "" || c.rootMissing(ctx) {
			return &NotEnabledError{Drive: c.drive.Label(), Region: c.drive.Region}
		}
		return &NotFoundError{Path: displayPath}
	}
	return err
}

// rootMissing lists the drive root once; only a 404 counts as missing.
func (c *DriveClient) rootMissing(ctx context.Context) bool {
	req, err := c.build(ctx, c.svc.Ops.ListDirectory, "", []executor.QueryPair{{Key: "limit", Value: "1"}})
	if err != nil {
		return false
	}
	_, err = executor.DoWithReauth(c.svc.API, req, c.svc.Creds)
	var httpErr *executor.HTTPError
	return errors.As(err, &httpErr) && httpErr.StatusCode == http.StatusNotFound
}

func parseEntries(body []byte) []Entry {
	var entries []Entry
	gjson.GetBytes(body, "items").ForEach(func(_, item gjson.Result) bool {
		entries = append(entries, Entry{
			Name:         item.Get("name").String(),
			Path:         item.Get("path").String(),
			Type:         item.Get("type").String(),
			Size:         item.Get("size").Int(),
			LastModified: item.Get("lastModified").String(),
		})
		return true
	})
	return entries
}

// ---- operations ----

// List returns every entry in a folder, following next_page to the end.
// In dry-run mode it prints the first page request and returns nil.
func (c *DriveClient) List(ctx context.Context, remote string) ([]Entry, error) {
	p, err := parseRemotePath(remote)
	if err != nil {
		return nil, err
	}
	folder := p.folderPath()

	entries := []Entry{}
	cursor := ""
	for page := 0; page < maxListPages; page++ {
		query := []executor.QueryPair{{Key: "limit", Value: fmt.Sprintf("%d", listPageLimit)}}
		if cursor != "" {
			query = append(query, executor.QueryPair{Key: "cursor", Value: cursor})
		}
		body, err := c.send(ctx, c.svc.Ops.ListDirectory, folder, query)
		if err != nil {
			return nil, c.mapError(ctx, err, folder)
		}
		if c.dryRun() {
			return nil, nil
		}
		entries = append(entries, parseEntries(body)...)

		next := strings.TrimSpace(gjson.GetBytes(body, "next_page").String())
		if next == "" {
			return entries, nil
		}
		if next == cursor {
			return nil, fmt.Errorf("listing %q did not advance: the server returned the same page cursor twice", folder)
		}
		cursor = next
	}
	return nil, fmt.Errorf("listing %q stopped after %d pages", folder, maxListPages)
}

// Mkdir creates each level of a nested folder in order, one request per
// level, so no level is created implicitly. A folder that exists is kept;
// a file with that name is an error.
func (c *DriveClient) Mkdir(ctx context.Context, remote string) error {
	p, err := parseRemotePath(remote)
	if err != nil {
		return err
	}
	if p.isRoot() {
		return fmt.Errorf("a folder path is required")
	}
	for i := range p.segments {
		level := strings.Join(p.segments[:i+1], "/") + "/"
		_, err := c.send(ctx, c.svc.Ops.CreateDirectory, level, nil)
		var httpErr *executor.HTTPError
		if errors.As(err, &httpErr) && httpErr.StatusCode == http.StatusConflict {
			if err := c.requireNotFile(ctx, p.segments[:i+1]); err != nil {
				return err
			}
			continue // already a folder, like mkdir -p
		}
		if err != nil {
			return c.mapError(ctx, err, level)
		}
	}
	return nil
}

// requireNotFile runs after a 409 on creating the folder at segments. It
// lists the parent and fails when the name is listed as a file.
func (c *DriveClient) requireNotFile(ctx context.Context, segments []string) error {
	name := segments[len(segments)-1]
	entries, err := c.List(ctx, strings.Join(segments[:len(segments)-1], "/")+"/")
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if strings.Trim(entry.Name, "/") == name && !entry.IsFolder() {
			return fmt.Errorf("cannot create folder %s: a file with that name already exists", strings.Join(segments, "/")+"/")
		}
	}
	return nil
}

// Upload sends one local file to remote and returns the remote file path.
// A remote path ending in "/" (or the root) gets the local file's name.
func (c *DriveClient) Upload(ctx context.Context, remote, localPath string) (string, error) {
	p, err := parseRemotePath(remote)
	if err != nil {
		return "", err
	}
	if p.folder {
		p.segments = append(append([]string(nil), p.segments...), filepath.Base(localPath))
		p.folder = false
	}
	target := p.filePath()

	info, err := os.Stat(localPath)
	if err != nil {
		return "", fmt.Errorf("local file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("local path is not a regular file: %s", localPath)
	}

	body, err := c.send(ctx, c.svc.Ops.CreateUploadURL, target, nil)
	if err != nil {
		return "", c.mapError(ctx, err, target)
	}
	if c.dryRun() {
		return target, nil
	}
	uploadURL := strings.TrimSpace(gjson.GetBytes(body, "uploadUrl").String())
	if uploadURL == "" {
		return "", fmt.Errorf("upload URL response is missing uploadUrl")
	}
	if err := executor.UploadFileToPresignedURL(ctx, c.svc.Transfer, uploadURL, localPath); err != nil {
		return "", fmt.Errorf("uploading %s to %s: %w", localPath, target, err)
	}
	return target, nil
}

// Download writes one remote file to w. The API answers with a redirect to a
// presigned storage URL, which is fetched without any Wherobots credential.
func (c *DriveClient) Download(ctx context.Context, remote string, w io.Writer) error {
	p, err := parseRemotePath(remote)
	if err != nil {
		return err
	}
	if err := p.requireFile(remote); err != nil {
		return err
	}
	target := p.filePath()

	req, err := c.build(ctx, c.svc.Ops.DownloadFile, target, nil)
	if err != nil {
		return err
	}
	if c.dryRun() {
		_, err := fmt.Fprintln(c.svc.DryRun, executor.RenderCurl(req, ""))
		return err
	}
	location, err := executor.ResolveRedirectWithReauth(c.svc.API, req, c.svc.Creds)
	if err != nil {
		return c.mapError(ctx, err, target)
	}
	if _, err := executor.StreamFromURL(ctx, c.svc.Transfer, location, w); err != nil {
		return fmt.Errorf("downloading %s: %w", target, err)
	}
	return nil
}

// Rename gives a file a new name in its own folder.
func (c *DriveClient) Rename(ctx context.Context, remote, newName string) error {
	p, err := parseRemotePath(remote)
	if err != nil {
		return err
	}
	if p.isRoot() {
		return fmt.Errorf("cannot rename the drive root")
	}
	if err := p.requireFile(remote); err != nil {
		return err
	}
	if err := validateNewName(newName); err != nil {
		return err
	}
	target := p.filePath()
	_, err = c.send(ctx, c.svc.Ops.RenameFile, target, []executor.QueryPair{{Key: "new_name", Value: newName}})
	if err != nil {
		return c.mapError(ctx, err, target)
	}
	return nil
}

// DeleteFile removes exactly one file.
func (c *DriveClient) DeleteFile(ctx context.Context, remote string) error {
	p, err := parseRemotePath(remote)
	if err != nil {
		return err
	}
	if p.isRoot() {
		return fmt.Errorf("a file path is required")
	}
	if p.folder {
		return fmt.Errorf("%q ends in \"/\", which names a folder: use rmdir to delete folders", remote)
	}
	target := p.filePath()
	if _, err := c.send(ctx, c.svc.Ops.DeleteFile, target, nil); err != nil {
		return c.mapError(ctx, err, target)
	}
	return nil
}

// FolderNotEmptyError refuses a non-recursive delete of a folder with content.
type FolderNotEmptyError struct {
	Path string
}

func (e *FolderNotEmptyError) Error() string {
	return fmt.Sprintf("folder is not empty: %s (use --recursive to delete it and everything in it)", e.Path)
}

// DeleteDir removes a folder. Unless recursive is set, it first lists the
// folder and refuses when anything is in it, because the server deletes
// the folder together with its contents. The check is not atomic: content
// added between the listing and the delete is deleted too.
func (c *DriveClient) DeleteDir(ctx context.Context, remote string, recursive bool) error {
	p, err := parseRemotePath(remote)
	if err != nil {
		return err
	}
	if p.isRoot() {
		return fmt.Errorf("refusing to delete the root of %s", c.drive.Label())
	}
	folder := p.folderPath()

	if !recursive {
		body, err := c.send(ctx, c.svc.Ops.ListDirectory, folder, []executor.QueryPair{{Key: "limit", Value: "1"}})
		if err != nil {
			return c.mapError(ctx, err, folder)
		}
		if c.dryRun() {
			if _, err := fmt.Fprintln(c.svc.DryRun, "# the DELETE below is sent only if the listing above shows the folder is empty"); err != nil {
				return err
			}
		} else {
			// A further page means more entries than the marker, so it is not empty.
			if strings.TrimSpace(gjson.GetBytes(body, "next_page").String()) != "" {
				return &FolderNotEmptyError{Path: folder}
			}
			for _, entry := range parseEntries(body) {
				if strings.Trim(entry.Name, "/") != "" {
					return &FolderNotEmptyError{Path: folder}
				}
			}
		}
	}

	if _, err := c.send(ctx, c.svc.Ops.DeleteDirectory, folder, nil); err != nil {
		return c.mapError(ctx, err, folder)
	}
	return nil
}
