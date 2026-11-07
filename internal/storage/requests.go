package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// What a user can ask for on an item they requested. Ephemeral needs no
// approval; space-based is for admins only.
const (
	AskKeep   = "keep"
	AskDelete = "delete"
)

// What became of an ask.
const (
	AskApplied = "applied" // done straight away: auto-approve
	AskPending = "pending" // waiting for an admin
)

// IsRequester reports whether the user asked for this item in Seerr.
func (db *DB) IsRequester(ctx context.Context, ratingKey string, seerrID int64) (bool, error) {
	var n int
	err := db.QueryRowContext(ctx, `SELECT count(*) FROM requests WHERE rating_key = ? AND seerr_user_id = ?`, ratingKey, seerrID).Scan(&n)
	return n > 0, err
}

// Ask records what a user wants done with an item. A user with auto-approve
// gets it at once unless someone else's ask on the item is waiting: then an
// admin has to settle it, so it waits too.
func (db *DB) Ask(ctx context.Context, u User, ratingKey, kind string, dryRun bool) (string, error) {
	if kind != AskKeep && kind != AskDelete {
		return "", errors.New("can't ask for that")
	}
	var others int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM change_requests WHERE rating_key = ? AND state = 'pending' AND seerr_user_id <> ?`,
		ratingKey, u.SeerrID).Scan(&others); err != nil {
		return "", err
	}
	if u.AutoApprove && others == 0 {
		if err := db.applyAsk(ctx, ratingKey, kind, u.SeerrID, dryRun); err != nil {
			return "", err
		}
		return AskApplied, nil
	}
	_, err := db.ExecContext(ctx, `INSERT INTO change_requests (rating_key, seerr_user_id, kind, created_at) VALUES (?, ?, ?, ?)
		ON CONFLICT (rating_key, seerr_user_id) WHERE state = 'pending' DO UPDATE SET kind = excluded.kind, created_at = excluded.created_at`,
		ratingKey, u.SeerrID, kind, time.Now().Unix())
	return AskPending, err
}

// errNotScheduled says a delete couldn't be scheduled, usually because the item is set to keep.
var errNotScheduled = errors.New("it couldn't be scheduled: it may be set to keep, or already going")

func (db *DB) applyAsk(ctx context.Context, ratingKey, kind string, by int64, dryRun bool) error {
	if kind == AskKeep {
		return db.SetItemStatus(ctx, ratingKey, StatusKeep, by)
	}
	ok, err := db.ScheduleDeletion(ctx, ratingKey, by, dryRun, KindManual)
	if err == nil && !ok {
		err = errNotScheduled
	}
	return err
}

// ChangeRequest is an ask waiting for an admin.
type ChangeRequest struct {
	ID                    int64
	RatingKey, Title, Who string
	Kind                  string
	CreatedAt             int64
	Conflict              bool // someone else asked for something different on the same item
}

// PendingRequests lists asks waiting for an admin, oldest first.
func (db *DB) PendingRequests(ctx context.Context) ([]ChangeRequest, error) {
	rows, err := db.QueryContext(ctx, `SELECT c.id, c.rating_key, coalesce(m.title, c.rating_key), coalesce(u.display_name, ''), c.kind, c.created_at,
			EXISTS (SELECT 1 FROM change_requests o WHERE o.rating_key = c.rating_key AND o.state = 'pending' AND o.kind <> c.kind)
		FROM change_requests c LEFT JOIN media m ON m.rating_key = c.rating_key LEFT JOIN users u ON u.seerr_user_id = c.seerr_user_id
		WHERE c.state = 'pending' ORDER BY c.rating_key, c.created_at`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []ChangeRequest
	for rows.Next() {
		var r ChangeRequest
		if err := rows.Scan(&r.ID, &r.RatingKey, &r.Title, &r.Who, &r.Kind, &r.CreatedAt, &r.Conflict); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Decide approves or denies an ask. Approving one settles the item: any other
// waiting ask on it is denied, saying which won.
func (db *DB) Decide(ctx context.Context, id int64, approve bool, admin User, dryRun bool) (title, kind string, err error) {
	var key string
	var by int64
	err = db.QueryRowContext(ctx, `SELECT c.rating_key, coalesce(m.title, c.rating_key), c.kind, c.seerr_user_id FROM change_requests c
		LEFT JOIN media m ON m.rating_key = c.rating_key WHERE c.id = ? AND c.state = 'pending'`, id).Scan(&key, &title, &kind, &by)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", errors.New("that request was already decided")
	}
	if err != nil {
		return "", "", err
	}
	now := time.Now().Unix()
	if !approve {
		_, err = db.ExecContext(ctx, `UPDATE change_requests SET state = 'denied', decided_by = ?, decided_at = ? WHERE id = ?`, admin.Name, now, id)
		return title, kind, err
	}
	if err := db.applyAsk(ctx, key, kind, admin.SeerrID, dryRun); err != nil {
		return title, kind, err
	}
	err = db.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE change_requests SET state = 'approved', decided_by = ?, decided_at = ? WHERE id = ?`, admin.Name, now, id); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE change_requests SET state = 'denied', decided_by = ?, decided_at = ?, note = ?
			WHERE rating_key = ? AND state = 'pending' AND id <> ?`, admin.Name, now, fmt.Sprintf("settled: %s was approved", kind), key, id)
		return err
	})
	return title, kind, err
}

// SetAutoApprove changes whether a user's asks apply without an admin.
func (db *DB) SetAutoApprove(ctx context.Context, seerrID int64, on bool) (bool, error) {
	return changed(db.ExecContext(ctx, `UPDATE users SET auto_approve = ? WHERE seerr_user_id = ?`, on, seerrID))
}
