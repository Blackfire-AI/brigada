package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"

	"modernc.org/sqlite"
)

// backuper is what the driver's connection offers beyond database/sql:
// SQLite's online backup API, in both directions. NewBackup copies this
// connection's database into the file at dst; NewRestore copies the file
// at src into this connection's database.
type backuper interface {
	NewBackup(dstURI string) (*sqlite.Backup, error)
	NewRestore(srcURI string) (*sqlite.Backup, error)
}

// copyAll steps the backup to the end in one go, then commits it. Commit
// leaves the other side's connection open and hands it back, so it is
// closed here.
func copyAll(bk *sqlite.Backup) error {
	for more := true; more; {
		var err error
		if more, err = bk.Step(-1); err != nil {
			bk.Finish()
			return err
		}
	}
	other, err := bk.Commit()
	if other != nil {
		other.Close()
	}
	return err
}

// Backup writes a consistent copy of the store to the file at dst through
// SQLite's backup API — never a file copy of a live database, which could
// catch a write half done. dst must not exist; it is a complete database
// once Backup returns.
func (s *Store) Backup(ctx context.Context, dst string) error {
	if _, err := os.Stat(dst); err == nil {
		return fmt.Errorf("backup %s: the file exists", dst)
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("backup %s: %w", dst, err)
	}
	defer conn.Close()
	err = conn.Raw(func(driverConn any) error {
		b, ok := driverConn.(backuper)
		if !ok {
			return errors.New("the SQLite driver has no backup API")
		}
		bk, err := b.NewBackup(dst)
		if err != nil {
			return err
		}
		return copyAll(bk)
	})
	if err == nil {
		// The copy holds the whole store: owner-only, whatever the umask.
		err = os.Chmod(dst, 0o600)
	}
	if err != nil {
		os.Remove(dst)
		return fmt.Errorf("backup %s: %w", dst, err)
	}
	return nil
}

// Restore copies the snapshot file into a new store file at dst, through
// the backup API in reverse. dst must not exist: the caller moves a lost
// store aside first. The result is the snapshot as it was, not yet opened
// or migrated; Open does that.
func Restore(ctx context.Context, snapshot, dst string) error {
	if _, err := os.Stat(snapshot); err != nil {
		return fmt.Errorf("restore %s: %w", snapshot, err)
	}
	if _, err := os.Stat(dst); err == nil {
		return fmt.Errorf("restore into %s: the file exists", dst)
	}
	db, err := sql.Open("sqlite", dst)
	if err != nil {
		return fmt.Errorf("restore into %s: %w", dst, err)
	}
	defer db.Close()
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("restore into %s: %w", dst, err)
	}
	defer conn.Close()
	err = conn.Raw(func(driverConn any) error {
		b, ok := driverConn.(backuper)
		if !ok {
			return errors.New("the SQLite driver has no backup API")
		}
		bk, err := b.NewRestore(snapshot)
		if err != nil {
			return err
		}
		return copyAll(bk)
	})
	if err == nil {
		err = os.Chmod(dst, 0o600)
	}
	if err != nil {
		conn.Close()
		db.Close()
		os.Remove(dst)
		return fmt.Errorf("restore %s into %s: %w", snapshot, dst, err)
	}
	return nil
}
