package main

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func TestRotatingLogCapsSize(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "server.log")
	l, err := openLog(p, 64, 3) // tiny cap, 3 backups
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	chunk := []byte("0123456789abcdef") // 16 bytes
	for i := 0; i < 40; i++ {
		if _, err := l.Write(chunk); err != nil {
			t.Fatal(err)
		}
	}
	st, _ := os.Stat(p)
	if st.Size() > 64 {
		t.Fatalf("size %d exceeds cap", st.Size())
	}
	cnt := 0
	for i := 1; i <= 4; i++ {
		if _, err := os.Stat(p + "." + strconv.Itoa(i)); err == nil {
			cnt++
		}
	}
	if cnt < 1 {
		t.Fatal("expected at least one rotated backup")
	}
}
