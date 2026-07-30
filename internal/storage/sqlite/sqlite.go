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

	_ "github.com/mattn/go-sqlite3"
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
	_, err = tx.ExecContext(ctx, "INSERT INTO lcs_clients(id,credential_hash,enabled,created_at,updated_at) VALUES(?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET credential_hash=excluded.credential_hash,enabled=excluded.enabled,updated_at=excluded.updated_at", c.ID, c.CredentialHash, boolInt(c.Enabled), now, now)
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
func (s *Store) GetClient(ctx context.Context, id string) (storage.Client, error) {
	var c storage.Client
	var enabled int
	err := s.db.QueryRowContext(ctx, "SELECT id,credential_hash,enabled FROM lcs_clients WHERE id=?", id).Scan(&c.ID, &c.CredentialHash, &enabled)
	if errors.Is(err, sql.ErrNoRows) {
		return c, storage.ErrNotFound
	}
	if err != nil {
		return c, err
	}
	c.Enabled = enabled == 1
	rows, err := s.db.QueryContext(ctx, "SELECT service FROM client_services WHERE client_id=?", id)
	if err != nil {
		return c, err
	}
	defer rows.Close()
	for rows.Next() {
		var v string
		if err = rows.Scan(&v); err != nil {
			return c, err
		}
		c.Services = append(c.Services, domain.ServiceType(v))
	}
	rows.Close()
	rows, err = s.db.QueryContext(ctx, "SELECT prefix FROM client_target_prefixes WHERE client_id=?", id)
	if err != nil {
		return c, err
	}
	defer rows.Close()
	for rows.Next() {
		var v string
		if err = rows.Scan(&v); err != nil {
			return c, err
		}
		c.TargetPrefixes = append(c.TargetPrefixes, v)
	}
	return c, rows.Err()
}
func (s *Store) CreateRequest(ctx context.Context, r domain.Request) (domain.Request, bool, error) {
	now := time.Now().UTC()
	r.CreatedAt = now
	r.UpdatedAt = now
	_, err := s.db.ExecContext(ctx, "INSERT INTO location_requests(id,client_id,idempotency_key,service,target_kind,target_value,state,failure_code,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?)", r.ID, r.ClientID, r.IdempotencyKey, r.Service, r.TargetKind, r.TargetValue, r.State, r.FailureCode, utc(now), utc(now))
	if err == nil {
		return r, true, nil
	}
	if !strings.Contains(err.Error(), "UNIQUE constraint failed: location_requests.client_id, location_requests.idempotency_key") {
		return r, false, err
	}
	existing, e := s.byClientKey(ctx, r.ClientID, r.IdempotencyKey)
	return existing, false, e
}
func (s *Store) byClientKey(ctx context.Context, c, k string) (domain.Request, error) {
	var r domain.Request
	var created, updated string
	err := s.db.QueryRowContext(ctx, "SELECT id,client_id,idempotency_key,service,target_kind,target_value,state,failure_code,attempt_count,created_at,updated_at FROM location_requests WHERE client_id=? AND idempotency_key=?", c, k).Scan(&r.ID, &r.ClientID, &r.IdempotencyKey, &r.Service, &r.TargetKind, &r.TargetValue, &r.State, &r.FailureCode, &r.AttemptCount, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return r, storage.ErrNotFound
	}
	if err != nil {
		return r, err
	}
	r.CreatedAt, _ = parseTime(created)
	r.UpdatedAt, _ = parseTime(updated)
	return r, nil
}
func (s *Store) GetRequest(ctx context.Context, id string) (domain.Request, error) {
	var r domain.Request
	var created, updated string
	err := s.db.QueryRowContext(ctx, "SELECT id,client_id,idempotency_key,service,target_kind,target_value,state,failure_code,attempt_count,created_at,updated_at FROM location_requests WHERE id=?", id).Scan(&r.ID, &r.ClientID, &r.IdempotencyKey, &r.Service, &r.TargetKind, &r.TargetValue, &r.State, &r.FailureCode, &r.AttemptCount, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return r, storage.ErrNotFound
	}
	if err != nil {
		return r, err
	}
	r.CreatedAt, _ = parseTime(created)
	r.UpdatedAt, _ = parseTime(updated)
	return r, nil
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
	_, e = tx.ExecContext(ctx, "INSERT INTO location_results(request_id,raw_gad,shape,created_at,latitude,longitude,ecgi,source) VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(request_id) DO UPDATE SET raw_gad=excluded.raw_gad,shape=excluded.shape,created_at=excluded.created_at,latitude=excluded.latitude,longitude=excluded.longitude,ecgi=excluded.ecgi,source=excluded.source", id, v.RawGAD, v.Shape, utc(v.CreatedAt), v.Latitude, v.Longitude, v.ECGI, v.Source)
	if e != nil {
		return e
	}
	return tx.Commit()
}
func (s *Store) GetResult(ctx context.Context, id string) (domain.Result, error) {
	var v domain.Result
	var created string
	var lat, lon sql.NullFloat64
	err := s.db.QueryRowContext(ctx, "SELECT request_id,raw_gad,shape,created_at,latitude,longitude,ecgi,source FROM location_results WHERE request_id=?", id).Scan(&v.RequestID, &v.RawGAD, &v.Shape, &created, &lat, &lon, &v.ECGI, &v.Source)
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
	return v, nil
}
func (s *Store) SaveServingNode(ctx context.Context, requestID string, n domain.ServingNode) error {
	_, err := s.db.ExecContext(ctx, "INSERT INTO request_serving_nodes(request_id,node_type,mme_host,mme_realm,source,resolved_at) VALUES(?,?,?,?,?,?) ON CONFLICT(request_id) DO UPDATE SET node_type=excluded.node_type,mme_host=excluded.mme_host,mme_realm=excluded.mme_realm,source=excluded.source,resolved_at=excluded.resolved_at", requestID, n.Type, n.MMEHost, n.MMERealm, n.Source, utc(n.ResolvedAt))
	return err
}
func getRequestTx(ctx context.Context, tx *sql.Tx, id string) (domain.Request, error) {
	var r domain.Request
	var c, u string
	err := tx.QueryRowContext(ctx, "SELECT id,client_id,idempotency_key,service,target_kind,target_value,state,failure_code,attempt_count,created_at,updated_at FROM location_requests WHERE id=?", id).Scan(&r.ID, &r.ClientID, &r.IdempotencyKey, &r.Service, &r.TargetKind, &r.TargetValue, &r.State, &r.FailureCode, &r.AttemptCount, &c, &u)
	if errors.Is(err, sql.ErrNoRows) {
		return r, storage.ErrNotFound
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
	return tx.Commit()
}
func (s *Store) Close(ctx context.Context) error {
	_, _ = s.db.ExecContext(ctx, fmt.Sprintf("PRAGMA wal_checkpoint(TRUNCATE)"))
	return s.db.Close()
}
