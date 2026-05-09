package services

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// thread_index.go maintains a per-agent fallback mapping from Outlook's
// Thread-Index conversation prefix (gated on a normalised-subject match)
// to the canonical ThreadRoot under which the thread's memory is stored.
// Exchange and Outlook frequently rewrite or drop standard References on
// internal hops; without this fallback every such reply spawns a fresh
// memory file and re-runs the task.
//
// The store is a single JSON file co-located with memoryDir so it shares
// the agent's data volume. It is intentionally NOT shared between agents
// — each agent only needs to recognize threads it has participated in,
// and the cross-machine deployment model has no shared filesystem.

var threadIndexMu sync.Mutex

func threadIndexPath() string {
	return filepath.Join(memoryDir, "thread_index.json")
}

func loadThreadIndex() (map[string]string, error) {
	data, err := os.ReadFile(threadIndexPath())
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]string{}, nil
		}
		return nil, err
	}
	m := map[string]string{}
	if err := json.Unmarshal(data, &m); err != nil {
		// A corrupted index is non-fatal — better to start fresh than to
		// crash the agent loop. The next successful save overwrites it.
		log.Printf("warning: thread_index.json unreadable, starting fresh: %v", err)
		return map[string]string{}, nil
	}
	return m, nil
}

func saveThreadIndex(m map[string]string) error {
	if err := ensureMemoryDir(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(threadIndexPath(), data, 0644)
}

// threadIndexKey composes the dual key. An empty convID disables
// indexing for the entry — non-Outlook clients never write or look up.
func threadIndexKey(convID, subject string) string {
	if convID == "" {
		return ""
	}
	return convID + "|" + NormalizeSubject(subject)
}

// RememberThreadIndex records (convID, normalisedSubject) → root. Called
// whenever memory is created or updated for a thread that carried a
// Thread-Index. Idempotent and safe to call on every inbound.
func RememberThreadIndex(convID, subject, root string) error {
	key := threadIndexKey(convID, subject)
	if key == "" || root == "" {
		return nil
	}
	threadIndexMu.Lock()
	defer threadIndexMu.Unlock()
	idx, err := loadThreadIndex()
	if err != nil {
		return err
	}
	if idx[key] == root {
		return nil
	}
	idx[key] = root
	return saveThreadIndex(idx)
}

// LookupThreadRoot returns a stored ThreadRoot for an Outlook
// conversation when subject also matches. Returns ("", nil) on miss.
func LookupThreadRoot(convID, subject string) (string, error) {
	key := threadIndexKey(convID, subject)
	if key == "" {
		return "", nil
	}
	threadIndexMu.Lock()
	defer threadIndexMu.Unlock()
	idx, err := loadThreadIndex()
	if err != nil {
		return "", err
	}
	return idx[key], nil
}

// ResolveThreadRoot is the canonical "find the existing thread for this
// inbound" lookup. Order:
//  1. References[0] / MessageId — if memory already exists under that
//     key, use it. This is the fast path and the only path for non-MS
//     clients.
//  2. Thread-Index conv ID + normalised subject — when (1) misses and
//     the inbound carries an Outlook fingerprint, consult the index.
//
// On total miss, falls back to email.GetThreadRoot() so brand-new
// threads still get a stable key.
func ResolveThreadRoot(e *Email) (string, error) {
	if e == nil {
		return "", nil
	}
	primary := e.GetThreadRoot()
	if primary != "" {
		if _, err := GetMemory(primary); err == nil {
			return primary, nil
		} else if !errors.Is(err, ErrMemoryNotFound) {
			return "", fmt.Errorf("memory lookup: %w", err)
		}
	}
	if e.ThreadIndexConvID != "" {
		subj := e.ThreadTopic
		if subj == "" {
			subj = e.Subject
		}
		root, err := LookupThreadRoot(e.ThreadIndexConvID, subj)
		if err != nil {
			return "", err
		}
		if root != "" {
			// Confirm the resolved root still has a memory file. A stale
			// index entry (memory deleted out from under us) should not
			// silently misdirect new traffic.
			if _, err := GetMemory(root); err == nil {
				log.Printf("thread index: matched inbound %s to existing thread %s via Thread-Index/Topic",
					e.MessageId, root)
				return root, nil
			} else if !errors.Is(err, ErrMemoryNotFound) {
				return "", fmt.Errorf("memory lookup (resolved): %w", err)
			}
		}
	}
	return primary, nil
}

// PruneThreadIndex drops index entries whose ThreadRoot no longer has a
// corresponding memory file. Returns (kept, pruned). Cheap enough to
// run on a slow timer; the index is one map of short strings.
func PruneThreadIndex() (int, int, error) {
	threadIndexMu.Lock()
	defer threadIndexMu.Unlock()
	idx, err := loadThreadIndex()
	if err != nil {
		return 0, 0, err
	}
	if len(idx) == 0 {
		return 0, 0, nil
	}
	pruned := 0
	for key, root := range idx {
		path := filepath.Join(memoryDir, msgIDToFilename(root))
		if _, statErr := os.Stat(path); statErr != nil {
			if os.IsNotExist(statErr) {
				delete(idx, key)
				pruned++
				continue
			}
			// Transient stat error — leave the entry alone.
			log.Printf("thread index prune: stat %s: %v", path, statErr)
		}
	}
	if pruned == 0 {
		return len(idx), 0, nil
	}
	if err := saveThreadIndex(idx); err != nil {
		return len(idx), pruned, err
	}
	return len(idx), pruned, nil
}

// StartThreadIndexPruner runs PruneThreadIndex on a fixed interval until
// ctx is cancelled. Intended to be launched once from main as a
// background goroutine.
func StartThreadIndexPruner(stop <-chan struct{}, every time.Duration) {
	if every <= 0 {
		every = 6 * time.Hour
	}
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				kept, pruned, err := PruneThreadIndex()
				if err != nil {
					log.Printf("thread index prune error: %v", err)
					continue
				}
				if pruned > 0 {
					log.Printf("thread index pruned %d stale entries (%d remain)", pruned, kept)
				}
			}
		}
	}()
}
