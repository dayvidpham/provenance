package sqlite

import (
	"fmt"
	"time"

	"github.com/dayvidpham/provenance/pkg/ptypes"
	"github.com/google/uuid"
	zs "zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// AddComment adds a comment to a task authored by authorID.
// A UUIDv7 CommentID is assigned automatically. Acquires the DB mutex.
func (db *DB) AddComment(id ptypes.TaskID, authorID ptypes.AgentID, body string) (ptypes.Comment, error) {
	now := time.Now().UTC()
	comment := ptypes.Comment{
		ID:        ptypes.CommentID{Namespace: id.Namespace, UUID: uuid.Must(uuid.NewV7())},
		TaskID:    id,
		AuthorID:  authorID,
		Body:      body,
		CreatedAt: now,
	}

	db.mu.Lock()
	defer db.mu.Unlock()
	if err := sqlitex.Execute(db.conn, "INSERT INTO comments (id, task_id, author_id, body, created_at) VALUES (?1, ?2, ?3, ?4, ?5)", &sqlitex.ExecOptions{Args: []any{
		comment.ID.String(), comment.TaskID.String(),
		comment.AuthorID.String(), comment.Body, comment.CreatedAt.UnixNano(),
	}}); err != nil {
		return ptypes.Comment{}, fmt.Errorf(
			"sqlite.AddComment: failed to insert comment on task %q: %w — "+
				"check that the task and author agent both exist",
			id.String(), err,
		)
	}
	return comment, nil
}

// GetComment returns one comment by id. found is false when no such comment exists.
// Acquires the DB mutex. Used by the journaled Session.AddComment read-back path.
func (db *DB) GetComment(id ptypes.CommentID) (ptypes.Comment, bool, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	var (
		comment ptypes.Comment
		found   bool
	)
	if err := sqlitex.Execute(db.conn, "SELECT id, task_id, author_id, body, created_at FROM comments WHERE id = ?1", &sqlitex.ExecOptions{
		Args: []any{id.String()},
		ResultFunc: func(stmt *zs.Stmt) error {
			c, err := ScanComment(stmt)
			if err != nil {
				return err
			}
			comment = c
			found = true
			return nil
		},
	}); err != nil {
		return ptypes.Comment{}, false, fmt.Errorf("sqlite.GetComment %q: %w", id.String(), err)
	}
	return comment, found, nil
}

// GetComments returns all comments on a task in chronological order.
// Acquires the DB mutex.
func (db *DB) GetComments(id ptypes.TaskID) ([]ptypes.Comment, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	var comments []ptypes.Comment
	err := sqlitex.Execute(db.conn, "SELECT id, task_id, author_id, body, created_at\n\t\t FROM comments WHERE task_id = ?1 ORDER BY created_at ASC", &sqlitex.ExecOptions{
		Args: []any{id.String()},
		ResultFunc: func(stmt *zs.Stmt) error {
			c, err := ScanComment(stmt)
			if err != nil {
				return err
			}
			comments = append(comments, c)
			return nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf("sqlite.GetComments: %w", err)
	}
	return comments, nil
}
