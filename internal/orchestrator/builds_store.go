package orchestrator

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// cloneBuildRecord copies rec's layers, so a stored record and a caller's copy
// never share them.
func cloneBuildRecord(rec BuildRecord) BuildRecord {
	rec.Layers = append([]BuildLayer(nil), rec.Layers...)
	if rec.FinishedAt != nil {
		t := *rec.FinishedAt
		rec.FinishedAt = &t
	}
	return rec
}

func (s *MemoryStateStore) UpsertBuild(_ context.Context, rec BuildRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.builds == nil {
		s.builds = map[string]BuildRecord{}
	}
	s.builds[rec.ID] = cloneBuildRecord(rec)
	return nil
}

func (s *MemoryStateStore) GetBuild(_ context.Context, id string) (BuildRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rec, ok := s.builds[id]
	if !ok {
		return BuildRecord{}, fmt.Errorf("%w: %s", ErrBuildNotFound, id)
	}
	return cloneBuildRecord(rec), nil
}

func (s *MemoryStateStore) DeleteBuild(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.builds, id)
	return nil
}

func (s *PostgresStateStore) UpsertBuild(ctx context.Context, rec BuildRecord) error {
	raw, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("encode build %s: %w", rec.ID, err)
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO orchestrator_builds (build_id, tenant_id, state, record, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (build_id) DO UPDATE SET
			tenant_id=EXCLUDED.tenant_id,
			state=EXCLUDED.state,
			record=EXCLUDED.record,
			updated_at=EXCLUDED.updated_at
	`, rec.ID, rec.TenantID, rec.State, string(raw), rec.CreatedAt.UTC(), rec.UpdatedAt.UTC())
	if err != nil {
		return fmt.Errorf("upsert build %s: %w", rec.ID, err)
	}
	return nil
}

func (s *PostgresStateStore) GetBuild(ctx context.Context, id string) (BuildRecord, error) {
	var raw string
	err := s.db.QueryRowContext(ctx, `SELECT record FROM orchestrator_builds WHERE build_id = $1`, id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return BuildRecord{}, fmt.Errorf("%w: %s", ErrBuildNotFound, id)
	}
	if err != nil {
		return BuildRecord{}, fmt.Errorf("get build %s: %w", id, err)
	}
	var rec BuildRecord
	if err := json.Unmarshal([]byte(raw), &rec); err != nil {
		return BuildRecord{}, fmt.Errorf("decode build %s: %w", id, err)
	}
	return rec, nil
}

func (s *PostgresStateStore) DeleteBuild(ctx context.Context, id string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM orchestrator_builds WHERE build_id = $1`, id); err != nil {
		return fmt.Errorf("delete build %s: %w", id, err)
	}
	return nil
}

var (
	_ BuildStore = (*MemoryStateStore)(nil)
	_ BuildStore = (*PostgresStateStore)(nil)
)
