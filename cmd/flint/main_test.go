//go:build unix

package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

const signalHelperEnv = "FLINT_TEST_SIGNAL_HELPER"

// The cleanup outlasts duplicateSignalWindow, like a listener saving its
// resume checkpoint after Ctrl-C.
const helperCleanup = 3 * time.Second

func TestMain(m *testing.M) {
	if mode := os.Getenv(signalHelperEnv); mode != "" {
		ctx, stop := signalContext(context.Background())
		fmt.Println("ready")
		<-ctx.Done()
		fmt.Println("canceled")
		if mode == "exit" {
			// A command that cancels at once: run returns and its deferred
			// stop runs while the duplicate signal may still be in flight.
			stop()
			fmt.Println("stopped")
			time.Sleep(500 * time.Millisecond)
		} else {
			time.Sleep(helperCleanup)
		}
		fmt.Println("cleaned up")
		os.Exit(0)
	}
	os.Exit(m.Run())
}

type signalHelper struct {
	command *exec.Cmd
	lines   chan string
	output  strings.Builder
}

func startSignalHelper(t *testing.T, mode string) *signalHelper {
	t.Helper()
	command := exec.Command(os.Args[0], "-test.run=^$")
	command.Env = append(os.Environ(), signalHelperEnv+"="+mode)
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	helper := &signalHelper{command: command, lines: make(chan string, 8)}
	t.Cleanup(func() {
		_ = command.Process.Kill()
		_ = command.Wait()
	})
	go func() {
		defer close(helper.lines)
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			helper.lines <- scanner.Text()
		}
	}()
	helper.expect(t, "ready")
	return helper
}

func (h *signalHelper) expect(t *testing.T, want string) {
	t.Helper()
	select {
	case line, ok := <-h.lines:
		if !ok || line != want {
			t.Fatalf("helper output = %q (open=%t), want %q", line, ok, want)
		}
		h.output.WriteString(line + "\n")
	case <-time.After(10 * time.Second):
		t.Fatalf("helper did not print %q", want)
	}
}

func (h *signalHelper) signal(t *testing.T, value syscall.Signal) {
	t.Helper()
	if err := h.command.Process.Signal(value); err != nil {
		t.Fatalf("send %v: %v", value, err)
	}
}

func (h *signalHelper) wait(t *testing.T) *exec.ExitError {
	t.Helper()
	result := make(chan error, 1)
	go func() {
		for line := range h.lines {
			h.output.WriteString(line + "\n")
		}
		result <- h.command.Wait()
	}()
	select {
	case err := <-result:
		if err == nil {
			return nil
		}
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("wait: %v", err)
		}
		return exitErr
	case <-time.After(helperCleanup + 10*time.Second):
		t.Fatal("helper did not exit")
		return nil
	}
}

func TestDuplicateSignalDoesNotInterruptGracefulCancellation(t *testing.T) {
	t.Parallel()
	helper := startSignalHelper(t, "cleanup")
	// One Ctrl-C under the npm wrapper: the terminal's copy, the wrapper's
	// forwarded copy, and npm's forwarded copy when started through npx.
	helper.signal(t, syscall.SIGINT)
	helper.signal(t, syscall.SIGINT)
	helper.expect(t, "canceled")
	helper.signal(t, syscall.SIGTERM)
	if exitErr := helper.wait(t); exitErr != nil {
		t.Fatalf("duplicate signal stopped the process: %v\noutput:\n%s", exitErr, &helper.output)
	}
	if !strings.Contains(helper.output.String(), "cleaned up") {
		t.Fatalf("cleanup did not finish:\n%s", &helper.output)
	}
}

func TestDuplicateSignalDoesNotKillAFastExit(t *testing.T) {
	t.Parallel()
	helper := startSignalHelper(t, "exit")
	helper.signal(t, syscall.SIGINT)
	helper.expect(t, "canceled")
	helper.expect(t, "stopped")
	helper.signal(t, syscall.SIGINT)
	if exitErr := helper.wait(t); exitErr != nil {
		t.Fatalf("duplicate signal after stop killed the exiting process: %v\noutput:\n%s", exitErr, &helper.output)
	}
}

func TestLaterSecondSignalStopsTheProcess(t *testing.T) {
	t.Parallel()
	helper := startSignalHelper(t, "cleanup")
	helper.signal(t, syscall.SIGINT)
	helper.expect(t, "canceled")
	time.Sleep(duplicateSignalWindow + 500*time.Millisecond)
	helper.signal(t, syscall.SIGINT)
	exitErr := helper.wait(t)
	if exitErr == nil {
		t.Fatalf("second signal after the duplicate window did not stop the process:\n%s", &helper.output)
	}
	status, ok := exitErr.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() || status.Signal() != syscall.SIGINT {
		t.Fatalf("exit = %v, want termination by SIGINT", exitErr)
	}
	if strings.Contains(helper.output.String(), "cleaned up") {
		t.Fatalf("cleanup finished despite the second signal:\n%s", &helper.output)
	}
}
