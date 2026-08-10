package config

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/vectorcore/gmlc/internal/domain"
	"gopkg.in/yaml.v3"
)

type Config struct {
	Server    Server    `yaml:"server"`
	Database  Database  `yaml:"database"`
	Retention Retention `yaml:"retention"`
	Clients   []Client  `yaml:"clients"`
	Diameter  Diameter  `yaml:"diameter"`
	Logging   Logging   `yaml:"logging"`
	Delivery  Delivery  `yaml:"delivery"`
	LRR          LRR          `yaml:"lrr"`
	MLP          MLP          `yaml:"mlp"`
	MLPReporting MLPReporting `yaml:"mlp_reporting"`
}

// MLPReporting configures unsolicited MLP report pushes — slrep (Standard
// Location Report, for MO-LR events) and emerep (Emergency Location
// Report, for emergency call origination/release/handover events) — sent
// by internal/mlp.Pusher, invoked from internal/lrr on qualifying inbound
// TS 29.172 Location-Report-Requests. Disabled by default. Unlike MLP
// itself (an inbound listener), this is purely outbound: MLP gives GMLC no
// per-subscriber routing concept for a report nothing requested (see
// docs/mlp-le-interface-plan.md's Phase D "start simple" decision), so
// each report type has exactly one operator-configured destination, not a
// routing table.
type MLPReporting struct {
	Enabled bool `yaml:"enabled"`
	// StandardReportURL/StandardReportClientID: destination and the client
	// identity GMLC presents in its own outbound hdr for slrep pushes.
	// Both empty (the default) disables standard-report pushing even if
	// Enabled is true.
	StandardReportURL      string `yaml:"standard_report_url"`
	StandardReportClientID string `yaml:"standard_report_client_id"`
	// EmergencyReportURL/EmergencyReportClientID: same, for emerep pushes.
	EmergencyReportURL      string        `yaml:"emergency_report_url"`
	EmergencyReportClientID string        `yaml:"emergency_report_client_id"`
	Timeout                 time.Duration `yaml:"timeout"`
}

// MLP configures the OMA MLP (Le interface) adapter — internal/mlp — a
// second, independent HTTP listener alongside the REST/JSON one (see
// Server.Enabled), not a replacement for it. Disabled by default: existing
// deployments are unaffected until an operator opts in, matching the
// Delivery/LRR precedent.
type MLP struct {
	Enabled       bool   `yaml:"enabled"`
	ListenAddress string `yaml:"listen_address"`
	// SyncWaitTimeout bounds how long a slir blocks waiting for a target to
	// reach a terminal state when the client's own eqop/resp_timer doesn't
	// say otherwise. MaxSyncWaitTimeout caps how long resp_timer itself may
	// extend that wait to — a client can shorten the wait, never lengthen
	// it past this.
	SyncWaitTimeout    time.Duration `yaml:"sync_wait_timeout"`
	MaxSyncWaitTimeout time.Duration `yaml:"max_sync_wait_timeout"`
	ShutdownTimeout    time.Duration `yaml:"shutdown_timeout"`
}

// LRR gates inbound TS 29.172 Location-Report-Request handling. Disabled by
// default: a GMLC that never enables it registers no request handler, so an
// unexpected LRR simply falls through to the existing answer-only catch-all
// (internal/diameter's ALL_CMD_INDEX handler) and is silently dropped,
// exactly like today's pre-Phase-6 behavior — enabling it is a deliberate
// opt-in, not a default-on protocol upgrade.
type LRR struct {
	Enabled bool `yaml:"enabled"`
}

