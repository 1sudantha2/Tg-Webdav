// Package db implements the virtual file system metadata index on top of a
// pure-Go SQLite database (modernc.org/sqlite, no CGO required).
//
// The tree is stored in a single `nodes` table. Directory rows have is_dir=1;
// file rows additionally carry the size, MIME type, Telegram message id and
// the TL-encoded tg.Document blob needed to stream the bytes back.
package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// ErrNotFound is returned when a path does not exist in the index.
var ErrNotFound = errors.New("db: path not found")

// Node is one entry of the virtual file system.
type Node struct {
	ID       int64
	ParentID int64
	Name     string
	IsDir    bool
	Size     int64
	MTime    time.Time
	MIME     string
	MsgID    int  // Telegram message id inside the storage channel (files only).
	DocID    int64
	Doc      []byte // TL-encoded tg.Document (files only).
}

// Path returns the absolute virtual path of the node given its ancestors.
// It is mostly useful for logging.
func (n *Node) PathOf(parentPath string) string {
	if parentPath == "/" {
		return "/" + n.Name
	}
	return parentPath + "/" + n.Name
}

// DB is the metadata store.
type DB struct {
	conn *sql.DB
}

// Open opens (creating if necessary) the SQLite database at path.
func Open(dbPath string) (*DB, error) {
	if d := filepath.Dir(strings.TrimSpace(dbPath)); d != "." && d != "" {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, fmt.Errorf("create db dir: %w", err)
		}
	}
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)&_pragma=synchronous(NORMAL)", dbPath)
	conn, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// A single connection keeps SQLite happy and all our queries are tiny.
	conn.SetMaxOpenConns(1)

	if _, err := conn.Exec(schema); err != nil {
		conn.Close()
		return nil, fmt.Errorf("init schema: %w", err)
	}
	return &DB{conn: conn}, nil
}

const schema = `
CREATE TABLE IF NOT EXISTS nodes (
	id        INTEGER PRIMARY KEY AUTOINCREMENT,
	parent_id INTEGER NOT NULL DEFAULT 0,
	name      TEXT    NOT NULL,
	is_dir    INTEGER NOT NULL DEFAULT 0,
	size      INTEGER NOT NULL DEFAULT 0,
	mtime     INTEGER NOT NULL DEFAULT 0,
	mime      TEXT    NOT NULL DEFAULT '',
	msg_id    INTEGER,
	doc_id    INTEGER,
	doc       BLOB,
	UNIQUE (parent_id, name)
);
CREATE INDEX IF NOT EXISTS idx_nodes_parent ON nodes(parent_id);
CREATE INDEX IF NOT EXISTS idx_nodes_doc    ON nodes(doc_id);
CREATE INDEX IF NOT EXISTS idx_nodes_msg    ON nodes(msg_id);
`

// Close closes the underlying database.
func (d *DB) Close() error { return d.conn.Close() }

// ---------------------------------------------------------------------------
// Lookup / listing
// ---------------------------------------------------------------------------

// Lookup resolves an absolute virtual path ("/a/b/c") to its node. The root
// "/" is represented by the zero node (ID 0, IsDir).
func (d *DB) Lookup(ctx context.Context, p string) (*Node, error) {
	p = path.Clean("/" + p)
	if p == "/" {
		return &Node{ID: 0, Name: "/", IsDir: true, MTime: time.Unix(0, 0)}, nil
	}
	parentID := int64(0)
	var node *Node
	for _, seg := range strings.Split(strings.TrimPrefix(p, "/"), "/") {
		n, err := d.Child(ctx, parentID, seg)
		if err != nil {
			return nil, err
		}
		node, parentID = n, n.ID
	}
	return node, nil
}

