package storage

import (
	"crypto/aes"
	"crypto/cipher"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct {
	DB   *sql.DB
	aead cipher.AEAD
}

func Open(dir string, key []byte) (*Store, error) {
	if len(key) != 32 {
		return nil, errors.New("master key must be 32 bytes")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCMWithRandomNonce(block)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "bridge.sqlite"))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	var version int
	if err = db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version > 3 {
		db.Close()
		return nil, fmt.Errorf("unreadable or unsupported storage schema %d", version)
	}
	_, err = db.Exec(`PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000; BEGIN IMMEDIATE;
 CREATE TABLE IF NOT EXISTS objects(kind TEXT,id TEXT,revision INTEGER,data BLOB,PRIMARY KEY(kind,id));
 CREATE TABLE IF NOT EXISTS task_reads(signature TEXT PRIMARY KEY,hits INTEGER,seen INTEGER);
 CREATE INDEX IF NOT EXISTS task_reads_seen ON task_reads(seen);
 CREATE TABLE IF NOT EXISTS audit(seq INTEGER PRIMARY KEY,ts INTEGER,actor TEXT,action TEXT,id TEXT);
 CREATE TABLE IF NOT EXISTS events(seq INTEGER PRIMARY KEY,source TEXT,eventid TEXT,name TEXT,data BLOB,stamp TEXT,UNIQUE(source,eventid));
 CREATE TABLE IF NOT EXISTS deliveries(id TEXT PRIMARY KEY,flow TEXT,destination TEXT,data BLOB,status TEXT,attempt INTEGER DEFAULT 0,next INTEGER,error TEXT DEFAULT '');
 CREATE INDEX IF NOT EXISTS due_delivery ON deliveries(status,next);
 CREATE TABLE IF NOT EXISTS retired_revisions(kind TEXT,id TEXT,revision INTEGER,PRIMARY KEY(kind,id));
 CREATE TRIGGER IF NOT EXISTS remember_revision AFTER DELETE ON objects WHEN OLD.kind IN ('flow','binding') BEGIN
 INSERT OR REPLACE INTO retired_revisions VALUES(OLD.kind,OLD.id,OLD.revision); END;
 CREATE TABLE IF NOT EXISTS binding_versions(id TEXT,revision INTEGER,data BLOB,PRIMARY KEY(id,revision));
 INSERT OR IGNORE INTO binding_versions SELECT id,revision,data FROM objects WHERE kind='binding';
 CREATE TRIGGER IF NOT EXISTS archive_binding_insert AFTER INSERT ON objects WHEN NEW.kind='binding' BEGIN
 INSERT INTO binding_versions VALUES(NEW.id,NEW.revision,NEW.data); END;
 CREATE TRIGGER IF NOT EXISTS archive_binding_update AFTER UPDATE ON objects WHEN NEW.kind='binding' BEGIN
 INSERT INTO binding_versions VALUES(NEW.id,NEW.revision,NEW.data); END;
 CREATE TABLE IF NOT EXISTS trigger_runs(id TEXT PRIMARY KEY,instance TEXT,trigger_id TEXT,owner TEXT,status TEXT,input BLOB,output BLOB,created INTEGER);
 CREATE INDEX IF NOT EXISTS run_queue ON trigger_runs(status,created);
 CREATE INDEX IF NOT EXISTS run_history ON trigger_runs(instance,created);
 CREATE TABLE IF NOT EXISTS trigger_reviews(instance TEXT,candidate INTEGER,config INTEGER,samples INTEGER,result TEXT,PRIMARY KEY(instance,candidate,config,samples));
 CREATE TABLE IF NOT EXISTS trigger_clock(instance TEXT PRIMARY KEY, checked INTEGER);
 CREATE TABLE IF NOT EXISTS local_deliveries(id TEXT PRIMARY KEY REFERENCES deliveries(id),kind TEXT NOT NULL,result BLOB);
 PRAGMA user_version=3; COMMIT;
 `)
	if err != nil {
		db.Close()
		return nil, err
	}
	return &Store{DB: db, aead: aead}, nil
}
func (s *Store) Seal(kind, id string, v any) ([]byte, error) {
	raw, e := json.Marshal(v)
	if e != nil {
		return nil, e
	}
	return s.aead.Seal(nil, nil, raw, []byte(kind+"/"+id)), nil
}
func (s *Store) Decode(kind, id string, raw []byte, v any) error {
	data, e := s.aead.Open(nil, nil, raw, []byte(kind+"/"+id))
	if e != nil {
		return fmt.Errorf("cannot decrypt %s/%s", kind, id)
	}
	return json.Unmarshal(data, v)
}
func List[T any](s *Store, kind string) ([]T, error) {
	return listObjects[T](s, s.DB, kind)
}

func ListTx[T any](s *Store, tx *sql.Tx, kind string) ([]T, error) {
	return listObjects[T](s, tx, kind)
}

func listObjects[T any](s *Store, db interface {
	Query(string, ...any) (*sql.Rows, error)
}, kind string) ([]T, error) {
	rows, e := db.Query("SELECT id,data FROM objects WHERE kind=? ORDER BY id", kind)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []T{}
	for rows.Next() {
		var id string
		var raw []byte
		var v T
		if e = rows.Scan(&id, &raw); e != nil {
			return nil, e
		}
		if e = s.Decode(kind, id, raw, &v); e != nil {
			return nil, e
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
func (s *Store) Get(kind, id string, v any) (int, error) {
	var rev int
	var raw []byte
	e := s.DB.QueryRow("SELECT revision,data FROM objects WHERE kind=? AND id=?", kind, id).Scan(&rev, &raw)
	if e != nil {
		return 0, e
	}
	return rev, s.Decode(kind, id, raw, v)
}
func (s *Store) Put(actor string, kind, id string, v any, rev int) error {
	tx, e := s.DB.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if e = s.PutTx(tx, actor, kind, id, v, rev); e != nil {
		return e
	}
	return tx.Commit()
}
func (s *Store) PutTx(tx *sql.Tx, actor, kind, id string, v any, rev int) error {
	data, e := s.Seal(kind, id, v)
	if e != nil {
		return e
	}
	var res sql.Result
	if rev == 0 {
		res, e = tx.Exec("INSERT INTO objects VALUES(?,?,1+coalesce((SELECT revision FROM retired_revisions WHERE kind=? AND id=?),0),?) ON CONFLICT DO NOTHING", kind, id, kind, id, data)
	} else if v == nil {
		res, e = tx.Exec("DELETE FROM objects WHERE kind=? AND id=? AND revision=?", kind, id, rev)
	} else {
		res, e = tx.Exec("UPDATE objects SET data=?,revision=revision+1 WHERE kind=? AND id=? AND revision=?", data, kind, id, rev)
	}
	if e != nil {
		return e
	}
	n, e := res.RowsAffected()
	if e != nil || n != 1 {
		return errors.New("revision conflict; reload configuration")
	}
	if _, e = tx.Exec("INSERT INTO audit(ts,actor,action,id) VALUES(?,?,?,?)", time.Now().Unix(), actor, kind, id); e != nil {
		return e
	}
	return nil
}
