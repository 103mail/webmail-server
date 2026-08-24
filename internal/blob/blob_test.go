package blob

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPathMatchesPHPLayout(t *testing.T) {
	s := New("data/attachments")
	hash := "d41d8cd98f00b204e9800998ecf8427e"
	want := filepath.Join("data/attachments", "d4", "1d", "8c", "d9", hash)
	if got := s.Path(hash); got != want {
		t.Errorf("Path = %q, want %q", got, want)
	}
}

func TestPutThenGet(t *testing.T) {
	s := New(t.TempDir())
	data := []byte("hello")
	hash, err := s.Put(data)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if hash != Hash(data) {
		t.Errorf("Put returned %q, want %q", hash, Hash(data))
	}
	got, err := s.Get(hash)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got) != "hello" {
		t.Errorf("Get = %q, want %q", got, "hello")
	}
}

func TestPutIsIdempotent(t *testing.T) {
	s := New(t.TempDir())
	data := []byte("same")
	first, err := s.Put(data)
	if err != nil {
		t.Fatalf("first Put: %v", err)
	}
	second, err := s.Put(data)
	if err != nil {
		t.Fatalf("second Put: %v", err)
	}
	if first != second {
		t.Errorf("hashes differ: %q and %q", first, second)
	}
}

func TestPutLeavesNoTemporaryFiles(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	if _, err := s.Put([]byte("x")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	var stray []string
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && len(info.Name()) > 0 && info.Name()[0] == '.' {
			stray = append(stray, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(stray) > 0 {
		t.Errorf("temporary files left behind: %v", stray)
	}
}

func TestGetMissingReturnsNothing(t *testing.T) {
	s := New(t.TempDir())
	got, err := s.Get("00000000000000000000000000000000")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != nil {
		t.Errorf("Get = %v, want nil", got)
	}
}
