package cli

import "testing"

func TestSnapshotTestClockAndDisputeCommands(t *testing.T) {
	registry := NewRegistry()
	for _, want := range []struct {
		name, method, path, operationID, idempotency string
		sensitive                                    bool
	}{
		{"test-clocks.create", "POST", "/v1/test-clocks", "createTestClock", "optional", false},
		{"test-clocks.list", "GET", "/v1/test-clocks", "listTestClocks", "none", false},
		{"test-clocks.get", "GET", "/v1/test-clocks/{test_clock_id}", "getTestClock", "none", false},
		{"test-clocks.delete", "DELETE", "/v1/test-clocks/{test_clock_id}", "deleteTestClock", "optional", false},
		{"test-clocks.advance", "POST", "/v1/test-clocks/{test_clock_id}/advance", "advanceTestClock", "optional", false},
		{"disputes.simulate", "POST", "/v1/disputes/{dispute_id}/simulate", "simulateDispute", "optional", true},
	} {
		t.Run(want.name, func(t *testing.T) {
			command, ok := registry.ByName(want.name)
			if !ok {
				t.Fatal("reviewed snapshot operation has no generated command")
			}
			if command.Method != want.method || command.APIPath != want.path || command.OperationID != want.operationID {
				t.Fatalf("got %s %s (%s), want %s %s (%s)", command.Method, command.APIPath, command.OperationID, want.method, want.path, want.operationID)
			}
			if got := commandIdempotencyClass(command); got != want.idempotency {
				t.Errorf("idempotency = %s, want %s", got, want.idempotency)
			}
			if command.Sensitive != want.sensitive {
				t.Errorf("sensitive = %t, want %t", command.Sensitive, want.sensitive)
			}
			if !command.AuthRequired || command.Mutation != (want.method != "GET") {
				t.Error("generated command lost authentication or mutation metadata")
			}
			if command.Mutation && (!command.Supports.DryRunClient || !command.Supports.IdempotencyKey) {
				t.Error("generated write lost dry-run or caller idempotency-key support")
			}
		})
	}
}
