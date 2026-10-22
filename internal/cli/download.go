package cli

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

type responseContractKey struct{}

func responseContract(ctx context.Context) *Command {
	command, _ := ctx.Value(responseContractKey{}).(*Command)
	return command
}

func fileResponse(raw []byte, mediaType string) map[string]any {
	return map[string]any{"data": map[string]any{"content_type": mediaType, "size": len(raw), "content_base64": base64.StdEncoding.EncodeToString(raw)}}
}

func (a *App) followReportDownload(ctx context.Context, location string) (*apiResponse, *CLIError) {
	target, err := url.Parse(location)
	if err != nil || target.Scheme != "https" || target.Hostname() == "" || target.User != nil {
		return nil, invalidResponseError("INVALID_DOWNLOAD_URL", "The report download must use an absolute HTTPS URL.", err)
	}
	// Use a separate request and reject subsequent redirects. API credentials
	// and request headers must never be forwarded to file storage.
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, invalidResponseError("INVALID_DOWNLOAD_URL", "The report download URL is invalid.", err)
	}
	client := &http.Client{CheckRedirect: rejectAPIRedirect}
	if a.HTTPClient != nil {
		copy := *a.HTTPClient
		client = &copy
		client.CheckRedirect = rejectAPIRedirect
		client.Jar = nil
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, networkError("DOWNLOAD_FAILED", "The report file could not be downloaded.", nil)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, invalidResponseError("DOWNLOAD_FAILED", "The report file server did not return HTTP 200.", nil)
	}
	mediaType := strings.TrimSpace(strings.Split(response.Header.Get("Content-Type"), ";")[0])
	if mediaType != "text/csv" && mediaType != "application/csv" && mediaType != "application/octet-stream" {
		return nil, invalidResponseError("INVALID_CONTENT_TYPE", "The report server did not return a CSV file.", nil)
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, (16<<20)+1))
	if err != nil {
		return nil, networkError("DOWNLOAD_FAILED", "The report file could not be read.", nil)
	}
	if len(raw) > 16<<20 {
		return nil, invalidResponseError("RESPONSE_TOO_LARGE", "The report file exceeded 16 MiB.", nil)
	}
	return &apiResponse{Status: response.StatusCode, Raw: raw, Headers: response.Header.Clone(), Value: fileResponse(raw, "text/csv")}, nil
}

func (a *App) saveDownload(value any, destination string) (any, *CLIError) {
	envelope, _ := value.(map[string]any)
	data, _ := envelope["data"].(map[string]any)
	encoded, ok := data["content_base64"].(string)
	if !ok {
		return nil, invalidResponseError("INVALID_DOWNLOAD_RESPONSE", "The response did not contain a file.", nil)
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, invalidResponseError("INVALID_DOWNLOAD_RESPONSE", "The file response could not be decoded.", err)
	}
	if strings.TrimSpace(destination) == "" {
		return nil, usageError("INVALID_DESTINATION", "--save-to requires a file path, or - for stdout.", "save_to")
	}
	if destination == "-" {
		// An MCP tool result is JSON, so it cannot carry raw file bytes.
		if mcpSessionFrom(a.commandContext()) != nil {
			return nil, usageError("INVALID_DESTINATION", `save_to "-" writes raw file bytes to stdout, which an MCP tool result cannot carry. Omit save_to to receive content_base64, or pass a file path.`, "save_to")
		}
		if _, err := a.Stdout.Write(raw); err != nil {
			return nil, networkError("OUTPUT_WRITE_FAILED", "Could not write the downloaded file to stdout.", err)
		}
		return outputHandled{}, nil
	}
	file, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		mediaType, _ := data["content_type"].(string)
		return nil, downloadDestinationError(destination, mediaType, err)
	}
	_, writeErr := file.Write(raw)
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		_ = os.Remove(destination)
		cause := errors.Join(writeErr, closeErr)
		return nil, configError("FILE_WRITE_FAILED", "Could not write the download file: "+terminalSafe(cause.Error())+".", cause)
	}
	delete(data, "content_base64")
	data["path"] = destination
	return envelope, nil
}

// downloadDestinationError names why --save-to could not create its file.
// Downloads never overwrite, so an existing path is a usage error too.
func downloadDestinationError(destination, mediaType string, err error) *CLIError {
	path := terminalSafe(destination)
	switch {
	case errors.Is(err, fs.ErrExist):
		if info, statErr := os.Stat(destination); statErr == nil && info.IsDir() {
			name := map[string]string{"application/pdf": "download.pdf", "text/csv": "download.csv"}[mediaType]
			if name == "" {
				name = "download"
			}
			return usageError("DESTINATION_IS_DIRECTORY", fmt.Sprintf("%s is a directory. Pass a file path inside it, such as %s.", path, filepath.Join(path, name)), "save_to")
		}
		return usageError("DESTINATION_EXISTS", fmt.Sprintf("%s already exists, and downloads never overwrite files. Choose a new path or remove the existing file.", path), "save_to")
	case errors.Is(err, fs.ErrNotExist):
		return usageError("DESTINATION_DIRECTORY_NOT_FOUND", fmt.Sprintf("The directory %s does not exist. Create it or choose another path.", terminalSafe(filepath.Dir(destination))), "save_to")
	case errors.Is(err, syscall.ENOTDIR):
		return usageError("DESTINATION_DIRECTORY_NOT_FOUND", fmt.Sprintf("Part of the directory path %s is a file, not a directory. Choose another path.", terminalSafe(filepath.Dir(destination))), "save_to")
	case errors.Is(err, fs.ErrPermission):
		return usageError("DESTINATION_NOT_WRITABLE", fmt.Sprintf("Permission denied creating %s. Choose a directory you can write to.", path), "save_to")
	}
	reason := err
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) {
		reason = pathErr.Err
	}
	return usageError("INVALID_DESTINATION", fmt.Sprintf("Could not create %s: %s.", path, terminalSafe(reason.Error())), "save_to")
}
