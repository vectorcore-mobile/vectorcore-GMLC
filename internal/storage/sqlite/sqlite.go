package sqlite

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	sqlite3 "github.com/mattn/go-sqlite3"
	"github.com/vectorcore/gmlc/internal/domain"
	"github.com/vectorcore/gmlc/internal/storage"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

type Config struct {
	Path            string
	BusyTimeout     time.Duration
	Synchronous     string
	CheckpointPages int
}
type Store struct {
	db              *sql.DB
	checkpointPages int
}

func Open(ctx context.Context, cfg Config) (*Store, error) {
	if cfg.Path == "" || cfg.Path == ":memory:" {
		return nil, fmt.Errorf("sqlite: a local database path is required")
	}
	if err := rejectNFS(filepath.Dir(cfg.Path)); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(cfg.Path), 0750); err != nil {
		return nil, err
	}
	if cfg.BusyTimeout <= 0 {
		cfg.BusyTimeout = 5 * time.Second
	}
	if cfg.CheckpointPages <= 0 {
		cfg.CheckpointPages = 1000
	}
	sync := strings.ToUpper(cfg.Synchronous)
	if sync == "" {
		sync = "FULL"
	}
	if sync != "FULL" && sync != "NORMAL" && sync != "OFF" {
		return nil, fmt.Errorf("sqlite: invalid synchronous mode")
	}
	dsn := fmt.Sprintf("file:%s?_foreign_keys=on&_busy_timeout=%d&_journal_mode=WAL&_synchronous=%s", cfg.Path, cfg.BusyTimeout.Milliseconds(), sync)
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err = db.PingContext(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db, checkpointPages: cfg.CheckpointPages}, nil
}
func rejectNFS(dir string) error {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if st.Type == 0x6969 {
		return fmt.Errorf("sqlite: database directory is on NFS")
	}
	return nil
}
func utc(t time.Time) string                { return t.UTC().Format(time.RFC3339Nano) }
func parseTime(s string) (time.Time, error) { return time.Parse(time.RFC3339Nano, s) }

func (s *Store) Migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, "CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL)"); err != nil {
		return err
	}
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, e := range entries {
		b, err := migrationFS.ReadFile("migrations/" + e.Name())
		if err != nil {
			return err
		}
		v, err := strconv.Atoi(strings.SplitN(e.Name(), "_", 2)[0])
		if err != nil {
			return err
		}
		var exists int
		err = s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM schema_migrations WHERE version=?", v).Scan(&exists)
		if err != nil {
			return err
		}
		if exists > 0 {
			continue
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, string(b)); err == nil {
			_, err = tx.ExecContext(ctx, "INSERT INTO schema_migrations(version, applied_at) VALUES(?,?)", v, utc(time.Now()))
		}
		if err != nil {
			tx.Rollback()
			return err
		}
		if err = tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}
func (s *Store) UpsertClient(ctx context.Context, c storage.Client) error {
	now := utc(time.Now())
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, "INSERT INTO lcs_clients(id,credential_hash,enabled,lcs_client_type,lcs_privacy_check,created_at,updated_at) VALUES(?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET credential_hash=excluded.credential_hash,enabled=excluded.enabled,lcs_client_type=excluded.lcs_client_type,lcs_privacy_check=excluded.lcs_privacy_check,updated_at=excluded.updated_at", c.ID, c.CredentialHash, boolInt(c.Enabled), c.LCSClientType, c.LCSPrivacyCheck, now, now)
	if err != nil {
		return err
	}
	for _, q := range []string{"DELETE FROM client_services WHERE client_id=?", "DELETE FROM client_target_prefixes WHERE client_id=?"} {
		if _, err = tx.ExecContext(ctx, q, c.ID); err != nil {
			return err
		}
	}
	for _, v := range c.Services {
		if _, err = tx.ExecContext(ctx, "INSERT INTO client_services(client_id,service) VALUES(?,?)", c.ID, v); err != nil {
			return err
		}
	}
	for _, v := range c.TargetPrefixes {
		if _, err = tx.ExecContext(ctx, "INSERT INTO client_target_prefixes(client_id,prefix) VALUES(?,?)", c.ID, v); err != nil {
			return err
		}
	}
	return tx.Commit()
}
func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

// qosColumns splits a domain.QoSRequest into the five location_requests
// columns; a nil q yields all-nil/zero args, so an INSERT with no QoS
// requested stores NULLs exactly like today's behavior before this field
// existed.
func qosColumns(q *domain.QoSRequest) (class *uint32, hAcc, vAcc *float64, vertReq int, respTime *uint32) {
	if q == nil {
		return nil, nil, nil, 0, nil
	}
	return q.Class, q.HorizontalAccuracyMeters, q.VerticalAccuracyMeters, boolInt(q.VerticalRequested), q.ResponseTimeClass
}

