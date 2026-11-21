package catalog

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

// CacheKey names the catalog kept on disk, models.dev's provider catalog
// and the lab catalog beside it, by what the files hold, not by when they
// were written. A Claude account's model list fetches the catalog again
// every 45 minutes (Asked), and usage keyed its whole index on the file's
// mtime: each fetch, the same bytes, had the next usage query read every
// session file again, ~15 s of CPU over a long history.
func CacheKey() string { return fileKey(CachePath()) + "|" + fileKey(LabsPath()) }

// fileKeySettled is how old a file's mtime must be for any later write to
// stamp a later one, many ticks of the coarsest clock (matching
// filememo.settled and provider.codexListSettled).
const fileKeySettled = 2 * time.Second

// fileKeys holds each file's last key by the stat it was hashed at, so a
// key costs a stat once the file's mtime has settled.
var fileKeys = struct {
	sync.Mutex
	m map[string]keyedFile
}{m: map[string]keyedFile{}}

type keyedFile struct {
	size int64
	mod  time.Time
	key  string
}

// fileKey is a hash of path's bytes, "missing" when there is no file, and
// its stat when it can't be read: an unknown read is a change each time
// the file is written, as it was before.
func fileKey(path string) string {
	now := time.Now()
	st, err := os.Stat(path)
	if err != nil {
		fileKeys.Lock()
		delete(fileKeys.m, path)
		fileKeys.Unlock()
		return "missing"
	}
	if now.Sub(st.ModTime()) >= fileKeySettled {
		fileKeys.Lock()
		k, ok := fileKeys.m[path]
		fileKeys.Unlock()
		if ok && k.size == st.Size() && k.mod.Equal(st.ModTime()) {
			return k.key
		}
	}
	// hashed as it is read: the catalog is ~6 MB, and holding it whole
	// for its sum added that much to the start's peak
	h := sha256.New()
	f, err := os.Open(path)
	var settled bool
	if err == nil {
		var fst, after os.FileInfo
		fst, err = f.Stat()
		if err == nil {
			st = fst
			_, err = io.Copy(h, f)
		}
		if err == nil {
			after, err = f.Stat()
			if err == nil {
				st = after
				settled = fst.Size() == after.Size() &&
					fst.ModTime().Equal(after.ModTime()) &&
					now.Sub(after.ModTime()) >= fileKeySettled
			}
		}
		f.Close()
	}
	if err != nil {
		fileKeys.Lock()
		delete(fileKeys.m, path)
		fileKeys.Unlock()
		return fmt.Sprintf("stat:%d:%d", st.Size(), st.ModTime().UnixNano())
	}
	key := hex.EncodeToString(h.Sum(nil))
	fileKeys.Lock()
	if settled {
		fileKeys.m[path] = keyedFile{size: st.Size(), mod: st.ModTime(), key: key}
	} else {
		delete(fileKeys.m, path)
	}
	fileKeys.Unlock()
	return key
}

// keepFetched writes b to p, unless p holds b already: then only p's time
// is now, which Stale, Missing and Asked read as the fetch. It reports
// whether p's bytes changed.
func keepFetched(p string, b []byte) (bool, error) {
	if old, err := os.ReadFile(p); err == nil && bytes.Equal(old, b) {
		now := time.Now()
		if err := os.Chtimes(p, now, now); err != nil {
			return false, err
		}
		return false, nil
	}
	return true, writeWhole(p, b, time.Time{})
}
