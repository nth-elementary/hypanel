package files

import (
	"archive/tar"
	"archive/zip"
	"compress/bzip2"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"hypanel/internal/core"
)

const maxArchiveEntries = 5000

type archiveKind int

const (
	notArchive archiveKind = iota
	kindZip
	kindTar
	kindTarGz
	kindTarBz2
	kindTarXz
)

func archiveKindOf(name string) archiveKind {
	lower := strings.ToLower(name)
	switch {
	case strings.HasSuffix(lower, ".zip"):
		return kindZip
	case strings.HasSuffix(lower, ".tar"):
		return kindTar
	case strings.HasSuffix(lower, ".tar.gz"), strings.HasSuffix(lower, ".tgz"):
		return kindTarGz
	case strings.HasSuffix(lower, ".tar.bz2"), strings.HasSuffix(lower, ".tbz2"):
		return kindTarBz2
	case strings.HasSuffix(lower, ".tar.xz"), strings.HasSuffix(lower, ".txz"):
		return kindTarXz
	}
	return notArchive
}

type archiveEntry struct {
	Name    string `json:"name"`
	Dir     bool   `json:"dir"`
	Size    int64  `json:"size"`
	ModTime int64  `json:"modTime"`
	Link    string `json:"link,omitempty"`
}

// listArchive reads an archive's table of contents. It stops at limit entries, so an
// enormous archive can't tie up the panel, and reports whether it stopped early.
func listArchive(ctx context.Context, path string, limit int) ([]archiveEntry, bool, error) {
	kind := archiveKindOf(path)
	if kind == kindZip {
		return listZip(path, limit)
	}
	if kind == notArchive {
		return nil, false, fmt.Errorf("unsupported archive type")
	}
	in, err := os.Open(path)
	if err != nil {
		return nil, false, err
	}
	defer in.Close()

	var src io.Reader = in
	switch kind {
	case kindTarGz:
		gz, err := gzip.NewReader(in)
		if err != nil {
			return nil, false, err
		}
		defer gz.Close()
		src = gz
	case kindTarBz2:
		src = bzip2.NewReader(in)
	case kindTarXz:
		// The standard library has no xz; the xz binary ships with Ubuntu.
		cmd := exec.CommandContext(ctx, "xz", "-dc")
		cmd.Stdin = in
		pipe, err := cmd.StdoutPipe()
		if err != nil {
			return nil, false, err
		}
		if err := cmd.Start(); err != nil {
			return nil, false, err
		}
		defer func() {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}()
		src = pipe
	}

	entries := []archiveEntry{}
	reader := tar.NewReader(src)
	for {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		hdr, err := reader.Next()
		if err == io.EOF {
			return entries, false, nil
		}
		if err != nil {
			if len(entries) > 0 {
				return entries, true, nil // truncated archive: show what's readable
			}
			return nil, false, fmt.Errorf("can't read archive: %w", err)
		}
		if len(entries) >= limit {
			return entries, true, nil
		}
		entries = append(entries, archiveEntry{
			Name: hdr.Name, Dir: hdr.Typeflag == tar.TypeDir, Size: hdr.Size,
			ModTime: hdr.ModTime.Unix(), Link: hdr.Linkname,
		})
	}
}

func listZip(path string, limit int) ([]archiveEntry, bool, error) {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return nil, false, fmt.Errorf("can't read archive: %w", err)
	}
	defer zr.Close()
	entries := []archiveEntry{}
	for _, f := range zr.File {
		if len(entries) >= limit {
			return entries, true, nil
		}
		entries = append(entries, archiveEntry{
			Name: f.Name, Dir: f.FileInfo().IsDir(), Size: int64(f.UncompressedSize64), ModTime: f.Modified.Unix(),
		})
	}
	return entries, false, nil
}

func (f *Files) archive(w http.ResponseWriter, r *http.Request) {
	p, err := clean(r.URL.Query().Get("path"))
	if err != nil {
		core.Fail(w, http.StatusBadRequest, "%v", err)
		return
	}
	entries, truncated, err := listArchive(r.Context(), p, maxArchiveEntries)
	if err != nil {
		core.Fail(w, http.StatusBadRequest, "%v", err)
		return
	}
	core.OK(w, map[string]any{"entries": entries, "truncated": truncated})
}

// extractZip unpacks natively, so the server doesn't need the unzip package. Entries that
// would land outside dest are refused, and symlinks aren't created: a planted link is the
// other way an archive could write outside the destination.
func extractZip(ctx context.Context, path, dest string, t *core.Task) error {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return err
	}
	defer zr.Close()
	root, err := filepath.Abs(dest)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	for _, entry := range zr.File {
		if err := ctx.Err(); err != nil {
			return err
		}
		target := filepath.Join(root, filepath.FromSlash(entry.Name))
		if target != root && !strings.HasPrefix(target, root+string(filepath.Separator)) {
			return fmt.Errorf("refusing unsafe path in archive: %s", entry.Name)
		}
		mode := entry.Mode()
		if mode&os.ModeSymlink != 0 {
			t.Printf("skipped symlink %s\n", entry.Name)
			continue
		}
		if entry.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		perm := mode.Perm()
		if perm == 0 {
			perm = 0o644
		}
		if err := writeZipEntry(entry, target, perm); err != nil {
			return fmt.Errorf("%s: %w", entry.Name, err)
		}
		_ = os.Chtimes(target, entry.Modified, entry.Modified)
	}
	t.Printf("extracted %d entries\n", len(zr.File))
	return nil
}

func writeZipEntry(entry *zip.File, target string, perm os.FileMode) error {
	in, err := entry.Open()
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// createZip stores paths relative to base. Symlinks are skipped: zip tools disagree about
// them, and following one could pull in files from anywhere on the server.
func createZip(ctx context.Context, dest, base string, paths []string, t *core.Task) error {
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	done := false
	defer func() {
		out.Close()
		if !done {
			_ = os.Remove(dest) // never leave a partial archive behind
		}
	}()
	zw := zip.NewWriter(out)
	count := 0
	for _, root := range paths {
		err := filepath.Walk(root, func(path string, info fs.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if path == dest {
				return nil
			}
			if info.Mode()&os.ModeSymlink != 0 {
				t.Printf("skipped symlink %s\n", path)
				return nil
			}
			rel, err := filepath.Rel(base, path)
			if err != nil {
				return err
			}
			hdr, err := zip.FileInfoHeader(info)
			if err != nil {
				return err
			}
			hdr.Name = filepath.ToSlash(rel)
			if info.IsDir() {
				hdr.Name += "/"
				_, err := zw.CreateHeader(hdr)
				return err
			}
			if !info.Mode().IsRegular() {
				return nil
			}
			hdr.Method = zip.Deflate
			entry, err := zw.CreateHeader(hdr)
			if err != nil {
				return err
			}
			in, err := os.Open(path)
			if err != nil {
				return err
			}
			defer in.Close()
			if _, err := io.Copy(entry, in); err != nil {
				return err
			}
			count++
			return nil
		})
		if err != nil {
			return err
		}
	}
	if err := zw.Close(); err != nil {
		return err
	}
	t.Printf("compressed %d files\n", count)
	done = true
	return nil
}