// scanQoS is the inverse of qosColumns: nil unless at least one column was
// actually set, mirroring qosGroup's own "don't send an empty group" rule
// on the decode side.
func scanQoS(class, respTime sql.NullInt64, hAcc, vAcc sql.NullFloat64, vertReq int) *domain.QoSRequest {
	if !class.Valid && !hAcc.Valid && !vAcc.Valid && !respTime.Valid && vertReq == 0 {
		return nil
	}
	q := &domain.QoSRequest{VerticalRequested: vertReq == 1}
	if class.Valid {
		c := uint32(class.Int64)
		q.Class = &c
	}
	if hAcc.Valid {
		q.HorizontalAccuracyMeters = &hAcc.Float64
	}
	if vAcc.Valid {
		q.VerticalAccuracyMeters = &vAcc.Float64
	}
	if respTime.Valid {
		r := uint32(respTime.Int64)
		q.ResponseTimeClass = &r
	}
	return q
}

// GetClientCredential is a single fixed-cost query (indexed primary-key
// lookup) regardless of whether id matches a row, and regardless of how
// many services/prefixes that client has — see storage.Store for why that
// matters on the pre-authentication path.
func (s *Store) GetClientCredential(ctx context.Context, id string) (storage.Client, error) {
	var c storage.Client
	var enabled int
	err := s.db.QueryRowContext(ctx, "SELECT id,credential_hash,enabled,lcs_client_type,lcs_privacy_check FROM lcs_clients WHERE id=?", id).Scan(&c.ID, &c.CredentialHash, &enabled, &c.LCSClientType, &c.LCSPrivacyCheck)
	if errors.Is(err, sql.ErrNoRows) {
		return c, storage.ErrNotFound
	}
	if err != nil {
		return c, err
	}
	c.Enabled = enabled == 1
	return c, nil
}
func (s *Store) GetClientAuthzData(ctx context.Context, id string) ([]domain.ServiceType, []string, error) {
	var services []domain.ServiceType
	rows, err := s.db.QueryContext(ctx, "SELECT service FROM client_services WHERE client_id=?", id)
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		var v string
		if err = rows.Scan(&v); err != nil {
			rows.Close()
			return nil, nil, err
		}
		services = append(services, domain.ServiceType(v))
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return nil, nil, err
	}
	rows.Close()
	var prefixes []string
	rows, err = s.db.QueryContext(ctx, "SELECT prefix FROM client_target_prefixes WHERE client_id=?", id)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var v string
		if err = rows.Scan(&v); err != nil {
			return nil, nil, err
		}
		prefixes = append(prefixes, v)
	}
	return services, prefixes, rows.Err()
}
func (s *Store) CreateRequest(ctx context.Context, r domain.Request) (domain.Request, bool, error) {
	now := time.Now().UTC()
	r.CreatedAt = now
	r.UpdatedAt = now
	qosClass, qosHAcc, qosVAcc, qosVertReq, qosRespTime := qosColumns(r.QoS)
	_, err := s.db.ExecContext(ctx, "INSERT INTO location_requests(id,client_id,idempotency_key,service,target_kind,target_value,state,failure_code,location_type,priority,qos_class,qos_horizontal_accuracy_meters,qos_vertical_accuracy_meters,qos_vertical_requested,qos_response_time,subscription_id,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)", r.ID, r.ClientID, r.IdempotencyKey, r.Service, r.TargetKind, r.TargetValue, r.State, r.FailureCode, r.LocationType, r.Priority, qosClass, qosHAcc, qosVAcc, qosVertReq, qosRespTime, r.SubscriptionID, utc(now), utc(now))
	if err == nil {
		return r, true, nil
	}
	if !isUniqueConstraintErr(err) {
		return r, false, err
	}
	existing, e := s.byClientKey(ctx, r.ClientID, r.IdempotencyKey)
	return existing, false, e
}
func isUniqueConstraintErr(err error) bool {
	var e sqlite3.Error
	return errors.As(err, &e) && e.Code == sqlite3.ErrConstraint
}
func (s *Store) byClientKey(ctx context.Context, c, k string) (domain.Request, error) {
	var r domain.Request
	var created, updated string
	var priority, qosClass, qosRespTime sql.NullInt64
	var qosHAcc, qosVAcc sql.NullFloat64
	var qosVertReq int
	var subscriptionID sql.NullString
	err := s.db.QueryRowContext(ctx, "SELECT id,client_id,idempotency_key,service,target_kind,target_value,state,failure_code,attempt_count,location_type,priority,qos_class,qos_horizontal_accuracy_meters,qos_vertical_accuracy_meters,qos_vertical_requested,qos_response_time,subscription_id,created_at,updated_at FROM location_requests WHERE client_id=? AND idempotency_key=?", c, k).Scan(&r.ID, &r.ClientID, &r.IdempotencyKey, &r.Service, &r.TargetKind, &r.TargetValue, &r.State, &r.FailureCode, &r.AttemptCount, &r.LocationType, &priority, &qosClass, &qosHAcc, &qosVAcc, &qosVertReq, &qosRespTime, &subscriptionID, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return r, storage.ErrNotFound
	}
	if err != nil {
		return r, err
	}
	if priority.Valid {
		p := uint32(priority.Int64)
		r.Priority = &p
	}
	r.QoS = scanQoS(qosClass, qosRespTime, qosHAcc, qosVAcc, qosVertReq)
	if subscriptionID.Valid {
		r.SubscriptionID = &subscriptionID.String
	}
	r.CreatedAt, _ = parseTime(created)
	r.UpdatedAt, _ = parseTime(updated)
	return r, nil
}
func (s *Store) GetRequest(ctx context.Context, id string) (domain.Request, error) {
	var r domain.Request
	var created, updated string
	var priority, qosClass, qosRespTime sql.NullInt64
	var qosHAcc, qosVAcc sql.NullFloat64
	var qosVertReq int
	var subscriptionID sql.NullString
	err := s.db.QueryRowContext(ctx, "SELECT id,client_id,idempotency_key,service,target_kind,target_value,state,failure_code,attempt_count,location_type,priority,qos_class,qos_horizontal_accuracy_meters,qos_vertical_accuracy_meters,qos_vertical_requested,qos_response_time,subscription_id,created_at,updated_at FROM location_requests WHERE id=?", id).Scan(&r.ID, &r.ClientID, &r.IdempotencyKey, &r.Service, &r.TargetKind, &r.TargetValue, &r.State, &r.FailureCode, &r.AttemptCount, &r.LocationType, &priority, &qosClass, &qosHAcc, &qosVAcc, &qosVertReq, &qosRespTime, &subscriptionID, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return r, storage.ErrNotFound
	}
	if err != nil {
		return r, err
	}
	if priority.Valid {
		p := uint32(priority.Int64)
		r.Priority = &p
	}
	r.QoS = scanQoS(qosClass, qosRespTime, qosHAcc, qosVAcc, qosVertReq)
	if subscriptionID.Valid {
		r.SubscriptionID = &subscriptionID.String
	}
	r.CreatedAt, _ = parseTime(created)
	r.UpdatedAt, _ = parseTime(updated)
	return r, nil
}
func (s *Store) SetRequestSubscription(ctx context.Context, requestID, subscriptionID string) error {
	_, err := s.db.ExecContext(ctx, "UPDATE location_requests SET subscription_id=? WHERE id=?", subscriptionID, requestID)
	return err
}
func (s *Store) TransitionRequest(ctx context.Context, id string, to domain.State, code string) (domain.Request, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.Request{}, err
	}
	defer tx.Rollback()
	r, err := getRequestTx(ctx, tx, id)
	if err != nil {
		return r, err
	}
	if !domain.CanTransition(r.State, to) {
		return r, storage.ErrConflict
	}
	now := time.Now().UTC()
	res, err := tx.ExecContext(ctx, "UPDATE location_requests SET state=?,failure_code=?,updated_at=? WHERE id=? AND state=?", to, code, utc(now), id, r.State)
	if err != nil {
		return r, err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return r, storage.ErrConflict
	}
	r.State = to
	r.FailureCode = code
	r.UpdatedAt = now
	if err = tx.Commit(); err != nil {
		return r, err
	}
	return r, nil
}
func (s *Store) ClaimNextQueued(ctx context.Context, now time.Time) (domain.Request, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.Request{}, false, err
	}
	defer tx.Rollback()
	var id string
	err = tx.QueryRowContext(ctx, "SELECT id FROM location_requests WHERE state=? AND (next_attempt_at IS NULL OR next_attempt_at<=?) ORDER BY created_at,id LIMIT 1", domain.StateQueued, utc(now)).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Request{}, false, nil
	}
	if err != nil {
		return domain.Request{}, false, err
	}
	res, err := tx.ExecContext(ctx, "UPDATE location_requests SET state=?,attempt_count=attempt_count+1,updated_at=? WHERE id=? AND state=?", domain.StateResolving, utc(now), id, domain.StateQueued)
	if err != nil {
		return domain.Request{}, false, err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return domain.Request{}, false, storage.ErrConflict
	}
	r, err := getRequestTx(ctx, tx, id)
	if err != nil {
		return r, false, err
	}
	r.State = domain.StateResolving
	if err = tx.Commit(); err != nil {
		return r, false, err
	}
	return r, true, nil
}
func (s *Store) SaveServingNodeAndLocate(ctx context.Context, id string, n domain.ServingNode) error {
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	res, e := tx.ExecContext(ctx, "UPDATE location_requests SET state=?,updated_at=? WHERE id=? AND state=?", domain.StateLocating, utc(time.Now()), id, domain.StateResolving)
	if e != nil {
		return e
	}
	x, _ := res.RowsAffected()
	if x != 1 {
		return storage.ErrConflict
	}
	_, e = tx.ExecContext(ctx, "INSERT INTO request_serving_nodes(request_id,node_type,mme_host,mme_realm,source,resolved_at) VALUES(?,?,?,?,?,?) ON CONFLICT(request_id) DO UPDATE SET node_type=excluded.node_type,mme_host=excluded.mme_host,mme_realm=excluded.mme_realm,source=excluded.source,resolved_at=excluded.resolved_at", id, n.Type, n.MMEHost, n.MMERealm, n.Source, utc(n.ResolvedAt))
	if e != nil {
		return e
	}
	return tx.Commit()
}
func (s *Store) Requeue(ctx context.Context, id string, from domain.State, next time.Time, code string) error {
	res, e := s.db.ExecContext(ctx, "UPDATE location_requests SET state=?,next_attempt_at=?,failure_code=?,updated_at=? WHERE id=? AND state=?", domain.StateQueued, utc(next), code, utc(time.Now()), id, from)
	if e != nil {
		return e
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return storage.ErrConflict
	}
	return nil
}
func (s *Store) FailRequest(ctx context.Context, id string, from domain.State, code, desc string) error {
	res, e := s.db.ExecContext(ctx, "UPDATE location_requests SET state=?,failure_code=?,failure_description=?,updated_at=? WHERE id=? AND state=?", domain.StateFailed, code, desc, utc(time.Now()), id, from)
	if e != nil {
		return e
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return storage.ErrConflict
	}
	return nil
}
func (s *Store) CompleteRequest(ctx context.Context, id string, v domain.Result) error {
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	res, e := tx.ExecContext(ctx, "UPDATE location_requests SET state=?,updated_at=? WHERE id=? AND state=?", domain.StateCompleted, utc(time.Now()), id, domain.StateLocating)
	if e != nil {
		return e
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return storage.ErrConflict
	}
	_, e = tx.ExecContext(ctx, "INSERT INTO location_results(request_id,raw_gad,shape,created_at,latitude,longitude,uncertainty_meters,semi_major_meters,semi_minor_meters,orientation_degrees,confidence_percent,age_of_location_estimate,accuracy_fulfilment,raw_velocity_estimate,eutran_positioning_data,ecgi,source) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(request_id) DO UPDATE SET raw_gad=excluded.raw_gad,shape=excluded.shape,created_at=excluded.created_at,latitude=excluded.latitude,longitude=excluded.longitude,uncertainty_meters=excluded.uncertainty_meters,semi_major_meters=excluded.semi_major_meters,semi_minor_meters=excluded.semi_minor_meters,orientation_degrees=excluded.orientation_degrees,confidence_percent=excluded.confidence_percent,age_of_location_estimate=excluded.age_of_location_estimate,accuracy_fulfilment=excluded.accuracy_fulfilment,raw_velocity_estimate=excluded.raw_velocity_estimate,eutran_positioning_data=excluded.eutran_positioning_data,ecgi=excluded.ecgi,source=excluded.source", id, v.RawGAD, v.Shape, utc(v.CreatedAt), v.Latitude, v.Longitude, v.UncertaintyMeters, v.SemiMajorMeters, v.SemiMinorMeters, v.OrientationDegrees, v.ConfidencePercent, v.AgeOfLocationEstimate, v.AccuracyFulfilment, v.RawVelocityEstimate, v.EUTRANPositioningData, v.ECGI, v.Source)
	if e != nil {
		return e
	}
	return tx.Commit()
}
func (s *Store) GetResult(ctx context.Context, id string) (domain.Result, error) {
	var v domain.Result
	var created string
	var lat, lon, uncertainty, semiMajor, semiMinor, orientation sql.NullFloat64
	var confidence, age, accuracy sql.NullInt64
	err := s.db.QueryRowContext(ctx, "SELECT request_id,raw_gad,shape,created_at,latitude,longitude,uncertainty_meters,semi_major_meters,semi_minor_meters,orientation_degrees,confidence_percent,age_of_location_estimate,accuracy_fulfilment,raw_velocity_estimate,eutran_positioning_data,ecgi,source FROM location_results WHERE request_id=?", id).Scan(&v.RequestID, &v.RawGAD, &v.Shape, &created, &lat, &lon, &uncertainty, &semiMajor, &semiMinor, &orientation, &confidence, &age, &accuracy, &v.RawVelocityEstimate, &v.EUTRANPositioningData, &v.ECGI, &v.Source)
	if errors.Is(err, sql.ErrNoRows) {
		return v, storage.ErrNotFound
	}
	if err != nil {
		return v, err
	}
	v.CreatedAt, _ = parseTime(created)
	if lat.Valid {
		v.Latitude = &lat.Float64
	}
	if lon.Valid {
		v.Longitude = &lon.Float64
	}
	if uncertainty.Valid {
		v.UncertaintyMeters = &uncertainty.Float64
	}
	if semiMajor.Valid {
		v.SemiMajorMeters = &semiMajor.Float64
	}
	if semiMinor.Valid {
		v.SemiMinorMeters = &semiMinor.Float64
	}
	if orientation.Valid {
		v.OrientationDegrees = &orientation.Float64
	}
	if confidence.Valid {
		c := uint32(confidence.Int64)
		v.ConfidencePercent = &c
	}
	if age.Valid {
		a := uint32(age.Int64)
		v.AgeOfLocationEstimate = &a
	}
	if accuracy.Valid {
		a := uint32(accuracy.Int64)
		v.AccuracyFulfilment = &a
	}
	return v, nil
}
func (s *Store) SaveServingNode(ctx context.Context, requestID string, n domain.ServingNode) error {
	_, err := s.db.ExecContext(ctx, "INSERT INTO request_serving_nodes(request_id,node_type,mme_host,mme_realm,source,resolved_at) VALUES(?,?,?,?,?,?) ON CONFLICT(request_id) DO UPDATE SET node_type=excluded.node_type,mme_host=excluded.mme_host,mme_realm=excluded.mme_realm,source=excluded.source,resolved_at=excluded.resolved_at", requestID, n.Type, n.MMEHost, n.MMERealm, n.Source, utc(n.ResolvedAt))
	return err
}
func getRequestTx(ctx context.Context, tx *sql.Tx, id string) (domain.Request, error) {
	var r domain.Request
	var c, u string
	var priority, qosClass, qosRespTime sql.NullInt64
	var qosHAcc, qosVAcc sql.NullFloat64
	var qosVertReq int
	var subscriptionID sql.NullString
	err := tx.QueryRowContext(ctx, "SELECT id,client_id,idempotency_key,service,target_kind,target_value,state,failure_code,attempt_count,location_type,priority,qos_class,qos_horizontal_accuracy_meters,qos_vertical_accuracy_meters,qos_vertical_requested,qos_response_time,subscription_id,created_at,updated_at FROM location_requests WHERE id=?", id).Scan(&r.ID, &r.ClientID, &r.IdempotencyKey, &r.Service, &r.TargetKind, &r.TargetValue, &r.State, &r.FailureCode, &r.AttemptCount, &r.LocationType, &priority, &qosClass, &qosHAcc, &qosVAcc, &qosVertReq, &qosRespTime, &subscriptionID, &c, &u)
	if errors.Is(err, sql.ErrNoRows) {
		return r, storage.ErrNotFound
	}
	if priority.Valid {
		p := uint32(priority.Int64)
		r.Priority = &p
	}
	r.QoS = scanQoS(qosClass, qosRespTime, qosHAcc, qosVAcc, qosVertReq)
	if subscriptionID.Valid {
		r.SubscriptionID = &subscriptionID.String
	}
	r.CreatedAt, _ = parseTime(c)
	r.UpdatedAt, _ = parseTime(u)
	return r, err
}
func (s *Store) RecordAudit(ctx context.Context, e storage.AuditEvent) error {
	if e.At.IsZero() {
		e.At = time.Now()
	}
	_, err := s.db.ExecContext(ctx, "INSERT INTO audit_events(request_id,client_id,type,detail,created_at) VALUES(?,?,?,?,?)", e.RequestID, e.ClientID, e.Type, e.Detail, utc(e.At))
	return err
}
func (s *Store) Recover(ctx context.Context, now time.Time) error {
	_, err := s.db.ExecContext(ctx, "UPDATE location_requests SET state=?, next_attempt_at=?, updated_at=? WHERE state IN (?,?)", domain.StateQueued, utc(now), utc(now), domain.StateResolving, domain.StateLocating)
	return err
}
func (s *Store) Purge(ctx context.Context, requestBefore, resultBefore time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "DELETE FROM location_results WHERE created_at < ?", utc(resultBefore)); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM location_requests WHERE created_at < ? AND state IN (?,?,?,?,?)", utc(requestBefore), domain.StateCompleted, domain.StateFailed, domain.StateCancelled, domain.StateExpired, domain.StateIndeterminate); err != nil {
		return err
	}
	// audit_events has no FK to location_requests (it must outlive a purged
	// request as an audit trail), so it needs its own explicit retention cut.
	if _, err = tx.ExecContext(ctx, "DELETE FROM audit_events WHERE created_at < ?", utc(requestBefore)); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) CreateSubscription(ctx context.Context, clientID, callbackURL string, callbackSecret []byte) (storage.Subscription, error) {
	sub := storage.Subscription{ID: uuid.NewString(), ClientID: clientID, CallbackURL: callbackURL, CallbackSecret: callbackSecret, CreatedAt: time.Now().UTC()}
	_, err := s.db.ExecContext(ctx, "INSERT INTO delivery_subscriptions(id,client_id,callback_url,callback_secret,created_at) VALUES(?,?,?,?,?)", sub.ID, sub.ClientID, sub.CallbackURL, sub.CallbackSecret, utc(sub.CreatedAt))
	return sub, err
}
func (s *Store) GetSubscription(ctx context.Context, id string) (storage.Subscription, error) {
	var sub storage.Subscription
	var created string
	err := s.db.QueryRowContext(ctx, "SELECT id,client_id,callback_url,callback_secret,created_at FROM delivery_subscriptions WHERE id=?", id).Scan(&sub.ID, &sub.ClientID, &sub.CallbackURL, &sub.CallbackSecret, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return sub, storage.ErrNotFound
	}
	if err != nil {
		return sub, err
	}
	sub.CreatedAt, _ = parseTime(created)
	return sub, nil
}
func (s *Store) GetDelivery(ctx context.Context, id string) (storage.Delivery, error) {
	var d storage.Delivery
	var created, updated string
	var nextAttempt sql.NullString
	var responseCode sql.NullInt64
	err := s.db.QueryRowContext(ctx, "SELECT id,subscription_id,payload,state,attempt_count,next_attempt_at,last_response_code,created_at,updated_at FROM deliveries WHERE id=?", id).Scan(&d.ID, &d.SubscriptionID, &d.Payload, &d.State, &d.AttemptCount, &nextAttempt, &responseCode, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return d, storage.ErrNotFound
	}
	if err != nil {
		return d, err
	}
	if nextAttempt.Valid {
		t, _ := parseTime(nextAttempt.String)
		d.NextAttemptAt = &t
	}
	if responseCode.Valid {
		c := int(responseCode.Int64)
		d.LastResponseCode = &c
	}
	d.CreatedAt, _ = parseTime(created)
	d.UpdatedAt, _ = parseTime(updated)
	return d, nil
}
func (s *Store) CreateDelivery(ctx context.Context, subscriptionID string, payload []byte) (storage.Delivery, error) {
	now := time.Now().UTC()
	d := storage.Delivery{ID: uuid.NewString(), SubscriptionID: subscriptionID, Payload: payload, State: storage.DeliveryPending, CreatedAt: now, UpdatedAt: now}
	_, err := s.db.ExecContext(ctx, "INSERT INTO deliveries(id,subscription_id,payload,state,attempt_count,created_at,updated_at) VALUES(?,?,?,?,?,?,?)", d.ID, d.SubscriptionID, d.Payload, d.State, 0, utc(now), utc(now))
	return d, err
}

