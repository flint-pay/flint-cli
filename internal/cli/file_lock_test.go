package cli

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestConcurrentConfigMutationsPreserveAllProfiles(t *testing.T) {
	t.Parallel()
	app := New(BuildInfo{})
	app.ConfigDir = t.TempDir()

	const writers = 20
	var wait sync.WaitGroup
	errors := make(chan error, writers)
	for index := 0; index < writers; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			name := fmt.Sprintf("profile-%02d", index)
			errors <- app.updateConfig(func(config *Config) error {
				if config.Profiles == nil {
					config.Profiles = map[string]Profile{}
				}
				config.Profiles[name] = Profile{MerchantGuard: fmt.Sprintf("mer_%02d", index)}
				return nil
			})
		}(index)
	}
	wait.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatalf("updateConfig() error = %v", err)
		}
	}

	config, err := app.loadConfig()
	if err != nil {
		t.Fatalf("loadConfig() error = %v", err)
	}
	if len(config.Profiles) != writers {
		t.Fatalf("profiles = %d, want %d: %#v", len(config.Profiles), writers, config.Profiles)
	}
}

func TestConcurrentHistoryMutationsPreserveAllEntries(t *testing.T) {
	t.Parallel()
	app := New(BuildInfo{})
	app.ConfigDir = t.TempDir()
	app.Now = func() time.Time { return time.Unix(1_800_000_000, 0).UTC() }

	const writers = 20
	var wait sync.WaitGroup
	errors := make(chan error, writers)
	for index := 0; index < writers; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			errors <- app.recordHistory(
				map[string]any{"data": map[string]any{"id": fmt.Sprintf("cus_%02d", index)}},
				"customers.get",
				historyScope{Profile: "default", Environment: "sandbox"},
			)
		}(index)
	}
	wait.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatalf("recordHistory() error = %v", err)
		}
	}

	history, err := app.loadHistory()
	if err != nil {
		t.Fatalf("loadHistory() error = %v", err)
	}
	if len(history.Entries) != writers {
		t.Fatalf("history entries = %d, want %d: %#v", len(history.Entries), writers, history.Entries)
	}
}
