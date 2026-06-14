package store

import (
	"fmt"

	"github.com/dgraph-io/badger/v4"
	"github.com/zeebo/blake3"
)

type Store struct {
	db *badger.DB
}

func Open(dir string) (*Store, error) {
	opts := badger.DefaultOptions(dir).WithLogger(nil)
	db, err := badger.Open(opts)
	if err != nil {
		return nil, err
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func HashBytes(data []byte) string {
	h := blake3.Sum256(data)
	return fmt.Sprintf("%x", h)
}

func (s *Store) Put(data []byte) (string, error) {
	hash := HashBytes(data)
	key := []byte(hash)
	err := s.db.Update(func(txn *badger.Txn) error {
		_, err := txn.Get(key)
		if err == nil {
			return nil
		}
		if err != badger.ErrKeyNotFound {
			return err
		}
		return txn.Set(key, data)
	})
	return hash, err
}

func (s *Store) Get(hash string) ([]byte, error) {
	var val []byte
	err := s.db.View(func(txn *badger.Txn) error {
		item, err := txn.Get([]byte(hash))
		if err != nil {
			return err
		}
		val, err = item.ValueCopy(nil)
		return err
	})
	return val, err
}

// ListHashes returns the hex hash of every segment held in the store. It iterates
// keys only (values not fetched), so it is cheap relative to the data size.
func (s *Store) ListHashes() ([]string, error) {
	var hashes []string
	err := s.db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.PrefetchValues = false
		it := txn.NewIterator(opts)
		defer it.Close()
		for it.Rewind(); it.Valid(); it.Next() {
			hashes = append(hashes, string(it.Item().KeyCopy(nil)))
		}
		return nil
	})
	return hashes, err
}

func (s *Store) Has(hash string) (bool, error) {
	err := s.db.View(func(txn *badger.Txn) error {
		_, err := txn.Get([]byte(hash))
		return err
	})
	if err == nil {
		return true, nil
	}
	if err == badger.ErrKeyNotFound {
		return false, nil
	}
	return false, err
}

// Delete removes a segment by hash. Deleting an absent key is a no-op. Callers
// must ensure no still-held genome references the hash (segments are shared, so
// reference-counting is the caller's responsibility).
func (s *Store) Delete(hash string) error {
	return s.db.Update(func(txn *badger.Txn) error {
		return txn.Delete([]byte(hash))
	})
}
