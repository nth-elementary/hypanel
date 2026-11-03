package files

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
	"testing"

	"hypanel/internal/core"
)

func task(t *testing.T) *core.Task { t.Helper(); return &core.Task{} }

func TestZipRoundTripAndListing(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "site")
	if err := os.MkdirAll(filepath.Join(src, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "sub", "a.txt"), []byte("hello"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/passwd", filepath.Join(src, "link")); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(dir, "site.zip")
	if err := createZip(context.Background(), archive, dir, []string{src}, task(t)); err != nil {
		t.Fatal(err)
	}

	entries, truncated, err := listArchive(context.Background(), archive, 100)
	if err != nil || truncated {
		t.Fatalf("list: %v %v", err, truncated)
	}
	names := map[string]bool{}
	for _, e := range entries {
		names[e.Name] = true
	}
	if !names["site/sub/a.txt"] || !names["site/sub/"] || names["site/link"] {
		t.Errorf("entries: %v", names)
	}

	dest := filepath.Join(dir, "out")
	if err := extractZip(context.Background(), archive, dest, task(t)); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dest, "site", "sub", "a.txt"))
	if err != nil || string(got) != "hello" {
		t.Errorf("extracted %q, %v", got, err)
	}
	if info, _ := os.Stat(filepath.Join(dest, "site", "sub", "a.txt")); info.Mode().Perm() != 0o640 {
		t.Errorf("mode %o", info.Mode().Perm())
	}
}

func TestZipSlipRefused(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "evil.zip")
	out, _ := os.Create(archive)
	zw := zip.NewWriter(out)
	w, _ := zw.Create("../../escaped.txt")
	_, _ = w.Write([]byte("x"))
	zw.Close()
	out.Close()

	dest := filepath.Join(dir, "a", "b")
	if err := extractZip(context.Background(), archive, dest, task(t)); err == nil {
		t.Fatal("expected refusal")
	}
	if _, err := os.Stat(filepath.Join(dir, "escaped.txt")); err == nil {
		t.Fatal("file escaped the destination")
	}
}

func TestListTarGzAndLimit(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
		_ = tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: 1, Typeflag: tar.TypeReg})
		_, _ = tw.Write([]byte("x"))
	}
	tw.Close()
	gz.Close()
	archive := filepath.Join(t.TempDir(), "x.tar.gz")
	if err := os.WriteFile(archive, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	entries, truncated, err := listArchive(context.Background(), archive, 100)
	if err != nil || truncated || len(entries) != 3 || entries[0].Name != "a.txt" || entries[0].Size != 1 {
		t.Errorf("full list: %+v %v %v", entries, truncated, err)
	}
	entries, truncated, err = listArchive(context.Background(), archive, 2)
	if err != nil || !truncated || len(entries) != 2 {
		t.Errorf("limited list: %d %v %v", len(entries), truncated, err)
	}
}

func TestArchiveKind(t *testing.T) {
	for name, want := range map[string]archiveKind{
		"a.ZIP": kindZip, "a.tar": kindTar, "a.tar.gz": kindTarGz, "a.tgz": kindTarGz,
		"a.tar.bz2": kindTarBz2, "a.tar.xz": kindTarXz, "a.txt": notArchive, "a.gz": notArchive,
	} {
		if got := archiveKindOf(name); got != want {
			t.Errorf("%s: got %d want %d", name, got, want)
		}
	}
}

func TestProtected(t *testing.T) {
	for path, want := range map[string]bool{
		"/": true, "/etc": true, "/www/sites": true, "/var/lib/docker/x": true,
		"/www/sites/blog": false, "/srv/x": false, "/opt/stacks/app": false,
	} {
		if got := protected(path); got != want {
			t.Errorf("%s: got %v", path, got)
		}
	}
}

func TestReferencedIn(t *testing.T) {
	conf := `server {
    root /www/sites/blog/index;
    access_log /www/sites/blog/log/access.log;
    ssl_certificate /etc/nginx/ssl/example.org/fullchain.pem;
    include /www/sites/wiki/rewrite/a.conf;
}`
	for path, want := range map[string]bool{
		"/www/sites/blog":            true,
		"/www/sites/blog/index":      true,
		"/www/sites/blog/log":        true,
		"/www/sites":                 true,
		"/etc/nginx/ssl/example.org":     true,
		"/www/sites/wiki":       true,
		"/www/sites/blo":             false, // a prefix of a name is a different path
		"/www/sites/blog/index/deep": false, // inside the root, but nginx doesn't name it
		"/www/sites/other":           false,
	} {
		if got := referencedIn(conf, path); got != want {
			t.Errorf("%s: got %v", path, got)
		}
	}
}

func TestWithin(t *testing.T) {
	for _, c := range []struct {
		inner, outer string
		want         bool
	}{
		{"/srv/read", "/srv/read", true},
		{"/srv/read/x", "/srv/read", true},
		{"/srv/read", "/srv", true},
		{"/srv/reader", "/srv/read", false},
		{"/www", "/www/sites/blog", false},
	} {
		if got := within(c.inner, c.outer); got != c.want {
			t.Errorf("within(%s, %s) = %v", c.inner, c.outer, got)
		}
	}
}
