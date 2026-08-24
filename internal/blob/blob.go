// Package blob stores attachment and document bytes on disk, addressed by the
// MD5 of their content. The layout matches the PHP backend exactly so both can
// read the same data directory: four levels of two hex characters, then the
// full hash as the file name.
package blob

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
)

// Store is a content-addressed directory of files.
type Store struct {
	root string
}

// New returns a store rooted at dir, for example "data/attachments".
func New(dir string) *Store {
	return &Store{root: dir}
}

const levels = 4

// Path returns the file path for a hash. It is exported because the API layer
// reports file locations in errors.
func (s *Store) Path(hashMD5 string) string {
	path := s.root
	rest := hashMD5
	for i := 0; i < levels; i++ {
		if len(rest) < 2 {
			break
		}
		path = filepath.Join(path, rest[:2])
		rest = rest[2:]
	}
	return filepath.Join(path, hashMD5)
}

// Hash returns the MD5 of data as lowercase hex, which is what the schema
// stores in hash_md5.
func Hash(data []byte) string {
	sum := md5.Sum(data)
	return hex.EncodeToString(sum[:])
}

// Put writes data and returns its hash. Writing an object that already exists
// is a no-op, since identical content hashes to the same path.
func (s *Store) Put(data []byte) (string, error) {
	hash := Hash(data)
	path := s.Path(hash)
	if _, err := os.Stat(path); err == nil {
		return hash, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", fmt.Errorf("create attachment directory: %w", err)
	}
	// Write to a temporary file first so a concurrent reader never sees a
	// partially written object at the final path.
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return "", fmt.Errorf("create temporary file: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return "", fmt.Errorf("write attachment: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("close attachment: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return "", fmt.Errorf("store attachment: %w", err)
	}
	return hash, nil
}

// Get reads the object with the given hash. A missing object returns empty
// bytes and no error, matching the PHP behaviour of serving an empty body when
// the file is gone.
func (s *Store) Get(hashMD5 string) ([]byte, error) {
	data, err := os.ReadFile(s.Path(hashMD5))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read attachment: %w", err)
	}
	return data, nil
}
