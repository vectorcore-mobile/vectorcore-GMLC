package diameter

import (
	"errors"
	"net"
	"syscall"
	"testing"

	"github.com/fiorix/go-diameter/v4/diam"
	"github.com/ishidawataru/sctp"
)

func TestSCTPConfigurationAndDiameterPPID(t *testing.T) {
	if diam.DiameterPPID != 46 {
		t.Fatalf("Diameter SCTP PPID=%d", diam.DiameterPPID)
	}
	c := TransportConfig{Address: "127.0.0.1:3868", Transport: "sctp", OriginHost: "gmlc.example", OriginRealm: "example", HostIP: net.ParseIP("127.0.0.1"), ConnectTimeout: 1, WatchdogInterval: 1, WatchdogTimeout: 1}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	c.Transport = "udp"
	if err := c.Validate(); err == nil {
		t.Fatal("non SCTP/TCP transport accepted")
	}
}
func TestSCTPKernelAvailability(t *testing.T) {
	a := &sctp.SCTPAddr{IPAddrs: []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}, Port: 0}
	ln, err := sctp.ListenSCTP("sctp", a)
	if err != nil {
		if errors.Is(err, syscall.EPROTONOSUPPORT) || errors.Is(err, syscall.EAFNOSUPPORT) || errors.Is(err, syscall.ENOPROTOOPT) {
			t.Skipf("kernel SCTP unavailable: %v", err)
		}
		t.Fatal(err)
	}
	defer ln.Close()
}