// ClaimNextDelivery mirrors ClaimNextQueued exactly: claim-then-read inside
// one transaction, so a concurrent claim (or the same delivery already
// in-flight) can't be picked twice — RowsAffected!=1 after the UPDATE means
// someone else claimed it first, reported as storage.ErrConflict.
func (s *Store) ClaimNextDelivery(ctx context.Context, now time.Time) (storage.Delivery, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return storage.Delivery{}, false, err
	}
	defer tx.Rollback()
	var id string
	err = tx.QueryRowContext(ctx, "SELECT id FROM deliveries WHERE state=? AND (next_attempt_at IS NULL OR next_attempt_at<=?) ORDER BY created_at,id LIMIT 1", storage.DeliveryPending, utc(now)).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return storage.Delivery{}, false, nil
	}
	if err != nil {
		return storage.Delivery{}, false, err
	}
	res, err := tx.ExecContext(ctx, "UPDATE deliveries SET state=?,attempt_count=attempt_count+1,updated_at=? WHERE id=? AND state=?", storage.DeliveryInFlight, utc(now), id, storage.DeliveryPending)
	if err != nil {
		return storage.Delivery{}, false, err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return storage.Delivery{}, false, storage.ErrConflict
	}
	d, err := getDeliveryTx(ctx, tx, id)
	if err != nil {
		return d, false, err
	}
	d.State = storage.DeliveryInFlight
	if err = tx.Commit(); err != nil {
		return d, false, err
	}
	return d, true, nil
}
func getDeliveryTx(ctx context.Context, tx *sql.Tx, id string) (storage.Delivery, error) {
	var d storage.Delivery
	var created, updated string
	var nextAttempt sql.NullString
	var responseCode sql.NullInt64
	err := tx.QueryRowContext(ctx, "SELECT id,subscription_id,payload,state,attempt_count,next_attempt_at,last_response_code,created_at,updated_at FROM deliveries WHERE id=?", id).Scan(&d.ID, &d.SubscriptionID, &d.Payload, &d.State, &d.AttemptCount, &nextAttempt, &responseCode, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return d, storage.ErrNotFound
	}
	if err != nil {
		return d, err
	}
	if nextAttempt.Valid {
		t, _ := parseTime(nextAttempt.String)
		d.NextAttemptAt = &t
	}
	if responseCode.Valid {
		c := int(responseCode.Int64)
		d.LastResponseCode = &c
	}
	d.CreatedAt, _ = parseTime(created)
	d.UpdatedAt, _ = parseTime(updated)
	return d, nil
}

