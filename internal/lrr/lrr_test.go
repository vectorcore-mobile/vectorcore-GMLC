package lrr

import (
	"context"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/fiorix/go-diameter/v4/diam"
	"github.com/fiorix/go-diameter/v4/diam/avp"
	"github.com/fiorix/go-diameter/v4/diam/datatype"
	"github.com/fiorix/go-diameter/v4/diam/dict"
	"github.com/vectorcore/gmlc/internal/slg"
	"github.com/vectorcore/gmlc/internal/storage"
	"github.com/vectorcore/gmlc/internal/storage/sqlite"
)

func testStore(t *testing.T) *sqlite.Store {
	t.Helper()
	s, e := sqlite.Open(context.Background(), sqlite.Config{Path: filepath.Join(t.TempDir(), "gmlc.db"), Synchronous: "FULL", CheckpointPages: 10})
	if e != nil {
		t.Fatal(e)
	}
	if e = s.Migrate(context.Background()); e != nil {
		t.Fatal(e)
	}
	if e = s.UpsertClient(context.Background(), storage.Client{ID: "c", CredentialHash: []byte("h"), Enabled: true}); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	return s
}

// testConnPair returns a diam.Conn wrapping one end of an in-memory
// net.Pipe, and the raw net.Conn for the other end — enough to invoke
// ServeDIAM directly and read back whatever it writes, without a real
// listener/dial round trip (that's what the internal/diameter loopback
// integration test already covers end to end).
func testConnPair(t *testing.T) (diam.Conn, net.Conn) {
	t.Helper()
	server, client := net.Pipe()
	c, e := diam.NewConn(server, "test", nil, dict.Default)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { c.Close(); client.Close() })
	return c, client
}

func TestServeDIAMDecodeFailureRespondsUnableToComply(t *testing.T) {
	s := testStore(t)
	codec, e := slg.New(slg.Config{OriginHost: "gmlc.example", OriginRealm: "example"}, nil)
	if e != nil {
		t.Fatal(e)
	}
	h := New(codec, s)

	c, client := testConnPair(t)
	// Location-Event is mandatory (TS 29.172 7.3.3); deliberately omitted so
	// DecodeLRR fails and ServeDIAM must fall back to UnableToComply rather
	// than silently claiming success.
	m := diam.NewRequest(slg.CommandLocationReport, slg.ApplicationID, dict.Default)
	m.NewAVP(avp.SessionID, avp.Mbit, 0, datatype.UTF8String("mme.example;lrr;bad"))
	m.NewAVP(avp.AuthSessionState, avp.Mbit, 0, datatype.Enumerated(1))

	done := make(chan struct{})
	go func() { h.ServeDIAM(c, m); close(done) }()

	_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
	ans, e := diam.ReadMessage(client, dict.Default)
	if e != nil {
		t.Fatal(e)
	}
	<-done
	if ans.Header.CommandFlags&diam.RequestFlag != 0 {
		t.Fatal("expected an answer, not a request")
	}
	rc, err := ans.FindAVP(avp.ResultCode, 0)
	if err != nil || rc.Data.(datatype.Unsigned32) != diam.UnableToComply {
		t.Fatalf("expected UnableToComply, got %v %v", rc, err)
	}
}
