// Package index maintains a reference-count index over content-addressed segments.
//
// Key layout (designed to share a BadgerDB instance with the segment store —
// plain hex keys used by the store never collide with these prefixed keys):
//
//	seg:{hash}          → 8-byte LE int64  (byte length of the segment)
//	ref:{hash}:{genome} → nil              (presence = genome references this hash)
//
// The two-key split avoids read-modify-write contention: each genome writes
// its own ref: key independently, so concurrent imports never conflict.
// Idempotency is free — writing the same ref: key twice is a no-op.
package index

import (
	"encoding/binary"
	"fmt"
	"sort"
	"strings"

	"github.com/dgraph-io/badger/v4"
	"github.com/luizcarvalho/genome-hub/internal/manifest"
)

const (
	segPrefix = "seg:"
	refPrefix = "ref:"
	hashLen   = 64 // blake3 hex digest is always 64 chars
)

// Index wraps a BadgerDB instance and provides reference-counted segment tracking.
type Index struct {
	db    *badger.DB
	owned bool // true = we opened db, we must close it
}

// New wraps an existing BadgerDB. The caller retains ownership and must close db.
func New(db *badger.DB) *Index {
	return &Index{db: db}
}

// Open opens a new BadgerDB at dir for exclusive use by this Index.
func Open(dir string) (*Index, error) {
	opts := badger.DefaultOptions(dir).WithLogger(nil)
	db, err := badger.Open(opts)
	if err != nil {
		return nil, err
	}
	return &Index{db: db, owned: true}, nil
}

// Close closes the underlying DB only if this Index owns it.
func (idx *Index) Close() error {
	if idx.owned {
		return idx.db.Close()
	}
	return nil
}

// Put records that genomeID references the segment identified by hash.
// Idempotent: calling with the same (hash, genomeID) pair has no effect.
func (idx *Index) Put(hash string, length int, genomeID string) error {
	segKey := []byte(segPrefix + hash)
	refKey := []byte(refPrefix + hash + ":" + genomeID)
	return idx.db.Update(func(txn *badger.Txn) error {
		if _, err := txn.Get(segKey); err == badger.ErrKeyNotFound {
			val := make([]byte, 8)
			binary.LittleEndian.PutUint64(val, uint64(length))
			if err := txn.Set(segKey, val); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		return txn.Set(refKey, []byte{})
	})
}

// Delete removes the reference from genomeID to hash.
// If no references remain after deletion, the seg: entry is also removed.
func (idx *Index) Delete(hash, genomeID string) error {
	refKey := []byte(refPrefix + hash + ":" + genomeID)
	return idx.db.Update(func(txn *badger.Txn) error {
		if err := txn.Delete(refKey); err != nil && err != badger.ErrKeyNotFound {
			return err
		}
		prefix := []byte(refPrefix + hash + ":")
		opts := badger.DefaultIteratorOptions
		opts.PrefetchValues = false
		it := txn.NewIterator(opts)
		it.Seek(prefix)
		hasRefs := it.ValidForPrefix(prefix)
		it.Close()
		if !hasRefs {
			return txn.Delete([]byte(segPrefix + hash))
		}
		return nil
	})
}

// RefCount returns how many genomes reference hash.
func (idx *Index) RefCount(hash string) (int, error) {
	prefix := []byte(refPrefix + hash + ":")
	count := 0
	err := idx.db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.PrefetchValues = false
		it := txn.NewIterator(opts)
		defer it.Close()
		for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
			count++
		}
		return nil
	})
	return count, err
}

// ReferencedBy returns the genome IDs that reference hash, sorted.
func (idx *Index) ReferencedBy(hash string) ([]string, error) {
	prefixStr := refPrefix + hash + ":"
	prefix := []byte(prefixStr)
	var genomes []string
	err := idx.db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.PrefetchValues = false
		it := txn.NewIterator(opts)
		defer it.Close()
		for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
			genomes = append(genomes, strings.TrimPrefix(string(it.Item().Key()), prefixStr))
		}
		return nil
	})
	return genomes, err
}