// RequeueDelivery mirrors Requeue: transitions an in-flight delivery back
// to DeliveryPending with a future next_attempt_at after a transient
// failure, so ClaimNextDelivery's WHERE state=DeliveryPending can pick it
// up again once due, but not before — and, critically, not concurrently
// with any other in-flight claim of the same row.
func (s *Store) RequeueDelivery(ctx context.Context, id string, next time.Time) error {
	res, err := s.db.ExecContext(ctx, "UPDATE deliveries SET state=?,next_attempt_at=?,updated_at=? WHERE id=? AND state=?", storage.DeliveryPending, utc(next), utc(time.Now()), id, storage.DeliveryInFlight)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return storage.ErrConflict
	}
	return nil
}
func (s *Store) FailDelivery(ctx context.Context, id string, responseCode int) error {
	res, err := s.db.ExecContext(ctx, "UPDATE deliveries SET state=?,last_response_code=?,updated_at=? WHERE id=? AND state=?", storage.DeliveryFailed, responseCode, utc(time.Now()), id, storage.DeliveryInFlight)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return storage.ErrConflict
	}
	return nil
}
func (s *Store) MarkDelivered(ctx context.Context, id string, responseCode int) error {
	res, err := s.db.ExecContext(ctx, "UPDATE deliveries SET state=?,last_response_code=?,updated_at=? WHERE id=? AND state=?", storage.DeliveryDelivered, responseCode, utc(time.Now()), id, storage.DeliveryInFlight)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return storage.ErrConflict
	}
	return nil
}
func (s *Store) CreateDeferredLocationSubscription(ctx context.Context, clientID, targetKind, targetValue string, ref byte) (storage.DeferredLocationSubscription, error) {
	now := time.Now().UTC()
	sub := storage.DeferredLocationSubscription{LCSReferenceNumber: ref, ClientID: clientID, TargetKind: targetKind, TargetValue: targetValue, State: storage.DeferredSubscriptionPending, CreatedAt: now, UpdatedAt: now}
	_, err := s.db.ExecContext(ctx, "INSERT INTO deferred_location_subscriptions(lcs_reference_number,client_id,target_kind,target_value,state,created_at,updated_at) VALUES(?,?,?,?,?,?,?)", ref, clientID, targetKind, targetValue, sub.State, utc(now), utc(now))
	return sub, err
}
func (s *Store) FindPendingDeferredLocationSubscription(ctx context.Context, ref byte) (storage.DeferredLocationSubscription, error) {
	var sub storage.DeferredLocationSubscription
	var created, updated string
	err := s.db.QueryRowContext(ctx, "SELECT lcs_reference_number,client_id,target_kind,target_value,state,created_at,updated_at FROM deferred_location_subscriptions WHERE lcs_reference_number=? AND state=?", ref, storage.DeferredSubscriptionPending).Scan(&sub.LCSReferenceNumber, &sub.ClientID, &sub.TargetKind, &sub.TargetValue, &sub.State, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return sub, storage.ErrNotFound
	}
	if err != nil {
		return sub, err
	}
	sub.CreatedAt, _ = parseTime(created)
	sub.UpdatedAt, _ = parseTime(updated)
	return sub, nil
}
func (s *Store) MarkDeferredLocationSubscriptionReported(ctx context.Context, ref byte) error {
	res, err := s.db.ExecContext(ctx, "UPDATE deferred_location_subscriptions SET state=?,updated_at=? WHERE lcs_reference_number=? AND state=?", storage.DeferredSubscriptionReported, utc(time.Now()), ref, storage.DeferredSubscriptionPending)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return storage.ErrConflict
	}
	return nil
}
func (s *Store) CreateLocationReport(ctx context.Context, r storage.LocationReport) (storage.LocationReport, error) {
	r.ID = uuid.NewString()
	if r.ReceivedAt.IsZero() {
		r.ReceivedAt = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx, "INSERT INTO location_reports(id,lcs_reference_number,location_event,target_kind,target_value,raw_gad,ecgi,accuracy_fulfilment,age_of_location_estimate,raw_velocity_estimate,eutran_positioning_data,received_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)",
		r.ID, r.LCSReferenceNumber, r.LocationEvent, r.TargetKind, r.TargetValue, r.RawGAD, r.ECGI, r.AccuracyFulfilment, r.AgeOfLocationEstimate, r.RawVelocityEstimate, r.EUTRANPositioningData, utc(r.ReceivedAt))
	return r, err
}
// maxHistoryPointsPerTarget caps location_history to the most recent N rows
// per target. 3GPP TS 23.271 leaves history retention as a national-
// regulation/operator decision, not something the protocol specifies (see
// e.g. §9.1.4.3's "last known location" being a single most-recent slot),
// so this is an operator-requested, hardcoded-for-now cap rather than a
// value derived from any spec.
const maxHistoryPointsPerTarget = 20

