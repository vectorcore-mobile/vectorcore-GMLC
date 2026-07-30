package logging

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIndependentFileAndConsoleLevels(t *testing.T) {
	p := filepath.Join(t.TempDir(), "g.log")
	var c bytes.Buffer
	close, e := Setup(p, "info", true, &c)
	if e != nil {
		t.Fatal(e)
	}
	slog.Debug("debug-message")
	slog.Info("info-message")
	if e = close(); e != nil {
		t.Fatal(e)
	}
	b, e := os.ReadFile(p)
	if e != nil {
		t.Fatal(e)
	}
	if strings.Contains(string(b), "debug-message") || !strings.Contains(string(b), "info-message") {
		t.Fatalf("file=%s", b)
	}
	if !strings.Contains(c.String(), "debug-message") {
		t.Fatal("debug absent from console")
	}
}