// Delivery configures the shared HTTP-callback outbox worker (internal/delivery)
// used by both LRR deferred-report delivery and REST-async completion
// delivery. Disabled by default: a GMLC that never enables it has no
// callback secrets to protect and no encryption key requirement.
type Delivery struct {
	Enabled         bool          `yaml:"enabled"`
	MaxAttempts     int           `yaml:"max_attempts"`
	RetryBackoffMin time.Duration `yaml:"retry_backoff_min"`
	RetryBackoffMax time.Duration `yaml:"retry_backoff_max"`
	RequestTimeout  time.Duration `yaml:"request_timeout"`
	// EncryptionKey is a 64-character hex-encoded 32-byte AES-256 key used
	// to encrypt callback secrets at rest. Required only if enabled;
	// operator-supplied deployment secret, like bearer tokens — never
	// source control. Rotating it invalidates every previously-stored
	// callback secret, so treat it with the same care as the database
	// itself, not as a rotatable-on-a-whim value.
	EncryptionKey string `yaml:"encryption_key"`
}
type Logging struct {
	File  string `yaml:"file"`
	Level string `yaml:"level"`
}
type Diameter struct {
	OriginHost        string         `yaml:"origin_host"`
	OriginRealm       string         `yaml:"origin_realm"`
	HostIPAddress     string         `yaml:"host_ip_address"`
	HSSRealm          string         `yaml:"hss_realm"`
	HSSHost           string         `yaml:"hss_host"`
	ConnectionTimeout time.Duration  `yaml:"connection_timeout"`
	RequestTimeout    time.Duration  `yaml:"request_timeout"`
	ReconnectMin      time.Duration  `yaml:"reconnect_min"`
	ReconnectMax      time.Duration  `yaml:"reconnect_max"`
	WatchdogInterval  time.Duration  `yaml:"watchdog_interval"`
	WatchdogTimeout   time.Duration  `yaml:"watchdog_timeout"`
	ShutdownTimeout   time.Duration  `yaml:"shutdown_timeout"`
	Peers             []DiameterPeer `yaml:"peers"`
}
type DiameterPeer struct {
	Name                string `yaml:"name"`
	Address             string `yaml:"address"`
	Transport           string `yaml:"transport"`
	ExpectedOriginHost  string `yaml:"expected_origin_host"`
	ExpectedOriginRealm string `yaml:"expected_origin_realm"`
}
type Server struct {
	// Enabled gates the REST/JSON adapter (internal/httpapi). Defaults to
	// true (applyDefaults) — it isn't being removed by MLP's arrival, this
	// just gives an operator the lever to turn it off later once MLP is
	// trusted, without a code change. Independent of MLP.Enabled: both,
	// either, or neither may run at once (see docs/mlp-le-interface-plan.md).
	Enabled         *bool         `yaml:"enabled"`
	ListenAddress   string        `yaml:"listen_address"`
	ShutdownTimeout time.Duration `yaml:"shutdown_timeout"`
}
type Database struct {
	Path            string        `yaml:"path"`
	BusyTimeout     time.Duration `yaml:"busy_timeout"`
	Synchronous     string        `yaml:"synchronous"`
	CheckpointPages int           `yaml:"checkpoint_pages"`
}
type Retention struct {
	Request       time.Duration `yaml:"request"`
	Result        time.Duration `yaml:"result"`
	PurgeInterval time.Duration `yaml:"purge_interval"`
}
type Client struct {
	ID             string               `yaml:"id"`
	BearerToken    string               `yaml:"bearer_token"`
	Services       []domain.ServiceType `yaml:"services"`
	TargetPrefixes []string             `yaml:"target_prefixes"`
	// LCSClientType selects the TS 29.172 LCS-Client-Type this client's
	// requests are tagged with. Operator-controlled only — never settable via
	// the REST API — since EMERGENCY_SERVICES/LAWFUL_INTERCEPT_SERVICES carry
	// regulatory weight downstream. One of: emergency_services,
	// value_added_services (default), plmn_operator_services,
	// lawful_intercept_services.
	LCSClientType string `yaml:"lcs_client_type"`
	// LCSPrivacyCheck selects the TS 29.172 LCS-Privacy-Check sent for this
	// client's requests. Operator-controlled only — never settable via the
	// REST API — since it governs the target subscriber's own privacy
	// protection, not anything about the requesting client. One of:
	// allowed_without_notification (default), allowed_with_notification,
	// allowed_if_no_response, restricted_if_no_response, not_allowed.
	LCSPrivacyCheck string `yaml:"lcs_privacy_check"`
}

var clientTypeValues = map[string]uint32{
	"emergency_services":        domain.ClientTypeEmergencyServices,
	"value_added_services":      domain.ClientTypeValueAddedServices,
	"plmn_operator_services":    domain.ClientTypePLMNOperatorServices,
	"lawful_intercept_services": domain.ClientTypeLawfulIntercept,
}

var privacyCheckValues = map[string]uint32{
	"allowed_without_notification": domain.PrivacyAllowedWithoutNotification,
	"allowed_with_notification":    domain.PrivacyAllowedWithNotification,
	"allowed_if_no_response":       domain.PrivacyAllowedIfNoResponse,
	"restricted_if_no_response":    domain.PrivacyRestrictedIfNoResponse,
	"not_allowed":                  domain.PrivacyNotAllowed,
}

// ClientTypeValue resolves the configured LCSClientType name to its TS
// 29.172 numeric value. LCSClientType is validated by Config.Validate, so
// this is only called on an already-validated config.
func (c Client) ClientTypeValue() uint32 { return clientTypeValues[c.LCSClientType] }

// PrivacyCheckValue resolves the configured LCSPrivacyCheck name to its TS
// 29.172 numeric value. LCSPrivacyCheck is validated by Config.Validate, so
// this is only called on an already-validated config.
func (c Client) PrivacyCheckValue() uint32 { return privacyCheckValues[c.LCSPrivacyCheck] }

