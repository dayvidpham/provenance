package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/dayvidpham/provenance/pkg/ptypes"
	"github.com/google/uuid"
)

// AddComment adds a comment to a task authored by authorID.
// A UUIDv7 CommentID is assigned automatically.
func (db *DB) AddComment(id ptypes.TaskID, authorID ptypes.AgentID, body string) (ptypes.Comment, error) {
	now := time.Now().UTC()
	comment := ptypes.Comment{
		ID:        ptypes.CommentID{Namespace: id.Namespace, UUID: uuid.Must(uuid.NewV7())},
		TaskID:    id,
		AuthorID:  authorID,
		Body:      body,
		CreatedAt: now,
	}

	scope, err := db.bindScope(context.Background(), projectionTargetLive)
	if err != nil {
		return ptypes.Comment{}, fmt.Errorf("sqlite.AddComment on task %q: %w", id.String(), err)
	}
	defer scope.release()
	if _, err := scope.conn.ExecContext(scope.ctx, "INSERT INTO comments (id, task_id, author_id, body, created_at) VALUES (?1, ?2, ?3, ?4, ?5)",
		comment.ID.String(), comment.TaskID.String(), comment.AuthorID.String(), comment.Body, comment.CreatedAt.UnixNano()); err != nil {
		return ptypes.Comment{}, fmt.Errorf("sqlite.AddComment: failed to insert comment on task %q: %w — check that the task and author agent both exist", id.String(), err)
	}
	return comment, nil
}

// GetComment returns one comment by id. found is false when no such comment exists.
// Used by the journaled Session.AddComment read-back path.
func (db *DB) GetComment(id ptypes.CommentID) (ptypes.Comment, bool, error) {
	scope, err := db.bindScope(context.Background(), projectionTargetLive)
	if err != nil {
		return ptypes.Comment{}, false, fmt.Errorf("sqlite.GetComment %q: %w", id.String(), err)
	}
	defer scope.release()
	comment, err := ScanComment(scope.conn.QueryRowContext(scope.ctx, "SELECT id, task_id, author_id, body, created_at FROM comments WHERE id = ?1", id.String()))
	if errors.Is(err, sql.ErrNoRows) {
		return ptypes.Comment{}, false, nil
	}
	if err != nil {
		return ptypes.Comment{}, false, fmt.Errorf("sqlite.GetComment %q: %w", id.String(), err)
	}
	return comment, true, nil
}

// GetComments returns all comments on a task in chronological order.
func (db *DB) GetComments(id ptypes.TaskID) ([]ptypes.Comment, error) {
	scope, err := db.bindScope(context.Background(), projectionTargetLive)
	if err != nil {
		return nil, fmt.Errorf("sqlite.GetComments %q: %w", id.String(), err)
	}
	defer scope.release()
	rows, err := scope.conn.QueryContext(scope.ctx, "SELECT id, task_id, author_id, body, created_at FROM comments WHERE task_id = ?1 ORDER BY created_at ASC", id.String())
	if err != nil {
		return nil, fmt.Errorf("sqlite.GetComments: %w", err)
	}
	defer rows.Close()
	comments := make([]ptypes.Comment, 0)
	for rows.Next() {
		comment, err := ScanComment(rows)
		if err != nil {
			return nil, fmt.Errorf("sqlite.GetComments: scan comment row: %w", err)
		}
		comments = append(comments, comment)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite.GetComments: iterate comment rows: %w", err)
	}
	return comments, nil
}
