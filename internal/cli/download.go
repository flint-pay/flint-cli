package cli

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
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

func saveDownload(value any, destination string) (any, *CLIError) {
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
		return nil, usageError("INVALID_DESTINATION", "--save-to requires a file path.", "save_to")
	}
	file, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, configError("FILE_WRITE_FAILED", "Could not create the download file. Choose a new, writable path.", err)
	}
	_, writeErr := file.Write(raw)
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		_ = os.Remove(destination)
		return nil, configError("FILE_WRITE_FAILED", "Could not write the download file.", writeErr)
	}
	delete(data, "content_base64")
	data["path"] = destination
	return envelope, nil
}
