package main

import (
	"encoding/json"
	fmt "fmt"
	"time"

	"github.com/tidwall/buntdb"
)

// Multiple read-only transactions can be opened at the same time but
// there can only be one read/write transaction at a time.
// Attempting to open a read/write transactions while another one is
// in progress will result in blocking until the current read/write
// transaction is completed.
// Use sync.Mutex underneath. Will come up with something else later.
type Cache struct {
	db *buntdb.DB
}

// Item that is internally saved to the database.
// Don't expect the Uid to be generated on Insert.
type dbItem struct {
	Uid string      `json:"uid"`
	Obj interface{} `json:"obj"`
}

func makeKey(table, uid string) string {
	return fmt.Sprintf("%s-%s", table, uid)
}

func (c *Cache) Get(table, uid string, obj interface{}) (bool, error) {
	key := makeKey(table, uid)
	var missing bool

	return missing, c.db.View(func(tx *buntdb.Tx) error {
		val, err := tx.Get(key) //IgnoreExpiredValue
		if err != nil {
			// NOTE: Weird syntax by buntDB where it returns a pre-constructed
			// value rather than an error type.
			if err == buntdb.ErrNotFound {
				missing = true
			}
			return err
		}

		return json.Unmarshal([]byte(val), obj)
	})
}

// Set is just a wrapper around ExpireSet with a no-expiration Intent.
func (c *Cache) Set(table, uid string, obj interface{}) error {
	return c.ExpireSet(table, uid, obj, 0)
}

func (c *Cache) ExpireSet(table, uid string, obj interface{}, expires int) error {
	b, err := json.Marshal(obj)
	if err != nil {
		return err
	}

	opts := &buntdb.SetOptions{}
	if expires > 0 {
		opts.Expires = true
		opts.TTL = time.Duration(expires) * time.Second
	}

	return c.db.Update(func(tx *buntdb.Tx) error {
		indices, err := tx.Indexes()
		if err != nil {
			return err
		}

		var indexExists bool
		for _, ix := range indices {
			if table == ix {
				indexExists = true
				break
			}
		}

		if !indexExists {
			if err := tx.CreateIndex(table, makeKey(table, "*"), buntdb.IndexString); err != nil {
				return err
			}
		}

		tx.Set(makeKey(table, uid), string(b), opts)
		return nil
	})
}

type Cachier interface {
	Set(table, uid string, obj interface{}) error
	ExpireSet(table, uid string, obj interface{}, expires int) error
	Get(table, uid string, obj interface{}) (bool, error)
}

func newCache() (Cachier, error) {
	db, err := buntdb.Open(":memory:")
	return &Cache{db}, err
}
