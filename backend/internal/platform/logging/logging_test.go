package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"
)

func TestContextFieldsAreLogged(t *testing.T) {
	var buf bytes.Buffer
	log := New(&buf, slog.LevelInfo)
	ctx, _ := WithFields(context.Background())
	SetRequestID(ctx, "req-1")
	SetTenantID(ctx, "tenant-1")
	log.InfoContext(ctx, "hello", "k", "v")

	var line map[string]any
	if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
		t.Fatal(err)
	}
	if line["request_id"] != "req-1" || line["tenant_id"] != "tenant-1" || line["k"] != "v" {
		t.Fatalf("unexpected log line: %v", line)
	}
	if RequestID(ctx) != "req-1" {
		t.Fatal("RequestID did not return the stored id")
	}
}

func TestParseLevel(t *testing.T) {
	for in, want := range map[string]slog.Level{"debug": slog.LevelDebug, "INFO": slog.LevelInfo, "warn": slog.LevelWarn, "error": slog.LevelError, "": slog.LevelInfo} {
		got, err := ParseLevel(in)
		if err != nil || got != want {
			t.Errorf("ParseLevel(%q) = %v, %v", in, got, err)
		}
	}
	if _, err := ParseLevel("verbose"); err == nil {
		t.Error("expected error for unknown level")
	}
}
