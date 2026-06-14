package store

import (
	"fmt"

	"github.com/dgraph-io/badger/v4"
	"github.com/zeebo/blake3"
)

type Store struct {
	db    *badger.DB
	cache *lruCache // nil = unbounded (origin); set via SetCacheLimit (cache node)
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
	if err == nil && s.cache != nil {
		// Record the new (or refreshed) key, then evict LRU keys until under cap.
		for _, k := range s.cache.add(hash, int64(len(data))) {
			_ = s.db.Update(func(txn *badger.Txn) error { return txn.Delete([]byte(k)) })
		}
	}
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
	if err == nil && s.cache != nil {
		s.cache.touch(hash) // reading a segment marks it recently used
	}
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
	err := s.db.Update(func(txn *badger.Txn) error {
		return txn.Delete([]byte(hash))
	})
	if err == nil && s.cache != nil {
		s.cache.remove(hash)
	}
	return err
}

// SetCacheLimit turns this store into a bounded LRU cache of at most maxBytes:
// least-recently-used segments are evicted when a Put would exceed the cap. It
// builds the recency index from the keys already present and evicts down to the
// cap immediately. maxBytes <= 0 leaves the store unbounded (the default, and
// what an origin should use — it is the source of truth, not a cache).
func (s *Store) SetCacheLimit(maxBytes int64) error {
	if maxBytes <= 0 {
		return nil
	}
	c := newLRU(maxBytes)
	var toEvict []string
	err := s.db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.PrefetchValues = false
		it := txn.NewIterator(opts)
		defer it.Close()
		for it.Rewind(); it.Valid(); it.Next() {
			item := it.Item()
			toEvict = append(toEvict, c.add(string(item.KeyCopy(nil)), item.ValueSize())...)
		}
		return nil
	})
	if err != nil {
		return err
	}
	s.cache = c
	for _, k := range toEvict {
		if err := s.Delete(k); err != nil {
			return err
		}
	}
	return nil
}

// CacheStats reports current and maximum cache bytes; both zero when unbounded.
func (s *Store) CacheStats() (cur, max int64) {
	if s.cache == nil {
		return 0, 0
	}
	return s.cache.stats()
}
