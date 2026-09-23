package executor

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
)

const maxAutoUploadSizeBytes = 500 * 1024 * 1024

func UploadFileToPresignedURL(ctx context.Context, client *http.Client, uploadURL, localPath string) error {
	if strings.TrimSpace(uploadURL) == "" {
		return fmt.Errorf("upload URL is required")
	}
	if strings.TrimSpace(localPath) == "" {
		return fmt.Errorf("local path is required")
	}
	if client == nil {
		return fmt.Errorf("http client is required")
	}

	parsed, err := url.Parse(uploadURL)
	if err != nil || !parsed.IsAbs() {
		return fmt.Errorf("invalid upload URL")
	}

	info, err := os.Stat(localPath)
	if err != nil {
		return fmt.Errorf("stat file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("file not found: %s", localPath)
	}
	if info.Size() > maxAutoUploadSizeBytes {
		sizeMB := float64(info.Size()) / (1024.0 * 1024.0)
		limitMB := float64(maxAutoUploadSizeBytes) / (1024.0 * 1024.0)
		return fmt.Errorf("file is %.1f MB, exceeding %.0f MB limit", sizeMB, limitMB)
	}

	content, err := os.ReadFile(localPath)
	if err != nil {
		return fmt.Errorf("read file: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, uploadURL, bytes.NewReader(content))
	if err != nil {
		return fmt.Errorf("build upload request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("upload request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return newStorageError("upload", resp)
	}

	return nil
}
