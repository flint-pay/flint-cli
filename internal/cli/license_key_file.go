package cli

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

const licenseKeyBatchSize = 1000

type licenseKeyFileCounts struct {
	Added    int `json:"added"`
	Existing int `json:"existing"`
	Rejected int `json:"rejected"`
}

func (a *App) executeLicenseKeyFile(ctx context.Context, cmd *Command, opts Options, resolved ResolvedConfig, auth AuthContext, credential, baseURL string) (any, *CLIError) {
	if opts.Input != "" || len(opts.Raw["keys"]) > 0 {
		return nil, usageError("CONFLICTING_ARGUMENTS", "--file cannot be combined with --keys or --input.", "file")
	}
	attestation := lastRawOption(opts, "seller-authorization-confirmed")
	if attestation != "" && attestation != "true" {
		return nil, usageError("INVALID_ARGUMENT", "--seller-authorization-confirmed must be true to upload keys.", "seller_authorization_confirmed")
	}
	keys, occurrences, rejected, e := readLicenseKeyFile(ctx, lastRawOption(opts, "file"))
	if e != nil {
		return nil, e
	}
	counts := licenseKeyFileCounts{Rejected: rejected}
	if len(keys) == 0 && !opts.DryRunClient() {
		return map[string]any{"data": counts}, nil
	}
	if attestation == "" && !opts.DryRunClient() {
		message := fmt.Sprintf("Confirm that the rights holder authorizes you to sell these %d distinct keys.", len(keys))
		if e := a.promptConfirmation(opts, message, "SELLER_AUTHORIZATION_REQUIRED", "--seller-authorization-confirmed"); e != nil {
			return nil, e
		}
	}
	// Resolve the pool once, including history references, so all batches use
	// the same target even if local history changes during the import.
	target := *cmd
	target.OperationID, target.InputSchema = "", ""
	req, e := a.prepareRequest(ctx, &target, opts, resolved, credential, baseURL)
	if e != nil {
		return nil, e
	}
	if opts.DryRunClient() {
		return map[string]any{"data": map[string]any{
			"method": req.Method, "path": req.Path, "environment": auth.Environment,
			"keys": len(keys), "rejected": rejected, "batches": (len(keys) + licenseKeyBatchSize - 1) / licenseKeyBatchSize,
			"persistent_side_effects": false,
		}}, nil
	}
	// The import seed identifies this run across restarts. Each batch must keep
	// its own key on transport retries, and replaying with the same seed and
	// unchanged file must recover the original per-key outcomes.
	seed := opts.IdempotencyKey
	batchNumber := 0
	for batch := range slices.Chunk(keys, licenseKeyBatchSize) {
		batchNumber++
		body, _ := json.Marshal(map[string]any{"keys": batch, "seller_authorization_confirmed": true})
		batchKey := fmt.Sprintf("flint-license-upload-%x", sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d", seed, batchNumber))))
		resp, e := a.doRequestWithin(ctx, opts.Timeout, baseURL, credential, req.Method, req.Path, body, batchKey, opts.Debug)
		if e == nil {
			e = countLicenseKeyBatch(resp.Value, batch, occurrences, &counts)
		}
		if e != nil {
			return nil, licenseKeyUploadError(e, seed, batchKey, batchNumber, counts)
		}
	}
	return map[string]any{"data": counts}, nil
}

func licenseKeyUploadError(e *CLIError, seed, batchKey string, batch int, counts licenseKeyFileCounts) *CLIError {
	details, _ := cloneErrorJSON(e.Details).(map[string]any)
	if details == nil {
		details = map[string]any{}
	}
	nested, _ := details["details"].(map[string]any)
	// Preserve request diagnostics and server remediation. Remove the CLI's
	// single-request retry commands because --file takes the import seed.
	e.idempotencyKey = ""
	for _, target := range []map[string]any{details, nested} {
		remediation, _ := target["remediation"].(map[string]any)
		actions, _ := remediation["next_actions"].([]any)
		if len(actions) == 0 {
			continue
		}
		actions = slices.DeleteFunc(actions, func(action any) bool {
			actionMap, _ := action.(map[string]any)
			message, _ := actionMap["reason_message"].(string)
			return strings.Contains(message, "--idempotency-key "+batchKey) || message == "Idempotency key: "+batchKey
		})
		remediation["next_actions"] = actions
	}
	details["upload"] = map[string]any{"counts": counts, "batch": batch, "idempotency_key": seed}
	e.Details = details
	e.Message += fmt.Sprintf(" Before batch %d, %d keys were added, %d already existed, and %d were rejected. When retrying the upload within 24 hours, use the unchanged file and --idempotency-key %s.", batch, counts.Added, counts.Existing, counts.Rejected, terminalSafe(quoteRemediationArgument(seed)))
	if _, apiEnvelope := details["message"]; apiEnvelope {
		details["message"] = e.Message
	}
	return e
}

func readLicenseKeyFile(ctx context.Context, path string) ([]string, map[string]int, int, *CLIError) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, 0, usageError("LICENSE_KEY_FILE_READ_FAILED", "Could not open the license key file.", "file")
	}
	defer f.Close()
	input := bufio.NewReader(&contextInputReader{ctx: ctx, reader: f})
	firstLine, err := input.ReadString('\n')
	firstLine = strings.TrimPrefix(firstLine, "\ufeff")
	for err == nil && strings.TrimSpace(firstLine) == "" {
		firstLine, err = input.ReadString('\n')
	}
	if err != nil && err != io.EOF {
		return nil, nil, 0, licenseKeyFileReadError(err)
	}
	header, headerErr := csv.NewReader(strings.NewReader(firstLine)).Read()
	column := slices.IndexFunc(header, func(name string) bool {
		name = strings.ToLower(strings.TrimSpace(name))
		return name == "license_key" || name == "key"
	})
	var keys []string
	occurrences := map[string]int{}
	rejected := 0
	add := func(value string) {
		// The API rejects the entire batch if any value fails this wire format.
		if len(value) < 8 || len(value) > 255 || strings.ContainsFunc(value, func(r rune) bool { return r < ' ' || r > '~' }) {
			rejected++
			return
		}
		if occurrences[value] == 0 {
			keys = append(keys, value)
		}
		occurrences[value]++
	}
	if strings.EqualFold(filepath.Ext(path), ".txt") {
		column = -1
	}
	if column < 0 && strings.EqualFold(filepath.Ext(path), ".csv") {
		return nil, nil, 0, usageError("INVALID_LICENSE_KEY_CSV", "A CSV needs a license_key or key column. Use a .txt file for a list without a header.", "file")
	}
	if column >= 0 {
		if headerErr != nil {
			return nil, nil, 0, usageError("INVALID_LICENSE_KEY_CSV", "The license key CSV header is malformed.", "file")
		}
		reader := csv.NewReader(io.MultiReader(strings.NewReader(firstLine), input))
		_, _ = reader.Read() // The first record is the header we just read.
		for {
			record, err := reader.Read()
			if err == io.EOF {
				break
			}
			if err != nil {
				var parseErr *csv.ParseError
				if errors.As(err, &parseErr) {
					return nil, nil, 0, usageError("INVALID_LICENSE_KEY_CSV", "The license key CSV contains a malformed row or an inconsistent column count.", "file")
				}
				return nil, nil, 0, licenseKeyFileReadError(err)
			}
			add(strings.TrimSpace(record[column]))
		}
	} else {
		for {
			if value := strings.TrimSpace(firstLine); value != "" {
				add(value)
			}
			if err == io.EOF {
				break
			}
			firstLine, err = input.ReadString('\n')
			if err != nil && err != io.EOF {
				return nil, nil, 0, licenseKeyFileReadError(err)
			}
		}
	}
	return keys, occurrences, rejected, nil
}

func licenseKeyFileReadError(err error) *CLIError {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return networkError("CLIENT_CANCELED", "License key file reading was canceled.", err)
	}
	return usageError("LICENSE_KEY_FILE_READ_FAILED", "Could not read the license key file.", "file")
}

func countLicenseKeyBatch(response any, batch []string, occurrences map[string]int, counts *licenseKeyFileCounts) *CLIError {
	value, _ := lookupPath(response, "data.additions")
	additions, _ := value.([]any)
	invalid := func() *CLIError {
		return invalidResponseError("INVALID_LICENSE_KEY_UPLOAD_RESPONSE", "Flint did not return a recognized outcome for each submitted key.", nil)
	}
	if len(additions) != len(batch) {
		return invalid()
	}
	var delta licenseKeyFileCounts
	for i, addition := range additions {
		outcome, _ := lookupPath(addition, "outcome")
		switch outcome {
		case "created":
			delta.Added++
			delta.Existing += occurrences[batch[i]] - 1
		case "existing":
			delta.Existing += occurrences[batch[i]]
		default:
			return invalid()
		}
	}
	counts.Added += delta.Added
	counts.Existing += delta.Existing
	return nil
}
