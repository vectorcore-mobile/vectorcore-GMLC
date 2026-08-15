package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func validDiameter(peers string) string {
	return `mlp: {listen_address: "127.0.0.1:9210"}
database: {path: "/tmp/gmlc.db", checkpoint_pages: 1}
retention: {request: "1h", result: "1h"}
logging: {file: "/tmp/gmlc.log", level: "info"}
clients: [{id: c, bearer_token: t, services: [immediate], target_prefixes: ["1"]}]
diameter:
  origin_host: gmlc.example
  origin_realm: example
  host_ip_address: 127.0.0.1
  connection_timeout: 1s
  request_timeout: 1s
  reconnect_min: 1s
  reconnect_max: 2s
  watchdog_interval: 1s
  watchdog_timeout: 1s
  shutdown_timeout: 1s
  peers:
` + peers
}
func loadText(t *testing.T, text string) error {
	t.Helper()
	p := filepath.Join(t.TempDir(), "gmlc.yaml")
	if err := os.WriteFile(p, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(p)
	return err
}
func TestDiameterPeerConfiguration(t *testing.T) {
	for _, tt := range []struct {
		name, peers string
		ok          bool
	}{
		{"tcp", "    - {name: hss, address: '127.0.0.1:3868', transport: tcp}\n", true},
		{"sctp", "    - {name: relay, address: '127.0.0.1:3868', transport: sctp, expected_origin_host: relay.example, expected_origin_realm: example}\n", true},
		{"mixed", "    - {name: a, address: 'a:1', transport: tcp}\n    - {name: b, address: 'b:2', transport: sctp}\n", true},
		{"invalid", "    - {name: a, address: 'a:1', transport: udp}\n", false},
		{"duplicate", "    - {name: a, address: 'a:1', transport: tcp}\n    - {name: b, address: 'a:1', transport: tcp}\n", false},
	} {
		if err := loadText(t, validDiameter(tt.peers)); (err == nil) != tt.ok {
			t.Errorf("%s: %v", tt.name, err)
		}
	}
	for _, key := range []string{"enabled: true", "mode: dra", "dra: {}", "direct: {}", "product_name: x", "vendor_id: 1", "slh: {}"} {
		text := strings.Replace(validDiameter("    - {name: a, address: 'a:1', transport: tcp}\n"), "  peers:", "  "+key+"\n  peers:", 1)
		if err := loadText(t, text); err == nil {
			t.Errorf("obsolete %q accepted", key)
		}
	}
	if err := loadText(t, strings.Replace(validDiameter("    - {name: a, address: 'a:1', transport: tcp}\n"), "  peers:\n", "  peers: []\n", 1)); err == nil {
		t.Error("empty peers accepted")
	}
}
func TestMLPConfigRequiresListenAddress(t *testing.T) {
	text := strings.Replace(validDiameter("    - {name: a, address: 'a:1', transport: tcp}\n"), `mlp: {listen_address: "127.0.0.1:9210"}`+"\n", "", 1)
	if err := loadText(t, text); err == nil {
		t.Error("missing mlp.listen_address accepted")
	}
}
func TestMLPConfigValidWithListenAddress(t *testing.T) {
	text := validDiameter("    - {name: a, address: 'a:1', transport: tcp}\n")
	c, err := func() (Config, error) {
		p := filepath.Join(t.TempDir(), "gmlc.yaml")
		if err := os.WriteFile(p, []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
		return Load(p)
	}()
	if err != nil {
		t.Fatalf("valid mlp config rejected: %v", err)
	}
	if c.MLP.ListenAddress != "127.0.0.1:9210" {
		t.Fatalf("mlp config not parsed: %#v", c.MLP)
	}
	if c.MLP.SyncWaitTimeout <= 0 || c.MLP.MaxSyncWaitTimeout <= 0 || c.MLP.ShutdownTimeout <= 0 {
		t.Fatalf("mlp defaults not applied: %#v", c.MLP)
	}
}
func TestMLPReportingDisabledByDefaultNeedsNoURLs(t *testing.T) {
	if err := loadText(t, validDiameter("    - {name: a, address: 'a:1', transport: tcp}\n")); err != nil {
		t.Fatalf("mlp_reporting disabled by default should not require any urls: %v", err)
	}
}
func TestMLPReportingEnabledRequiresAtLeastOneReportURL(t *testing.T) {
	text := validDiameter("    - {name: a, address: 'a:1', transport: tcp}\n") + "mlp_reporting: {enabled: true}\n"
	if err := loadText(t, text); err == nil {
		t.Error("mlp_reporting.enabled with no report urls accepted")
	}
}
func TestMLPReportingRequiresURLAndClientIDTogether(t *testing.T) {
	for _, tt := range []struct {
		name, yaml string
		ok         bool
	}{
		{"url without client id", `mlp_reporting: {enabled: true, standard_report_url: "http://x/"}`, false},
		{"client id without url", `mlp_reporting: {enabled: true, standard_report_client_id: "c"}`, false},
		{"both set", `mlp_reporting: {enabled: true, standard_report_url: "http://x/", standard_report_client_id: "c"}`, true},
		{"emergency url without client id", `mlp_reporting: {enabled: true, emergency_report_url: "http://x/"}`, false},
		{"emergency both set", `mlp_reporting: {enabled: true, emergency_report_url: "http://x/", emergency_report_client_id: "c"}`, true},
	} {
		text := validDiameter("    - {name: a, address: 'a:1', transport: tcp}\n") + tt.yaml + "\n"
		err := loadText(t, text)
		if (err == nil) != tt.ok {
			t.Errorf("%s: err=%v, want ok=%v", tt.name, err, tt.ok)
		}
	}
}
func TestMLPReportingValidConfigParsesWithDefaultTimeout(t *testing.T) {
	text := validDiameter("    - {name: a, address: 'a:1', transport: tcp}\n") + `mlp_reporting: {enabled: true, standard_report_url: "http://console.example/reports/standard", standard_report_client_id: "lcs-console"}` + "\n"
	p := filepath.Join(t.TempDir(), "gmlc.yaml")
	if err := os.WriteFile(p, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatalf("valid mlp_reporting config rejected: %v", err)
	}
	if !c.MLPReporting.Enabled || c.MLPReporting.StandardReportURL != "http://console.example/reports/standard" || c.MLPReporting.StandardReportClientID != "lcs-console" {
		t.Fatalf("mlp_reporting config not parsed: %#v", c.MLPReporting)
	}
	if c.MLPReporting.Timeout <= 0 {
		t.Fatalf("mlp_reporting.timeout default not applied: %#v", c.MLPReporting)
	}
}
func TestHSSRealmDefaultsAndOverride(t *testing.T) {
	p := filepath.Join(t.TempDir(), "gmlc.yaml")
	if err := os.WriteFile(p, []byte(validDiameter("    - {name: a, address: 'a:1', transport: tcp}\n")), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.Diameter.HSSRealm != c.Diameter.OriginRealm {
		t.Fatalf("expected HSSRealm to default to OriginRealm, got %q vs %q", c.Diameter.HSSRealm, c.Diameter.OriginRealm)
	}
	text := strings.Replace(validDiameter("    - {name: a, address: 'a:1', transport: tcp}\n"), "  host_ip_address: 127.0.0.1\n", "  host_ip_address: 127.0.0.1\n  hss_realm: hss-realm.example\n  hss_host: hss.example\n", 1)
	p2 := filepath.Join(t.TempDir(), "gmlc2.yaml")
	if err := os.WriteFile(p2, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	c2, err := Load(p2)
	if err != nil {
		t.Fatal(err)
	}
	if c2.Diameter.HSSRealm != "hss-realm.example" || c2.Diameter.HSSHost != "hss.example" {
		t.Fatalf("explicit hss_realm/hss_host not honored: %+v", c2.Diameter)
	}
}
func TestClientTypeDefaultAndValues(t *testing.T) {
	p := filepath.Join(t.TempDir(), "gmlc.yaml")
	if err := os.WriteFile(p, []byte(validDiameter("    - {name: a, address: 'a:1', transport: tcp}\n")), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.Clients[0].LCSClientType != "value_added_services" || c.Clients[0].ClientTypeValue() != 1 {
		t.Fatalf("expected default value_added_services, got %+v", c.Clients[0])
	}
	for name, want := range map[string]uint32{"emergency_services": 0, "value_added_services": 1, "plmn_operator_services": 2, "lawful_intercept_services": 3} {
		text := strings.Replace(validDiameter("    - {name: a, address: 'a:1', transport: tcp}\n"), "target_prefixes: [\"1\"]}]", "target_prefixes: [\"1\"], lcs_client_type: "+name+"}]", 1)
		p2 := filepath.Join(t.TempDir(), "gmlc.yaml")
		if err := os.WriteFile(p2, []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
		c2, err := Load(p2)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := c2.Clients[0].ClientTypeValue(); got != want {
			t.Fatalf("%s: got %d, want %d", name, got, want)
		}
	}
	invalid := strings.Replace(validDiameter("    - {name: a, address: 'a:1', transport: tcp}\n"), "target_prefixes: [\"1\"]}]", "target_prefixes: [\"1\"], lcs_client_type: bogus}]", 1)
	if err := loadText(t, invalid); err == nil {
		t.Fatal("invalid lcs_client_type accepted")
	}
}
func TestPrivacyCheckDefaultAndValues(t *testing.T) {
	p := filepath.Join(t.TempDir(), "gmlc.yaml")
	if err := os.WriteFile(p, []byte(validDiameter("    - {name: a, address: 'a:1', transport: tcp}\n")), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.Clients[0].LCSPrivacyCheck != "allowed_without_notification" || c.Clients[0].PrivacyCheckValue() != 0 {
		t.Fatalf("expected default allowed_without_notification, got %+v", c.Clients[0])
	}
	for name, want := range map[string]uint32{"allowed_without_notification": 0, "allowed_with_notification": 1, "allowed_if_no_response": 2, "restricted_if_no_response": 3, "not_allowed": 4} {
		text := strings.Replace(validDiameter("    - {name: a, address: 'a:1', transport: tcp}\n"), "target_prefixes: [\"1\"]}]", "target_prefixes: [\"1\"], lcs_privacy_check: "+name+"}]", 1)
		p2 := filepath.Join(t.TempDir(), "gmlc.yaml")
		if err := os.WriteFile(p2, []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
		c2, err := Load(p2)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := c2.Clients[0].PrivacyCheckValue(); got != want {
			t.Fatalf("%s: got %d, want %d", name, got, want)
		}
	}
	invalid := strings.Replace(validDiameter("    - {name: a, address: 'a:1', transport: tcp}\n"), "target_prefixes: [\"1\"]}]", "target_prefixes: [\"1\"], lcs_privacy_check: bogus}]", 1)
	if err := loadText(t, invalid); err == nil {
		t.Fatal("invalid lcs_privacy_check accepted")
	}
}
