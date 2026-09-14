package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A helper process gives the server inherited OS stdin, which unlike io.Pipe
// can have a blocking Read that is not interrupted by closing the file.
func TestVerifiedMCPProcessHelper(t *testing.T) {
	if os.Getenv("FLINT_TEST_MCP_PROCESS") != "1" {
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	a := New(BuildInfo{Version: "test"})
	a.ConfigDir = os.Getenv("FLINT_TEST_MCP_CONFIG_DIR")
	a.WorkingDir = a.ConfigDir
	a.Context = ctx
	os.Exit(a.Run([]string{"mcp", "serve", "--no-input"}))
}

func TestVerifiedMCPProcessShutdown(t *testing.T) {
	for _, action := range []string{"eof", "sigterm", "sigint", "jq-eof", "jq-sigterm", "jq-sigint", "jq-cancel"} {
		t.Run(action, func(t *testing.T) {
			shutdownAction := strings.TrimPrefix(action, "jq-")
			if runtime.GOOS == "windows" && shutdownAction != "eof" && shutdownAction != "cancel" {
				t.Skip("Windows does not support sending these process signals")
			}
			command := exec.Command(os.Args[0], "-test.run=^TestVerifiedMCPProcessHelper$")
			command.Env = append(os.Environ(), "FLINT_TEST_MCP_PROCESS=1", "FLINT_TEST_MCP_CONFIG_DIR="+t.TempDir(), "FLINT_PROFILE=default", "FLINT_MERCHANT=", "FLINT_OUTPUT=", "FLINT_COLOR=")
			stdin, err := command.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			defer stdin.Close()
			stdout, err := command.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			var stderr bytes.Buffer
			command.Stderr = &stderr
			if err := command.Start(); err != nil {
				t.Fatal(err)
			}
			defer command.Process.Kill()
			if err := json.NewEncoder(stdin).Encode(map[string]any{
				"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{
					"protocolVersion": latestMCPProtocolVersion, "capabilities": map[string]any{},
					"clientInfo": map[string]any{"name": "test", "version": "1"},
				},
			}); err != nil {
				t.Fatal(err)
			}
			ready := make(chan []byte, 1)
			reader := bufio.NewReader(stdout)
			readLine := func() { line, _ := reader.ReadBytes('\n'); ready <- line }
			go readLine()
			select {
			case line := <-ready:
				if !bytes.Contains(line, []byte(`"protocolVersion"`)) {
					t.Fatalf("initialize failed: %s", line)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("MCP initialization timed out")
			}
			if strings.HasPrefix(action, "jq-") {
				encoder := json.NewEncoder(stdin)
				for _, request := range []map[string]any{
					{"jsonrpc": "2.0", "method": "notifications/initialized"},
					{"jsonrpc": "2.0", "id": 2, "method": "tools/call", "params": map[string]any{
						"name": "version", "arguments": map[string]any{"_flint": map[string]any{"jq": "until(false; .)"}},
					}},
				} {
					if err := encoder.Encode(request); err != nil {
						t.Fatal(err)
					}
				}
				// Allow the worker to enter the non-producing jq loop before
				// testing cancellation or process shutdown.
				time.Sleep(100 * time.Millisecond)
				if shutdownAction == "cancel" {
					if err := encoder.Encode(map[string]any{"jsonrpc": "2.0", "method": "notifications/cancelled", "params": map[string]any{"requestId": 2}}); err != nil {
						t.Fatal(err)
					}
					go readLine()
					select {
					case line := <-ready:
						var response map[string]any
						if err := json.Unmarshal(line, &response); err != nil {
							t.Fatal(err)
						}
						code, _ := lookupPath(response, "error.code")
						if response["id"] != float64(2) || code != float64(-32800) {
							t.Fatalf("cancellation response: %s", line)
						}
					case <-time.After(4 * time.Second):
						t.Fatal("jq tool did not respond to cancellation")
					}
				}
			}
			switch shutdownAction {
			case "eof", "cancel":
				err = stdin.Close()
			case "sigterm":
				err = command.Process.Signal(syscall.SIGTERM)
			case "sigint":
				err = command.Process.Signal(os.Interrupt)
			}
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- command.Wait() }()
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("shutdown: %v stderr=%s", err, &stderr)
				}
			case <-time.After(4 * time.Second):
				_ = command.Process.Kill()
				<-done
				t.Fatalf("MCP did not exit after %s; stderr=%s", action, &stderr)
			}
		})
	}
}
