package scenario

import "testing"

func TestOversizedCheckpointTuningProfile(t *testing.T) {
	values, err := tuningProfileValues("oversized-checkpoint")
	if err != nil {
		t.Fatalf("tuningProfileValues returned error: %v", err)
	}

	if got, want := values["checkpoint_per_relationship_budget_bytes"], 262144; got != want {
		t.Fatalf("checkpoint_per_relationship_budget_bytes = %v, want %v", got, want)
	}
	if got, want := values["checkpoint_artifact_per_relationship_budget_bytes"], 262144; got != want {
		t.Fatalf("checkpoint_artifact_per_relationship_budget_bytes = %v, want %v", got, want)
	}
	if got, want := values["checkpoint_global_budget_bytes"], 536870912; got != want {
		t.Fatalf("checkpoint_global_budget_bytes = %v, want %v", got, want)
	}
	if got, want := values["checkpoint_artifact_global_budget_bytes"], 536870912; got != want {
		t.Fatalf("checkpoint_artifact_global_budget_bytes = %v, want %v", got, want)
	}
}

func TestOutOfHorizonLowFanoutTuningProfile(t *testing.T) {
	values, err := tuningProfileValues("out-of-horizon-low-fanout")
	if err != nil {
		t.Fatalf("tuningProfileValues returned error: %v", err)
	}

	if got, want := values["reconcile_max_range_drilldown_rpcs"], 1; got != want {
		t.Fatalf("reconcile_max_range_drilldown_rpcs = %v, want %v", got, want)
	}
	if got, want := values["stream_journal_max_bytes"], 262144; got != want {
		t.Fatalf("stream_journal_max_bytes = %v, want %v", got, want)
	}
	if got, want := values["reconcile_max_inflight_tasks"], 8; got != want {
		t.Fatalf("reconcile_max_inflight_tasks = %v, want %v", got, want)
	}
}

func TestCheckpointVerifyReasonRetryable(t *testing.T) {
	tests := []struct {
		name   string
		reason string
		want   bool
	}{
		{
			name:   "accumulator index race",
			reason: "checkpoint verification returned 400: accumulator_checkpoint_index_mismatch",
			want:   true,
		},
		{
			name:   "primary checkpoint admission",
			reason: "rpc error: code = ResourceExhausted desc = budget_exceeded: checkpoint build concurrency exceeded (1 >= 1); retry",
			want:   true,
		},
		{
			name:   "post handoff transport",
			reason: "rpc error: code = Unavailable desc = connection error: desc = \"transport: Error while dialing: remote error: tls: internal error\"",
			want:   true,
		},
		{
			name:   "primary checkpoint fence",
			reason: "rpc error: code = FailedPrecondition desc = checkpoint index fence failed: checkpoint index fence timeout: have 9003 need 9011",
			want:   true,
		},
		{
			name:   "checksum mismatch",
			reason: "range_checksum_mismatch",
			want:   false,
		},
		{
			name:   "proof mismatch",
			reason: "indexed repair proof mismatch",
			want:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := checkpointVerifyReasonRetryable(tt.reason); got != tt.want {
				t.Fatalf("checkpointVerifyReasonRetryable(%q) = %v, want %v", tt.reason, got, tt.want)
			}
		})
	}
}

func TestTuningWriteRetryable(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "active context canceled", err: errString("POST sys/replication/dr/tuning returned 500: active context canceled after getting state lock"), want: true},
		{name: "standby", err: errString("local node not active standby"), want: true},
		{name: "timeout", err: errString("context deadline exceeded"), want: true},
		{name: "bad payload", err: errString("POST sys/replication/dr/tuning returned 400: invalid value"), want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tuningWriteRetryable(tt.err); got != tt.want {
				t.Fatalf("tuningWriteRetryable(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

type errString string

func (e errString) Error() string {
	return string(e)
}