func (s *Store) RecordHistory(ctx context.Context, targetKind, targetValue string, v domain.Result) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "INSERT INTO location_history(id,target_kind,target_value,shape,latitude,longitude,uncertainty_meters,recorded_at) VALUES(?,?,?,?,?,?,?,?)",
		uuid.NewString(), targetKind, targetValue, v.Shape, v.Latitude, v.Longitude, v.UncertaintyMeters, utc(v.CreatedAt)); err != nil {
		return err
	}
	// Prune anything past the most recent maxHistoryPointsPerTarget rows for
	// this target, oldest first.
	if _, err = tx.ExecContext(ctx, "DELETE FROM location_history WHERE target_kind=? AND target_value=? AND id NOT IN (SELECT id FROM location_history WHERE target_kind=? AND target_value=? ORDER BY recorded_at DESC LIMIT ?)",
		targetKind, targetValue, targetKind, targetValue, maxHistoryPointsPerTarget); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) QueryHistory(ctx context.Context, targetKind, targetValue string, start, stop time.Time, minInterval time.Duration, limit int) ([]storage.HistoryPoint, error) {
	// A safety cap on rows fetched before thinning/limit are applied, well
	// above any realistic no_of_reports request — guards against an
	// unbounded scan for a long-lived target with no interval/limit given.
	const maxScan = 10000
	rows, err := s.db.QueryContext(ctx, "SELECT shape,latitude,longitude,uncertainty_meters,recorded_at FROM location_history WHERE target_kind=? AND target_value=? AND recorded_at>=? AND recorded_at<=? ORDER BY recorded_at ASC LIMIT ?",
		targetKind, targetValue, utc(start), utc(stop), maxScan)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var all []storage.HistoryPoint
	for rows.Next() {
		var p storage.HistoryPoint
		var lat, lon, uncertainty sql.NullFloat64
		var recorded string
		if err := rows.Scan(&p.Shape, &lat, &lon, &uncertainty, &recorded); err != nil {
			return nil, err
		}
		if lat.Valid {
			p.Latitude = &lat.Float64
		}
		if lon.Valid {
			p.Longitude = &lon.Float64
		}
		if uncertainty.Valid {
			p.UncertaintyMeters = &uncertainty.Float64
		}
		p.RecordedAt, _ = parseTime(recorded)
		all = append(all, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := all
	if minInterval > 0 {
		out = out[:0]
		var last time.Time
		for _, p := range all {
			if len(out) == 0 || p.RecordedAt.Sub(last) >= minInterval {
				out = append(out, p)
				last = p.RecordedAt
			}
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (s *Store) Close(ctx context.Context) error {
	_, _ = s.db.ExecContext(ctx, fmt.Sprintf("PRAGMA wal_checkpoint(TRUNCATE)"))
	return s.db.Close()
}
