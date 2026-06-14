package httpapi

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// DeleteResult reports what a cache delete removed vs kept.
type DeleteResult struct {
	Assembly string `json:"assembly"`
	Deleted  int    `json:"deleted"` // segments removed (unique to this genome)
	Kept     int    `json:"kept"`    // segments retained (shared with other held genomes)
}

// deleteGenome drops a genome from the local cache: it removes only the segments
// no other held genome needs (segments are shared, so blind deletion would
// corrupt others), then stops seeding it. Returns how many were freed vs kept.
func (srv *server) deleteGenome(assembly string) (DeleteResult, error) {
	cur := srv.cur()

	// Raw-blob delta: file-served, no store entries — just unseed.
	if _, ok := cur.Deltas[assembly]; ok {
		if !srv.unseed(assembly) {
			return DeleteResult{}, fmt.Errorf("%s is not in this node's catalog", assembly)
		}
		return DeleteResult{Assembly: assembly}, nil
	}

	// Recipe-backed delta: refcount chunk hashes against other recipe deltas.
	if _, ok := cur.Recipes[assembly]; ok {
		srv.cancelDownload(assembly)
		mine := srv.recipeSegsFor(assembly).hashes

		keep := map[string]struct{}{}
		for a := range cur.Recipes {
			if a == assembly {
				continue
			}
			for h := range srv.recipeSegsFor(a).hashes {
				keep[h] = struct{}{}
			}
		}

		res := DeleteResult{Assembly: assembly}
		for h := range mine {
			if _, shared := keep[h]; shared {
				res.Kept++
				continue
			}
			if has, _ := srv.store.Has(h); has {
				if err := srv.store.Delete(h); err != nil {
					return res, err
				}
			}
			res.Deleted++
		}
		srv.unseed(assembly)
		return res, nil
	}

	// Manifest genome.
	if _, ok := cur.Manifests[assembly]; !ok {
		return DeleteResult{}, fmt.Errorf("%s is not in this node's catalog", assembly)
	}
	srv.cancelDownload(assembly) // stop any in-flight fetch first

	mine := srv.segsFor(assembly).hashes

	// Union of every OTHER held manifest's segments — these must survive.
	keep := map[string]struct{}{}
	for a := range cur.Manifests {
		if a == assembly {
			continue
		}
		for h := range srv.segsFor(a).hashes {
			keep[h] = struct{}{}
		}
	}

	res := DeleteResult{Assembly: assembly}
	for h := range mine {
		if _, shared := keep[h]; shared {
			res.Kept++
			continue
		}
		if has, _ := srv.store.Has(h); has {
			if err := srv.store.Delete(h); err != nil {
				return res, err
			}
		}
		res.Deleted++
	}

	srv.unseed(assembly) // remove the manifest from the catalog too
	return res, nil
}

// unseed removes a genome from the catalog so the node stops advertising/serving
// it — without touching store segments. The persisted file (if it lives in
// manifestDir) is deleted so it does not reappear on restart. Handles manifests,
// recipe-backed deltas, and raw-blob deltas.
func (srv *server) unseed(assembly string) bool {
	old := srv.cur()
	next := &Catalog{
		Manifests: cloneMap(old.Manifests),
		Deltas:    cloneMap(old.Deltas),
		Recipes:   cloneMap(old.Recipes),
	}

	var path string
	if p, ok := old.Manifests[assembly]; ok {
		path = p
		delete(next.Manifests, assembly)
	} else if p, ok := old.Recipes[assembly]; ok {
		path = p
		delete(next.Recipes, assembly)
	} else if p, ok := old.Deltas[assembly]; ok {
		path = p
		delete(next.Deltas, assembly)
	} else {
		return false
	}

	srv.catalog.Store(next)

	// Only remove files we own (tracked under manifestDir); never delete origin files.
	if srv.manifestDir != "" && strings.HasPrefix(filepath.Clean(path), filepath.Clean(srv.manifestDir)) {
		os.Remove(path)
	}

	srv.mu.Lock()
	delete(srv.covCache, assembly)
	delete(srv.discCache, assembly)
	srv.mu.Unlock()
	return true
}
