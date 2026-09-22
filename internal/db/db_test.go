package db

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func openTest(t *testing.T) *DB {
	t.Helper()
	d, err := Open(filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func TestMkdirAllAndLookup(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()

	dir, err := d.MkdirAll(ctx, "/movies/2026/scifi")
	if err != nil {
		t.Fatalf("mkdirall: %v", err)
	}
	if dir.Name != "scifi" || !dir.IsDir {
		t.Fatalf("unexpected node %+v", dir)
	}

	root, err := d.Lookup(ctx, "/")
	if err != nil || !root.IsDir || root.ID != 0 {
		t.Fatalf("root lookup: %v %+v", err, root)
	}

	n, err := d.Lookup(ctx, "/movies/2026")
	if err != nil || n.Name != "2026" {
		t.Fatalf("lookup: %v %+v", err, n)
	}

	if _, err := d.Lookup(ctx, "/nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestCreateFileAndChildren(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()

	dir, _ := d.MkdirAll(ctx, "/general")
	if _, err := d.CreateFile(ctx, dir.ID, "a.mp4", 100, "video/mp4", 7, 77, []byte{1, 2, 3}); err != nil {
		t.Fatalf("create: %v", err)
	}
	// duplicate name must fail
	if _, err := d.CreateFile(ctx, dir.ID, "a.mp4", 1, "", 0, 0, nil); err == nil {
		t.Fatal("duplicate insert should fail")
	}

	unique, err := d.UniqueName(ctx, dir.ID, "a.mp4")
	if err != nil || unique != "a (1).mp4" {
		t.Fatalf("unique name: %v %q", err, unique)
	}

	kids, err := d.Children(ctx, dir.ID)
	if err != nil || len(kids) != 1 {
		t.Fatalf("children: %v %d", err, len(kids))
	}
	if kids[0].Size != 100 || kids[0].MsgID != 7 || kids[0].DocID != 77 {
		t.Fatalf("unexpected child %+v", kids[0])
	}
}

func TestRenameAndCopy(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()

	a, _ := d.MkdirAll(ctx, "/a")
	b, _ := d.MkdirAll(ctx, "/b")
	file, _ := d.CreateFile(ctx, a.ID, "x.txt", 5, "text/plain", 1, 11, []byte{9})

	if err := d.Rename(ctx, file.ID, b.ID, "y.txt"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if _, err := d.Lookup(ctx, "/a/x.txt"); !errors.Is(err, ErrNotFound) {
		t.Fatal("old path should be gone")
	}
	moved, err := d.Lookup(ctx, "/b/y.txt")
	if err != nil {
		t.Fatalf("moved lookup: %v", err)
	}

	if err := d.CopyRecursive(ctx, moved.ID, a.ID, "copy.txt"); err != nil {
		t.Fatalf("copy: %v", err)
	}
	cp, err := d.Lookup(ctx, "/a/copy.txt")
	if err != nil {
		t.Fatalf("copy lookup: %v", err)
	}
	if cp.DocID != moved.DocID {
		t.Fatal("copy must share the telegram document")
	}

	// two references to the same document
	n, err := d.CountByDocID(ctx, 11)
	if err != nil || n != 2 {
		t.Fatalf("refcount: %v %d", err, n)
	}
}

func TestDeleteRecursive(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()

	dir, _ := d.MkdirAll(ctx, "/trash/deep")
	_, _ = d.CreateFile(ctx, dir.ID, "one.bin", 1, "", 1, 101, nil)
	deep, _ := d.Lookup(ctx, "/trash/deep")
	_, _ = d.CreateFile(ctx, deep.ID, "two.bin", 2, "", 2, 102, nil)

	trash, _ := d.Lookup(ctx, "/trash")
	deleted, err := d.DeleteRecursive(ctx, trash.ID)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if len(deleted) != 2 {
		t.Fatalf("want 2 deleted files, got %d", len(deleted))
	}
	if _, err := d.Lookup(ctx, "/trash"); !errors.Is(err, ErrNotFound) {
		t.Fatal("tree should be gone")
	}
}