// Genomes returns all genome IDs present in the index, sorted.
// Hash hex is always hashLen chars, so the genome ID starts at position hashLen+1
// after stripping the "ref:" prefix.
func (idx *Index) Genomes() ([]string, error) {
	prefix := []byte(refPrefix)
	seen := map[string]struct{}{}
	err := idx.db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.PrefetchValues = false
		it := txn.NewIterator(opts)
		defer it.Close()
		for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
			key := string(it.Item().Key())
			rest := key[len(refPrefix):]          // "{hash}:{genome}"
			if len(rest) > hashLen+1 {            // sanity check
				seen[rest[hashLen+1:]] = struct{}{} // skip "{hash}:"
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	genomes := make([]string, 0, len(seen))
	for g := range seen {
		genomes = append(genomes, g)
	}
	sort.Strings(genomes)
	return genomes, nil
}

// Stats holds aggregate deduplication metrics.
type Stats struct {
	Genomes         []string
	TotalSegments   int
	CoreBytes       int64            // shared by all N genomes (core genome)
	PartialBytes    int64            // shared by 2..N-1 genomes
	UniqueBytes     int64            // unique to exactly one genome
	UniquePerGenome map[string]int64 // per-genome breakdown of unique bytes
	StoredBytes     int64            // actual bytes in store (one copy per unique hash)
	NaiveBytes      int64            // sum of all segment appearances across genomes
}

// Summary computes aggregate deduplication stats across all indexed genomes.
// It performs two passes over the index: one over ref: keys (in memory),
// one over seg: keys.
func (idx *Index) Summary() (Stats, error) {
	// Pass 1: build hash → []genomeID map from all ref: entries.
	hashRefs := map[string][]string{}
	err := idx.db.View(func(txn *badger.Txn) error {
		prefix := []byte(refPrefix)
		opts := badger.DefaultIteratorOptions
		opts.PrefetchValues = false
		it := txn.NewIterator(opts)
		defer it.Close()
		for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
			rest := string(it.Item().Key())[len(refPrefix):]
			if len(rest) <= hashLen+1 {
				continue
			}
			hash := rest[:hashLen]
			genome := rest[hashLen+1:]
			hashRefs[hash] = append(hashRefs[hash], genome)
		}
		return nil
	})
	if err != nil {
		return Stats{}, err
	}

	// Derive genome list from ref data.
	genomeSet := map[string]struct{}{}
	for _, gs := range hashRefs {
		for _, g := range gs {
			genomeSet[g] = struct{}{}
		}
	}
	genomes := make([]string, 0, len(genomeSet))
	for g := range genomeSet {
		genomes = append(genomes, g)
	}
	sort.Strings(genomes)
	n := len(genomes)

	stats := Stats{
		Genomes:         genomes,
		UniquePerGenome: make(map[string]int64, n),
	}
	for _, g := range genomes {
		stats.UniquePerGenome[g] = 0
	}

	// Pass 2: scan seg: entries, categorise by ref count.
	err = idx.db.View(func(txn *badger.Txn) error {
		prefix := []byte(segPrefix)
		opts := badger.DefaultIteratorOptions
		it := txn.NewIterator(opts)
		defer it.Close()
		for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
			item := it.Item()
			hash := string(item.Key())[len(segPrefix):]
			var length int64
			if err := item.Value(func(val []byte) error {
				if len(val) >= 8 {
					length = int64(binary.LittleEndian.Uint64(val))
				}
				return nil
			}); err != nil {
				return err
			}
			stats.TotalSegments++
			stats.StoredBytes += length
			refs := hashRefs[hash]
			rc := len(refs)
			stats.NaiveBytes += length * int64(rc)
			switch {
			case n > 0 && rc == n:
				stats.CoreBytes += length
			case rc == 1:
				stats.UniqueBytes += length
				stats.UniquePerGenome[refs[0]] += length
			default:
				stats.PartialBytes += length
			}
		}
		return nil
	})
	return stats, err
}

// RemoveGenome deletes all index references for genomeID.
// Segments with no remaining references also have their seg: entry removed.
// O(total ref: entries) — fine for hundreds of genomes.
func (idx *Index) RemoveGenome(genomeID string) error {
	suffix := ":" + genomeID
	// Collect all ref: keys that end in ":{genomeID}".
	var refKeys [][]byte
	err := idx.db.View(func(txn *badger.Txn) error {
		prefix := []byte(refPrefix)
		opts := badger.DefaultIteratorOptions
		opts.PrefetchValues = false
		it := txn.NewIterator(opts)
		defer it.Close()
		for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
			key := string(it.Item().Key())
			if strings.HasSuffix(key, suffix) {
				refKeys = append(refKeys, append([]byte{}, it.Item().Key()...))
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	// Delete each ref: key; if hash has no remaining refs, remove seg: too.
	for _, refKey := range refKeys {
		rest := string(refKey)[len(refPrefix):]
		hash := rest[:hashLen]
		if err := idx.Delete(hash, genomeID); err != nil {
			return err
		}
	}
	return nil
}

// Rebuild drops all index entries and reconstructs them from the given manifest files.
// Genome IDs are taken from manifest.Assembly; falls back to the file path.
// Safe to run at any time — acts as both a repair tool and a consistency check.
func (idx *Index) Rebuild(manifestPaths []string) error {
	if err := idx.dropIndexKeys(); err != nil {
		return fmt.Errorf("drop index: %w", err)
	}
	for _, path := range manifestPaths {
		m, err := manifest.Read(path)
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		genomeID := m.Assembly
		if genomeID == "" {
			genomeID = path
		}
		for _, chrom := range m.Chromosomes {
			for _, seg := range chrom.Segments {
				hash := strings.TrimPrefix(seg.Hash, "blake3:")
				if err := idx.Put(hash, seg.Length, genomeID); err != nil {
					return fmt.Errorf("index %s/%s: %w", genomeID, hash, err)
				}
			}
		}
	}
	return nil
}

// dropIndexKeys deletes all seg: and ref: keys in batches.
func (idx *Index) dropIndexKeys() error {
	for _, prefix := range []string{segPrefix, refPrefix} {
		pfx := []byte(prefix)
		for {
			done := true
			err := idx.db.Update(func(txn *badger.Txn) error {
				opts := badger.DefaultIteratorOptions
				opts.PrefetchValues = false
				it := txn.NewIterator(opts)
				var keys [][]byte
				for it.Seek(pfx); it.ValidForPrefix(pfx) && len(keys) < 10_000; it.Next() {
					keys = append(keys, append([]byte{}, it.Item().Key()...))
				}
				if it.ValidForPrefix(pfx) {
					done = false
				}
				it.Close()
				for _, k := range keys {
					if err := txn.Delete(k); err != nil {
						return err
					}
				}
				return nil
			})
			if err != nil {
				return err
			}
			if done {
				break
			}
		}
	}
	return nil
}
