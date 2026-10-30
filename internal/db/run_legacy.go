package db

import (
	"fmt"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// VerifyLegacyPreservedTerminalHead adopts independently verified legacy
// authority without changing the head, status, historical error, or push
// provenance. The caller must first verify the exact legacy ref and establish
// an exact upstream recovery anchor. A stale snapshot cannot authorize a write.
func (d *DB) VerifyLegacyPreservedTerminalHead(id string, status types.RunStatus, head string) (bool, error) {
	if !status.Terminal() || head == "" {
		return false, nil
	}
	ts := now()
	result, err := d.sql.Exec(`UPDATE runs SET terminal_head_verified_at = ?, updated_at = ? WHERE id = ? AND status = ? AND head_sha = ? AND terminal_head_verified_at IS NULL AND custody_returned_at IS NULL`, ts, ts, id, status, head)
	if err != nil {
		return false, fmt.Errorf("verify legacy terminal head: %w", err)
	}
	n, err := result.RowsAffected()
	return n == 1, err
}
