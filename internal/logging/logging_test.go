package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/endreszabo/zorplogger/session"
)

func TestZorpFormat(t *testing.T) {
	var buf bytes.Buffer
	l, err := New(&buf, "zorp", "info")
	if err != nil {
		t.Fatal(err)
	}
	ctx := session.WithID(context.Background(), "cafe0001")
	Component(l, "poller").InfoContext(ctx, "poll done", "device", "ap1")
	Component(l, "poller").Debug("hidden")
	out := buf.String()
	if !strings.Contains(out, "poller.info(cafe0001): poll done; device='ap1'") {
		t.Fatalf("unexpected output %q", out)
	}
	if strings.Contains(out, "hidden") {
		t.Fatal("debug line logged at info level")
	}
}

func TestJSONFormat(t *testing.T) {
	var buf bytes.Buffer
	l, err := New(&buf, "json", "debug")
	if err != nil {
		t.Fatal(err)
	}
	Component(l, "mqtt").Debug("connected")
	var m map[string]any
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	if m["component"] != "mqtt" || m["msg"] != "connected" {
		t.Fatalf("unexpected record %v", m)
	}
}

func TestBadInput(t *testing.T) {
	if _, err := New(nil, "xml", "info"); err == nil {
		t.Error("accepted unknown format")
	}
	if _, err := New(nil, "zorp", "loud"); err == nil {
		t.Error("accepted unknown level")
	}
}