// EncryptionKeyBytes decodes EncryptionKey from hex and validates it's a
// full 32-byte AES-256 key. Called both by Validate (so a bad key is caught
// at startup, not on the first delivery attempt) and by whatever
// constructs the delivery.Worker.
func (d Delivery) EncryptionKeyBytes() ([]byte, error) {
	key, err := hex.DecodeString(d.EncryptionKey)
	if err != nil {
		return nil, fmt.Errorf("must be hex-encoded: %w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("must decode to 32 bytes (AES-256), got %d", len(key))
	}
	return key, nil
}

func Load(path string) (Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	var c Config
	if err = rejectObsoleteDiameterKeys(b); err != nil {
		return c, err
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err = dec.Decode(&c); err != nil {
		return c, err
	}
	c.applyDefaults()
	return c, c.Validate()
}
func (c *Config) applyDefaults() {
	if c.Server.Enabled == nil {
		v := true
		c.Server.Enabled = &v
	}
	if c.Server.ShutdownTimeout <= 0 {
		c.Server.ShutdownTimeout = 10 * time.Second
	}
	if c.MLP.SyncWaitTimeout <= 0 {
		c.MLP.SyncWaitTimeout = 20 * time.Second
	}
	if c.MLP.MaxSyncWaitTimeout <= 0 {
		c.MLP.MaxSyncWaitTimeout = 60 * time.Second
	}
	if c.MLP.ShutdownTimeout <= 0 {
		c.MLP.ShutdownTimeout = 10 * time.Second
	}
	if c.MLPReporting.Timeout <= 0 {
		c.MLPReporting.Timeout = 10 * time.Second
	}
	if c.Database.BusyTimeout <= 0 {
		c.Database.BusyTimeout = 5 * time.Second
	}
	if c.Database.Synchronous == "" {
		c.Database.Synchronous = "FULL"
	}
	if c.Logging.Level == "" {
		c.Logging.Level = "info"
	}
	if c.Retention.PurgeInterval <= 0 {
		c.Retention.PurgeInterval = time.Hour
	}
	// HSS realm defaults to the GMLC's own realm, matching the common
	// same-operator deployment where GMLC and HSS share a Diameter realm.
	// diameter.hss_realm lets cross-realm/interconnect deployments override it.
	if c.Diameter.HSSRealm == "" {
		c.Diameter.HSSRealm = c.Diameter.OriginRealm
	}
	for i := range c.Clients {
		if c.Clients[i].LCSClientType == "" {
			c.Clients[i].LCSClientType = "value_added_services"
		}
		if c.Clients[i].LCSPrivacyCheck == "" {
			c.Clients[i].LCSPrivacyCheck = "allowed_without_notification"
		}
	}
	if c.Delivery.MaxAttempts <= 0 {
		c.Delivery.MaxAttempts = 5
	}
	if c.Delivery.RetryBackoffMin <= 0 {
		c.Delivery.RetryBackoffMin = time.Second
	}
	if c.Delivery.RetryBackoffMax < c.Delivery.RetryBackoffMin {
		c.Delivery.RetryBackoffMax = 5 * time.Minute
	}
	if c.Delivery.RequestTimeout <= 0 {
		c.Delivery.RequestTimeout = 10 * time.Second
	}
}
func (c Config) Validate() error {
	if c.Logging.File == "" {
		return fmt.Errorf("logging.file is required")
	}
	switch strings.ToLower(c.Logging.Level) {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("logging.level must be debug, info, warn, or error")
	}
	serverEnabled := c.Server.Enabled == nil || *c.Server.Enabled
	if serverEnabled && c.Server.ListenAddress == "" {
		return fmt.Errorf("server.listen_address is required")
	}
	if c.MLP.Enabled {
		if c.MLP.ListenAddress == "" {
			return fmt.Errorf("mlp.listen_address is required")
		}
		if serverEnabled && c.MLP.ListenAddress == c.Server.ListenAddress {
			return fmt.Errorf("mlp.listen_address must differ from server.listen_address")
		}
		if c.MLP.SyncWaitTimeout <= 0 || c.MLP.MaxSyncWaitTimeout <= 0 || c.MLP.SyncWaitTimeout > c.MLP.MaxSyncWaitTimeout {
			return fmt.Errorf("mlp sync wait timeouts must be positive, and sync_wait_timeout must not exceed max_sync_wait_timeout")
		}
		if c.MLP.ShutdownTimeout <= 0 {
			return fmt.Errorf("mlp.shutdown_timeout must be positive")
		}
	}
	if !serverEnabled && !c.MLP.Enabled {
		return fmt.Errorf("at least one of server.enabled or mlp.enabled must be true")
	}
	if c.MLPReporting.Enabled {
		if c.MLPReporting.StandardReportURL == "" && c.MLPReporting.EmergencyReportURL == "" {
			return fmt.Errorf("mlp_reporting.enabled requires at least one of standard_report_url or emergency_report_url")
		}
		if (c.MLPReporting.StandardReportURL == "") != (c.MLPReporting.StandardReportClientID == "") {
			return fmt.Errorf("mlp_reporting.standard_report_url and standard_report_client_id must be set together")
		}
		if (c.MLPReporting.EmergencyReportURL == "") != (c.MLPReporting.EmergencyReportClientID == "") {
			return fmt.Errorf("mlp_reporting.emergency_report_url and emergency_report_client_id must be set together")
		}
		if c.MLPReporting.Timeout <= 0 {
			return fmt.Errorf("mlp_reporting.timeout must be positive")
		}
	}
	if c.Database.Path == "" {
		return fmt.Errorf("database.path is required")
	}
	if c.Database.CheckpointPages <= 0 {
		return fmt.Errorf("database.checkpoint_pages must be positive")
	}
	if c.Retention.Request <= 0 || c.Retention.Result <= 0 {
		return fmt.Errorf("retention values must be positive")
	}
	if len(c.Clients) == 0 {
		return fmt.Errorf("at least one explicitly configured client is required")
	}
	d := c.Diameter
	if d.OriginHost == "" || d.OriginRealm == "" || d.HostIPAddress == "" || len(d.Peers) == 0 {
		return fmt.Errorf("diameter origin identity and at least one peer are required")
	}
	if d.ConnectionTimeout <= 0 || d.RequestTimeout <= 0 || d.ReconnectMin <= 0 || d.ReconnectMax < d.ReconnectMin || d.WatchdogInterval <= 0 || d.WatchdogTimeout <= 0 || d.ShutdownTimeout <= 0 {
		return fmt.Errorf("invalid diameter timeouts/backoff")
	}
	peers := map[string]bool{}
	names := map[string]bool{}
	for i, p := range d.Peers {
		if p.Name == "" || p.Address == "" || (p.Transport != "tcp" && p.Transport != "sctp") {
			return fmt.Errorf("diameter.peers[%d]: name, address and transport (tcp or sctp) are required", i)
		}
		k := strings.ToLower(p.Transport) + "\x00" + strings.ToLower(p.Address)
		if peers[k] {
			return fmt.Errorf("duplicate diameter peer %s/%s", p.Transport, p.Address)
		}
		if names[strings.ToLower(p.Name)] {
			return fmt.Errorf("duplicate diameter peer name %q", p.Name)
		}
		peers[k] = true
		names[strings.ToLower(p.Name)] = true
	}
	seen := map[string]bool{}
	for _, v := range c.Clients {
		if v.ID == "" || v.BearerToken == "" || seen[v.ID] {
			return fmt.Errorf("each client needs unique id and bearer_token")
		}
		seen[v.ID] = true
		if len(v.Services) == 0 || len(v.TargetPrefixes) == 0 {
			return fmt.Errorf("client %q needs services and target_prefixes", v.ID)
		}
		for _, p := range v.TargetPrefixes {
			if strings.Trim(p, "0123456789") != "" {
				return fmt.Errorf("client %q prefix is not numeric", v.ID)
			}
		}
		if _, ok := clientTypeValues[v.LCSClientType]; !ok {
			return fmt.Errorf("client %q has invalid lcs_client_type %q", v.ID, v.LCSClientType)
		}
		if _, ok := privacyCheckValues[v.LCSPrivacyCheck]; !ok {
			return fmt.Errorf("client %q has invalid lcs_privacy_check %q", v.ID, v.LCSPrivacyCheck)
		}
	}
	if c.Delivery.Enabled {
		if _, err := c.Delivery.EncryptionKeyBytes(); err != nil {
			return fmt.Errorf("delivery.encryption_key: %w", err)
		}
	}
	return nil
}

func rejectObsoleteDiameterKeys(b []byte) error {
	var root yaml.Node
	if err := yaml.Unmarshal(b, &root); err != nil {
		return err
	}
	if len(root.Content) == 0 || root.Content[0].Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i < len(root.Content[0].Content); i += 2 {
		if root.Content[0].Content[i].Value != "diameter" {
			continue
		}
		d := root.Content[0].Content[i+1]
		if d.Kind != yaml.MappingNode {
			return nil
		}
		forbidden := map[string]bool{"enabled": true, "mode": true, "dra": true, "direct": true, "product_name": true, "vendor_id": true, "peer_mode": true, "peer_address": true, "peer_port": true, "transport": true, "destination_host": true, "destination_realm": true, "hss": true, "mme": true, "mmes": true, "slh": true, "slg": true}
		for j := 0; j < len(d.Content); j += 2 {
			if forbidden[d.Content[j].Value] {
				return fmt.Errorf("diameter.%s is obsolete and rejected; configure diameter.peers", d.Content[j].Value)
			}
		}
	}
	return nil
}
