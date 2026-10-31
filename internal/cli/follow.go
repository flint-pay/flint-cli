package cli

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"time"
)

const followBooleanType = "boolean"
const followOverlap = 90 * time.Second

type followedLog struct {
	value map[string]any
	id    string
	at    time.Time
}

// A window is committed only after every page succeeds. Retrying a partial
// window therefore keeps both the cursor and the output unchanged.
func (a *App) followWindow(ctx context.Context, opts Options, req preparedRequest, key, baseURL string, bootstrap bool) ([]followedLog, time.Time, *CLIError) {
	path := setQuery(req.Path, "page_size", "100")
	var rows []followedLog
	var date time.Time
	tokens := map[string]bool{}
	for {
		resp, e := a.doRequestWithin(ctx, opts.Timeout, baseURL, key, req.Method, path, nil, "", opts.Debug)
		if e != nil {
			return nil, time.Time{}, e
		}
		if date.IsZero() {
			var err error
			date, err = http.ParseTime(resp.Headers.Get("Date"))
			if err != nil {
				return nil, date, invalidResponseError("INVALID_RESPONSE", "Request-log follow requires a valid server Date header.", err)
			}
		}
		body, _ := resp.Value.(map[string]any)
		data, ok := body["data"].([]any)
		if !ok {
			return nil, date, invalidResponseError("INVALID_PAGINATION_RESPONSE", "Request-log data is not an array.", nil)
		}
		for _, item := range data {
			row, _ := item.(map[string]any)
			id, _ := row["api_request_log_id"].(string)
			stamp, _ := row["created_at"].(string)
			at, err := time.Parse(time.RFC3339Nano, stamp)
			if id == "" || err != nil {
				return nil, date, invalidResponseError("INVALID_RESPONSE", "A request log is missing its ID or timestamp.", err)
			}
			rows = append(rows, followedLog{row, id, at})
		}
		// Without backfill the first request only establishes server time.
		if bootstrap {
			return rows, date, nil
		}
		token, e := nextPageToken(body)
		if e != nil {
			return nil, date, e
		}
		if token == "" {
			return rows, date, nil
		}
		if tokens[token] {
			return nil, date, invalidResponseError("PAGINATION_TOKEN_LOOP", "Flint returned a repeated pagination token.", nil)
		}
		tokens[token] = true
		path = setQuery(path, "page_token", token)
	}
}

