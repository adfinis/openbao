package main

import (
	"context"
	"fmt"
	"strings"
	"time"
)

func init() {
	RegisterWorkload("kv", newKVWorkload)
}

// KVWorkload implements the mixed KV v2 + DR status workload that matches
// the original bash script semantics.
type KVWorkload struct {
	cfg     *Config
	clients *ClientSet

	// Pre-built payloads (same as bash: repeated 's' / 'L' chars).
	payloadSmall string
	payloadLarge string
}

func newKVWorkload(cfg *Config, clients *ClientSet) (Workload, error) {
	return &KVWorkload{
		cfg:          cfg,
		clients:      clients,
		payloadSmall: strings.Repeat("s", cfg.SmallBytes),
		payloadLarge: strings.Repeat("L", cfg.LargeBytes),
	}, nil
}

func (kv *KVWorkload) Name() string { return "kv" }

func (kv *KVWorkload) Ops() []OpWeight {
	return []OpWeight{
		{Name: "put", Weight: kv.cfg.PutPercent},
		{Name: "get_primary", Weight: kv.cfg.GetPrimaryPercent},
		{Name: "status_s1", Weight: kv.cfg.StatusS1Percent},
		{Name: "status_s2", Weight: kv.cfg.StatusS2Percent},
	}
}

func (kv *KVWorkload) Execute(ctx context.Context, op string, w *Worker) Event {
	switch op {
	case "put":
		return kv.execPut(ctx, w)
	case "get_primary":
		return kv.execGetPrimary(ctx, w)
	case "status_s1":
		return kv.execStatus(ctx, w, "secondary1", kv.clients.Secondary1)
	case "status_s2":
		return kv.execStatus(ctx, w, "secondary2", kv.clients.Secondary2)
	default:
		return Event{
			TS:     time.Now(),
			Worker: w.ID,
			Op:     op,
			Err:    fmt.Sprintf("unknown op: %s", op),
		}
	}
}

func (kv *KVWorkload) execPut(ctx context.Context, w *Worker) Event {
	keyIdx := w.ChooseKeyIndex()
	keyPath := fmt.Sprintf("%s/%s/%s/k%d", kv.cfg.KVMount, kv.cfg.KeyPrefix, w.RunID, keyIdx)
	seq := w.NextSeq()
	payload := kv.choosePayload(w)
	payloadBytes := len(payload)

	data := map[string]interface{}{
		"payload": payload,
		"seq":     seq,
		"run_id":  w.RunID,
	}

	var code int
	var err error
	start := time.Now()

	// Retry loop matching bash write_put behavior.
	maxAttempts := kv.cfg.WriteRetries + 1
	for attempt := 0; attempt < maxAttempts; attempt++ {
		code, err = kv.clients.Primary.KVPut(ctx, kv.cfg.KVMount, fmt.Sprintf("%s/%s/k%d", kv.cfg.KeyPrefix, w.RunID, keyIdx), data)
		if err == nil && (code == 200 || code == 204) {
			break
		}
		if attempt < maxAttempts-1 {
			time.Sleep(time.Duration(attempt+2) * 100 * time.Millisecond)
		}
	}

	latency := time.Since(start)
	ev := Event{
		TS:        start,
		Worker:    w.ID,
		Op:        "put",
		Role:      "primary",
		Key:       keyPath,
		Seq:       seq,
		Bytes:     payloadBytes,
		LatencyNS: latency.Nanoseconds(),
		Code:      code,
	}
	if err != nil {
		ev.Err = classifyError(err)
	}
	return ev
}

func (kv *KVWorkload) execGetPrimary(ctx context.Context, w *Worker) Event {
	keyIdx := w.ChooseKeyIndex()
	keyPath := fmt.Sprintf("%s/%s/%s/k%d", kv.cfg.KVMount, kv.cfg.KeyPrefix, w.RunID, keyIdx)

	start := time.Now()
	code, _, err := kv.clients.Primary.KVGet(ctx, kv.cfg.KVMount, fmt.Sprintf("%s/%s/k%d", kv.cfg.KeyPrefix, w.RunID, keyIdx))
	latency := time.Since(start)

	ev := Event{
		TS:        start,
		Worker:    w.ID,
		Op:        "get_primary",
		Role:      "primary",
		Key:       keyPath,
		LatencyNS: latency.Nanoseconds(),
		Code:      code,
	}
	if err != nil {
		ev.Err = classifyError(err)
	}
	return ev
}

func (kv *KVWorkload) execStatus(ctx context.Context, w *Worker, role string, client *BaoClient) Event {
	op := "status_s1"
	if role == "secondary2" {
		op = "status_s2"
	}

	if client == nil {
		// Fallback: read from primary (mirrors bash behavior).
		return kv.execGetPrimary(ctx, w)
	}

	start := time.Now()
	_, code, err := client.DRStatus(ctx)
	latency := time.Since(start)

	ev := Event{
		TS:        start,
		Worker:    w.ID,
		Op:        op,
		Role:      role,
		LatencyNS: latency.Nanoseconds(),
		Code:      code,
	}
	if err != nil {
		ev.Err = classifyError(err)
	}
	return ev
}

func (kv *KVWorkload) choosePayload(w *Worker) string {
	roll := w.Rand.Intn(100)
	if roll < kv.cfg.LargePayloadPercent {
		return kv.payloadLarge
	}
	return kv.payloadSmall
}

// classifyError returns a short error class string for recording.
func classifyError(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	switch {
	case strings.Contains(s, "connection refused"):
		return "conn_refused"
	case strings.Contains(s, "connection reset"):
		return "conn_reset"
	case strings.Contains(s, "timeout"):
		return "timeout"
	case strings.Contains(s, "EOF"):
		return "eof"
	case strings.Contains(s, "TLS"):
		return "tls_error"
	case strings.Contains(s, "context canceled"):
		return "ctx_canceled"
	case strings.Contains(s, "context deadline exceeded"):
		return "ctx_deadline"
	case strings.Contains(s, "dial tcp") || strings.Contains(s, "lookup ") || strings.Contains(s, "no such host"):
		return "transport_error"
	default:
		// Truncate to keep NDJSON compact.
		if len(s) > 80 {
			return s[:80]
		}
		return s
	}
}
