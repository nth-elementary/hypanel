package core

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestTrimLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.log")
	var content strings.Builder
	for i := 0; i < 1000; i++ {
		content.WriteString("line of log output\n")
	}
	if err := os.WriteFile(path, []byte(content.String()), 0o640); err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(path)

	freed, err := TrimLog(path, 100)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(path)
	if after.Size() > 100 || after.Size() == 0 {
		t.Errorf("size after trim: %d", after.Size())
	}
	if freed != before.Size()-after.Size() {
		t.Errorf("freed %d, sizes %d -> %d", freed, before.Size(), after.Size())
	}
	data, _ := os.ReadFile(path)
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		if line != "line of log output" {
			t.Errorf("partial line kept: %q", line)
		}
	}
	// A log writer holding the file open must keep writing to the same file.
	if before.Sys().(*syscall.Stat_t).Ino != after.Sys().(*syscall.Stat_t).Ino {
		t.Error("file was replaced rather than trimmed in place")
	}
	if after.Mode().Perm() != 0o640 {
		t.Errorf("mode changed to %o", after.Mode().Perm())
	}
}

func TestTrimLogNoOpAndEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.log")
	if err := os.WriteFile(path, []byte("short\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if freed, err := TrimLog(path, 1000); err != nil || freed != 0 {
		t.Errorf("under the limit: freed %d, err %v", freed, err)
	}
	if freed, err := TrimLog(path, 0); err != nil || freed != 6 {
		t.Errorf("keep nothing: freed %d, err %v", freed, err)
	}
	if info, _ := os.Stat(path); info.Size() != 0 {
		t.Errorf("size %d", info.Size())
	}
	if _, err := TrimLog(path, -1); err == nil {
		t.Error("negative keep should be refused")
	}
}