// Child returns the child of parent with the given name.
func (d *DB) Child(ctx context.Context, parentID int64, name string) (*Node, error) {
	row := d.conn.QueryRowContext(ctx,
		`SELECT id, parent_id, name, is_dir, size, mtime, mime, msg_id, doc_id, doc
		 FROM nodes WHERE parent_id = ? AND name = ?`, parentID, name)
	n, err := scanNode(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return n, err
}

// Children lists the direct children of a directory, folders first then
// files, both alphabetically.
func (d *DB) Children(ctx context.Context, parentID int64) ([]*Node, error) {
	rows, err := d.conn.QueryContext(ctx,
		`SELECT id, parent_id, name, is_dir, size, mtime, mime, msg_id, doc_id, doc
		 FROM nodes WHERE parent_id = ? ORDER BY is_dir DESC, name COLLATE NOCASE`, parentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*Node, 0, 16)
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

type scanner interface {
	Scan(dest ...any) error
}

func scanNode(s scanner) (*Node, error) {
	var (
		n            Node
		isDir        int
		mtime        int64
		msgID, docID sql.NullInt64
	)
	err := s.Scan(&n.ID, &n.ParentID, &n.Name, &isDir, &n.Size, &mtime, &n.MIME, &msgID, &docID, &n.Doc)
	if err != nil {
		return nil, err
	}
	n.IsDir = isDir == 1
	n.MTime = time.Unix(mtime, 0).UTC()
	n.MsgID = int(msgID.Int64)
	n.DocID = docID.Int64
	return &n, nil
}

// ---------------------------------------------------------------------------
// Creation
// ---------------------------------------------------------------------------

// Mkdir creates a single directory. The parent must exist.
func (d *DB) Mkdir(ctx context.Context, parentID int64, name string) (*Node, error) {
	now := time.Now().Unix()
	res, err := d.conn.ExecContext(ctx,
		`INSERT INTO nodes (parent_id, name, is_dir, mtime) VALUES (?, ?, 1, ?)`,
		parentID, name, now)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return &Node{ID: id, ParentID: parentID, Name: name, IsDir: true, MTime: time.Unix(now, 0).UTC()}, nil
}

// MkdirAll creates the directory (and any missing parents) for the absolute
// path, e.g. "/movies/2026".
func (d *DB) MkdirAll(ctx context.Context, p string) (*Node, error) {
	p = path.Clean("/" + p)
	if p == "/" {
		return d.Lookup(ctx, "/")
	}
	parentID := int64(0)
	var node *Node
	for _, seg := range strings.Split(strings.TrimPrefix(p, "/"), "/") {
		n, err := d.Child(ctx, parentID, seg)
		if errors.Is(err, ErrNotFound) {
			n, err = d.Mkdir(ctx, parentID, seg)
		}
		if err != nil {
			return nil, err
		}
		if !n.IsDir {
			return nil, fmt.Errorf("db: %q is a file, cannot be used as directory", seg)
		}
		node, parentID = n, n.ID
	}
	return node, nil
}

// UniqueName returns name (or "name (1).ext", "name (2).ext", ...) so that it
// does not collide with an existing child of parentID.
func (d *DB) UniqueName(ctx context.Context, parentID int64, name string) (string, error) {
	_, err := d.Child(ctx, parentID, name)
	if errors.Is(err, ErrNotFound) {
		return name, nil
	} else if err != nil {
		return "", err
	}
	ext := path.Ext(name)
	base := strings.TrimSuffix(name, ext)
	for i := 1; i < 10000; i++ {
		candidate := fmt.Sprintf("%s (%d)%s", base, i, ext)
		_, err := d.Child(ctx, parentID, candidate)
		if errors.Is(err, ErrNotFound) {
			return candidate, nil
		} else if err != nil {
			return "", err
		}
	}
	return "", fmt.Errorf("db: too many duplicates for %q", name)
}

// CreateFile inserts a file node.
func (d *DB) CreateFile(ctx context.Context, parentID int64, name string, size int64, mime string, msgID int, docID int64, doc []byte) (*Node, error) {
	now := time.Now().Unix()
	res, err := d.conn.ExecContext(ctx,
		`INSERT INTO nodes (parent_id, name, is_dir, size, mtime, mime, msg_id, doc_id, doc)
		 VALUES (?, ?, 0, ?, ?, ?, ?, ?, ?)`,
		parentID, name, size, now, mime, msgID, docID, doc)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return &Node{
		ID: id, ParentID: parentID, Name: name, Size: size,
		MTime: time.Unix(now, 0).UTC(), MIME: mime, MsgID: msgID, DocID: docID, Doc: doc,
	}, nil
}

// ---------------------------------------------------------------------------
// Mutation
// ---------------------------------------------------------------------------

// Rename moves the node to a new parent under a new name. The destination
// must not exist.
func (d *DB) Rename(ctx context.Context, id, newParentID int64, newName string) error {
	_, err := d.conn.ExecContext(ctx,
		`UPDATE nodes SET parent_id = ?, name = ?, mtime = ? WHERE id = ?`,
		newParentID, newName, time.Now().Unix(), id)
	return err
}

// Touch updates the modification time.
func (d *DB) Touch(ctx context.Context, id int64, size int64) error {
	_, err := d.conn.ExecContext(ctx, `UPDATE nodes SET mtime = ?, size = ? WHERE id = ?`,
		time.Now().Unix(), size, id)
	return err
}

// UpdateDoc replaces the stored document blob (used to refresh expired
// Telegram file references).
func (d *DB) UpdateDoc(ctx context.Context, docID int64, doc []byte) error {
	_, err := d.conn.ExecContext(ctx, `UPDATE nodes SET doc = ? WHERE doc_id = ?`, doc, docID)
	return err
}

// CountByDocID reports how many file nodes reference the given Telegram
// document. Copies created with WebDAV COPY share one document.
func (d *DB) CountByDocID(ctx context.Context, docID int64) (int, error) {
	var n int
	err := d.conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM nodes WHERE doc_id = ?`, docID).Scan(&n)
	return n, err
}

// DeleteRecursive removes the subtree rooted at node id and returns the
// deleted file nodes (so the caller can delete the Telegram messages whose
// reference count dropped to zero).
func (d *DB) DeleteRecursive(ctx context.Context, id int64) ([]*Node, error) {
	var deleted []*Node
	queue := []int64{id}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		n, err := d.nodeByID(ctx, cur)
		if err != nil {
			return nil, err
		}
		if n.IsDir {
			kids, err := d.Children(ctx, cur)
			if err != nil {
				return nil, err
			}
			for _, k := range kids {
				queue = append(queue, k.ID)
			}
		} else {
			deleted = append(deleted, n)
		}
		if _, err := d.conn.ExecContext(ctx, `DELETE FROM nodes WHERE id = ?`, cur); err != nil {
			return nil, err
		}
	}
	return deleted, nil
}

// CopyRecursive duplicates the subtree rooted at node id into newParentID
// under newName. File copies share the underlying Telegram document (no data
// is moved).
func (d *DB) CopyRecursive(ctx context.Context, id, newParentID int64, newName string) error {
	n, err := d.nodeByID(ctx, id)
	if err != nil {
		return err
	}
	if !n.IsDir {
		_, err := d.CreateFile(ctx, newParentID, newName, n.Size, n.MIME, n.MsgID, n.DocID, n.Doc)
		return err
	}
	dir, err := d.Mkdir(ctx, newParentID, newName)
	if err != nil {
		return err
	}
	kids, err := d.Children(ctx, id)
	if err != nil {
		return err
	}
	for _, k := range kids {
		if err := d.CopyRecursive(ctx, k.ID, dir.ID, k.Name); err != nil {
			return err
		}
	}
	return nil
}

func (d *DB) nodeByID(ctx context.Context, id int64) (*Node, error) {
	row := d.conn.QueryRowContext(ctx,
		`SELECT id, parent_id, name, is_dir, size, mtime, mime, msg_id, doc_id, doc FROM nodes WHERE id = ?`, id)
	n, err := scanNode(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return n, err
}

// LookupByID returns the node with the given primary key.
func (d *DB) LookupByID(ctx context.Context, id int64) (*Node, error) {
	return d.nodeByID(ctx, id)
}

// NodeByDocID finds the file node referencing the given Telegram document id.
func (d *DB) NodeByDocID(ctx context.Context, docID int64) (*Node, error) {
	row := d.conn.QueryRowContext(ctx,
		`SELECT id, parent_id, name, is_dir, size, mtime, mime, msg_id, doc_id, doc FROM nodes WHERE doc_id = ? LIMIT 1`, docID)
	n, err := scanNode(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return n, err
}