func (a *App) executeFollow(ctx context.Context, opts Options, req preparedRequest, key, baseURL string) (any, *CLIError) {
	sleep := a.followSleep
	if sleep == nil {
		sleep = sleepContext
	}
	var deadline time.Time
	if opts.For > 0 {
		deadline = a.Now().Add(opts.For)
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.For)
		defer cancel()
	}
	u, _ := url.Parse(req.Path)
	floor, _ := time.Parse(time.RFC3339Nano, u.Query().Get("created_after"))
	bootstrap := floor.IsZero()
	var cursor time.Time
	seen := map[string]time.Time{}
	logs := []any{}
	count, failures := 0, 0
	noticed := false
	finish := func() (any, *CLIError) {
		stamp := cursor.UTC().Format(time.RFC3339Nano)
		var err error
		switch opts.Output {
		case "json":
			return map[string]any{"data": logs, "cursor": stamp}, nil
		case "ndjson":
			err = writeJSON(a.Stdout, map[string]any{"type": "end", "seen": count, "cursor": stamp})
		default:
			if !opts.Quiet {
				_, err = fmt.Fprintf(a.Stderr, "Stopped after %d requests.\n", count)
			}
		}
		if err != nil {
			return nil, networkError("OUTPUT_WRITE_FAILED", "Could not write command output.", err)
		}
		return outputHandled{}, nil
	}
	for ctx.Err() == nil && (deadline.IsZero() || a.Now().Before(deadline)) {
		window := req
		if !cursor.IsZero() {
			start := cursor.Add(-followOverlap)
			if start.Before(floor) {
				start = floor
			}
			window.Path = setQuery(req.Path, "created_after", start.UTC().Format(time.RFC3339Nano))
		}
		rows, date, e := a.followWindow(ctx, opts, window, key, baseURL, bootstrap)
		delay := 2 * time.Second
		if ctx.Err() != nil {
			break
		}
		if e != nil {
			if e.HTTPStatus != 429 && e.ExitCode != ExitNetwork && e.HTTPStatus < 500 {
				return nil, e
			}
			if e.HTTPStatus == 429 {
				if e.RetryAfterMs != nil {
					delay = max(delay, time.Duration(*e.RetryAfterMs)*time.Millisecond)
				}
			} else {
				failures++
				delay = min(2*time.Second<<min(failures-1, 4), 30*time.Second)
			}
			if !noticed {
				noticed = true
				var err error
				if opts.Output == "ndjson" {
					err = writeJSON(a.Stdout, map[string]any{"type": "throttled", "retry_after_ms": delay.Milliseconds()})
				} else if opts.Output == "human" && !opts.Quiet {
					message := "Request-log connection interrupted"
					if e.HTTPStatus == 429 {
						message = "Flint is rate limiting this key"
					}
					_, err = fmt.Fprintf(a.Stderr, "%s; pausing %s.\n", message, delay)
				}
				if err != nil {
					return nil, networkError("OUTPUT_WRITE_FAILED", "Could not write command output.", err)
				}
			}
		} else {
			failures, noticed = 0, false
			if date.After(cursor) {
				cursor = date
			}
			if bootstrap {
				floor = cursor
			}
			sort.Slice(rows, func(i, j int) bool {
				if rows[i].at.Equal(rows[j].at) {
					return rows[i].id < rows[j].id
				}
				return rows[i].at.Before(rows[j].at)
			})
			for _, row := range rows {
				_, duplicate := seen[row.id]
				seen[row.id] = row.at
				if row.at.After(cursor) {
					cursor = row.at
				}
				if bootstrap || duplicate || row.at.Before(floor) {
					continue
				}
				var err error
				switch opts.Output {
				case "json":
					logs = append(logs, row.value)
				case "ndjson":
					err = writeJSON(a.Stdout, row.value)
				default:
					err = a.writeFollowHuman(row, opts)
				}
				if err != nil {
					return nil, networkError("OUTPUT_WRITE_FAILED", "Could not write command output.", err)
				}
				count++
				if opts.MaxEvents > 0 && count >= opts.MaxEvents {
					cursor = row.at
					return finish()
				}
			}
			bootstrap = false
			pruneFollowSeen(seen, cursor)
		}
		if !deadline.IsZero() {
			delay = min(delay, max(deadline.Sub(a.Now()), 0))
		}
		if !sleep(ctx, delay) {
			break
		}
	}
	return finish()
}

func pruneFollowSeen(seen map[string]time.Time, cursor time.Time) {
	for id, at := range seen {
		if at.Before(cursor.Add(-followOverlap)) {
			delete(seen, id)
		}
	}
}

func (a *App) writeFollowHuman(row followedLog, opts Options) error {
	v := row.value
	text := func(key string) string {
		if value, ok := v[key]; ok {
			return terminalSafe(fmt.Sprint(value))
		}
		return ""
	}
	status := text("status_code")
	if a.IsTTY() && opts.Color != "never" {
		color := "32"
		if len(status) > 0 && status[0] >= '4' {
			color = "31"
		}
		status = "\x1b[" + color + "m" + status + "\x1b[0m"
	}
	_, err := fmt.Fprintf(a.Stdout, "%s  %-6s %-30s %s  %sms  %s  %s %s  %s %s\n", row.at.Format("15:04:05.000"), text("http_method"), text("path"), status, text("latency_milliseconds"), text("request_id"), text("resource_type"), text("resource_id"), text("error_code"), text("error_summary"))
	return err
}
