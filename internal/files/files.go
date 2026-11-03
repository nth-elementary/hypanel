// Package files is the file manager: browse, edit, upload, download, permissions
// and archives. The panel admin is root on this server, so access isn't jailed, but
// paths that would wreck the system can't be deleted or moved.
package files

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"

	"hypanel/internal/core"
)

const maxEditSize = 5 << 20

type Files struct{ app *core.App }

func New(app *core.App) *Files { return &Files{app: app} }

func (f *Files) Name() string { return "files" }

func (f *Files) Routes(r chi.Router) {
	r.Get("/files/list", f.list)
	r.Get("/files/read", f.read)
	r.Post("/files/write", f.write)
	r.Post("/files/mkdir", f.mkdir)
	r.Post("/files/rename", f.rename)
	r.Post("/files/delete", f.remove)
	r.Post("/files/chmod", f.chmod)
	r.Post("/files/chown", f.chown)
	r.Post("/files/upload", f.upload)
	r.Get("/files/download", f.download)
	r.Post("/files/compress", f.compress)
	r.Post("/files/extract", f.extract)
	r.Get("/files/archive", f.archive)
	r.Get("/files/size", f.size)
}

// clean requires an absolute path and normalises it.
func clean(p string) (string, error) {
	if !filepath.IsAbs(p) {
		return "", fmt.Errorf("path must be absolute: %q", p)
	}
	return filepath.Clean(p), nil
}

// protected reports whether deleting or moving p would break the system. Children
// of /www, /home and /opt are fine; the top-level directories themselves are not.
func protected(p string) bool {
	switch p {
	case "/", "/bin", "/boot", "/dev", "/etc", "/home", "/lib", "/lib32", "/lib64", "/libx32",
		"/opt", "/proc", "/root", "/run", "/sbin", "/srv", "/sys", "/tmp", "/usr", "/var",
		"/www", "/www/sites", "/opt/stacks", "/etc/nginx", "/var/lib/docker":
		return true
	}
	return strings.HasPrefix(p, "/proc/") || strings.HasPrefix(p, "/sys/") ||
		strings.HasPrefix(p, "/dev/") || strings.HasPrefix(p, "/var/lib/docker/") ||
		strings.HasPrefix(p, "/var/lib/hypanel")
}

type entry struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	Dir     bool   `json:"dir"`
	Size    int64  `json:"size"`
	Mode    string `json:"mode"`
	ModTime int64  `json:"modTime"`
	Owner   string `json:"owner"`
	Group   string `json:"group"`
	Link    string `json:"link,omitempty"`
}

func describe(path string, info fs.FileInfo) entry {
	e := entry{
		Name: info.Name(), Path: path, Dir: info.IsDir(), Size: info.Size(),
		Mode: fmt.Sprintf("%04o", info.Mode().Perm()), ModTime: info.ModTime().Unix(),
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		e.Owner, e.Group = strconv.Itoa(int(st.Uid)), strconv.Itoa(int(st.Gid))
		if u, err := user.LookupId(e.Owner); err == nil {
			e.Owner = u.Username
		}
		if g, err := user.LookupGroupId(e.Group); err == nil {
			e.Group = g.Name
		}
	}
	if info.Mode()&os.ModeSymlink != 0 {
		e.Link, _ = os.Readlink(path)
		if t, err := os.Stat(path); err == nil {
			e.Dir = t.IsDir()
		}
	}
	return e
}

func (f *Files) list(w http.ResponseWriter, r *http.Request) {
	dir, err := clean(r.URL.Query().Get("path"))
	if err != nil {
		core.Fail(w, http.StatusBadRequest, "%v", err)
		return
	}
	items, err := os.ReadDir(dir)
	if err != nil {
		core.Fail(w, http.StatusBadRequest, "%v", err)
		return
	}
	out := make([]entry, 0, len(items))
	for _, it := range items {
		info, err := it.Info()
		if err != nil {
			continue // vanished while listing
		}
		out = append(out, describe(filepath.Join(dir, it.Name()), info))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Dir != out[j].Dir {
			return out[i].Dir
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	core.OK(w, map[string]any{"path": dir, "entries": out})
}

func (f *Files) read(w http.ResponseWriter, r *http.Request) {
	p, err := clean(r.URL.Query().Get("path"))
	if err != nil {
		core.Fail(w, http.StatusBadRequest, "%v", err)
		return
	}
	info, err := os.Stat(p)
	if err != nil {
		core.Fail(w, http.StatusBadRequest, "%v", err)
		return
	}
	if !info.Mode().IsRegular() {
		core.Fail(w, http.StatusBadRequest, "not a regular file")
		return
	}
	if info.Size() > maxEditSize {
		core.Fail(w, http.StatusRequestEntityTooLarge, "file is over %d MB, download it instead", maxEditSize>>20)
		return
	}
	data, err := os.ReadFile(p)
	if err != nil {
		core.Fail(w, http.StatusBadRequest, "%v", err)
		return
	}
	if !utf8.Valid(data) {
		core.Fail(w, http.StatusUnsupportedMediaType, "binary file, download it instead")
		return
	}
	core.OK(w, map[string]any{"path": p, "content": string(data), "modTime": info.ModTime().UnixMilli()})
}

// write refuses to overwrite a file that changed after the editor opened it.
// The version is in milliseconds because nanoseconds don't survive a JavaScript number: the
// browser would send back a rounded value and every save would look like a conflict.
func (f *Files) write(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path, Content string
		ModTime       int64
	}
	if !core.Read(w, r, &req) {
		return
	}
	p, err := clean(req.Path)
	if err != nil {
		core.Fail(w, http.StatusBadRequest, "%v", err)
		return
	}
	info, statErr := os.Stat(p)
	if statErr == nil && req.ModTime != 0 && info.ModTime().UnixMilli() != req.ModTime {
		core.Fail(w, http.StatusConflict, "file changed on disk since you opened it")
		return
	}
	perm := os.FileMode(0o644)
	if statErr == nil {
		perm = info.Mode().Perm()
	}
	if err := core.AtomicWrite(p, []byte(req.Content), perm); err != nil {
		core.Fail(w, http.StatusInternalServerError, "%v", err)
		return
	}
	// Rename gives the file a fresh inode owned by root; keep the original owner.
	if statErr == nil {
		if st, ok := info.Sys().(*syscall.Stat_t); ok {
			_ = os.Chown(p, int(st.Uid), int(st.Gid))
		}
	}
	f.app.Audit(r, "file.write", p)
	after, _ := os.Stat(p)
	var mod int64
	if after != nil {
		mod = after.ModTime().UnixMilli()
	}
	core.OK(w, map[string]any{"modTime": mod})
}

func (f *Files) mkdir(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path    string
		Parents bool // create missing parents, and don't mind if it exists: folder uploads
	}
	if !core.Read(w, r, &req) {
		return
	}
	p, err := clean(req.Path)
	if err != nil {
		core.Fail(w, http.StatusBadRequest, "%v", err)
		return
	}
	mk := os.Mkdir
	if req.Parents {
		mk = os.MkdirAll
	}
	if err := mk(p, 0o755); err != nil {
		core.Fail(w, http.StatusBadRequest, "%v", err)
		return
	}
	f.app.Audit(r, "file.mkdir", p)
	core.OK(w, map[string]bool{"ok": true})
}

func (f *Files) rename(w http.ResponseWriter, r *http.Request) {
	var req struct {
		From, To string
		Force    bool
	}
	if !core.Read(w, r, &req) {
		return
	}
	from, err1 := clean(req.From)
	to, err2 := clean(req.To)
	if err := errors.Join(err1, err2); err != nil {
		core.Fail(w, http.StatusBadRequest, "%v", err)
		return
	}
	if protected(from) {
		core.Fail(w, http.StatusForbidden, "%s is protected", from)
		return
	}
	if _, err := os.Lstat(to); err == nil {
		core.Fail(w, http.StatusConflict, "%s already exists", to)
		return
	}
	if !req.Force {
		if users := usedBy(r.Context(), from); len(users) > 0 {
			core.FailInUse(w, from, users)
			return
		}
	}
	if err := os.Rename(from, to); err != nil {
		// rename can't cross filesystems; mv copies instead.
		if !errors.Is(err, syscall.EXDEV) {
			core.Fail(w, http.StatusBadRequest, "%v", err)
			return
		}
		if out, err := core.Run(r.Context(), "mv", "-n", "--", from, to); err != nil {
			core.Fail(w, http.StatusBadRequest, "%s", strings.TrimSpace(out))
			return
		}
	}
	f.app.Audit(r, "file.rename", from+" -> "+to)
	core.OK(w, map[string]bool{"ok": true})
}

func (f *Files) remove(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Paths []string
		Force bool
	}
	if !core.Read(w, r, &req) {
		return
	}
	cleaned := make([]string, 0, len(req.Paths))
	// Validate everything first so a bad entry can't leave a partial delete.
	for _, raw := range req.Paths {
		p, err := clean(raw)
		if err != nil {
			core.Fail(w, http.StatusBadRequest, "%v", err)
			return
		}
		if protected(p) {
			core.Fail(w, http.StatusForbidden, "%s is protected", p)
			return
		}
		if !req.Force {
			if users := usedBy(r.Context(), p); len(users) > 0 {
				core.FailInUse(w, p, users)
				return
			}
		}
		cleaned = append(cleaned, p)
	}
	for _, p := range cleaned {
		if err := os.RemoveAll(p); err != nil {
			core.Fail(w, http.StatusInternalServerError, "%v", err)
			return
		}
		f.app.Audit(r, "file.delete", p)
	}
	core.OK(w, map[string]bool{"ok": true})
}

func (f *Files) chmod(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path, Mode string
		Recursive  bool
	}
	if !core.Read(w, r, &req) {
		return
	}
	p, err := clean(req.Path)
	if err != nil {
		core.Fail(w, http.StatusBadRequest, "%v", err)
		return
	}
	if _, err := strconv.ParseUint(req.Mode, 8, 12); err != nil {
		core.Fail(w, http.StatusBadRequest, "mode must be octal, like 644")
		return
	}
	if req.Recursive && protected(p) {
		core.Fail(w, http.StatusForbidden, "%s is protected", p)
		return
	}
	args := []string{req.Mode, "--", p}
	if req.Recursive {
		args = append([]string{"-R"}, args...)
	}
	if out, err := core.Run(r.Context(), "chmod", args...); err != nil {
		core.Fail(w, http.StatusBadRequest, "%s", strings.TrimSpace(out))
		return
	}
	f.app.Audit(r, "file.chmod", req.Mode+" "+p)
	core.OK(w, map[string]bool{"ok": true})
}

func (f *Files) chown(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path, Owner, Group string
		Recursive          bool
	}
	if !core.Read(w, r, &req) {
		return
	}
	p, err := clean(req.Path)
	if err != nil {
		core.Fail(w, http.StatusBadRequest, "%v", err)
		return
	}
	if req.Recursive && protected(p) {
		core.Fail(w, http.StatusForbidden, "%s is protected", p)
		return
	}
	spec := req.Owner
	if req.Group != "" {
		spec += ":" + req.Group
	}
	if spec == "" || strings.HasPrefix(spec, "-") {
		core.Fail(w, http.StatusBadRequest, "invalid owner")
		return
	}
	args := []string{spec, "--", p}
	if req.Recursive {
		args = append([]string{"-R"}, args...)
	}
	if out, err := core.Run(r.Context(), "chown", args...); err != nil {
		core.Fail(w, http.StatusBadRequest, "%s", strings.TrimSpace(out))
		return
	}
	f.app.Audit(r, "file.chown", spec+" "+p)
	core.OK(w, map[string]bool{"ok": true})
}

// upload streams one file part at a time, so large files use no extra memory. A
// chunk with offset>0 appends, which lets the UI send big files in resumable pieces.
func (f *Files) upload(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	p, err := clean(q.Get("path"))
	if err != nil {
		core.Fail(w, http.StatusBadRequest, "%v", err)
		return
	}
	offset, _ := strconv.ParseInt(q.Get("offset"), 10, 64)
	part := p + ".uploading"
	flags := os.O_CREATE | os.O_WRONLY | os.O_TRUNC
	if offset > 0 {
		info, err := os.Stat(part)
		if err != nil || info.Size() != offset {
			core.Fail(w, http.StatusConflict, "upload offset does not match what the server has")
			return
		}
		flags = os.O_WRONLY | os.O_APPEND
	}
	out, err := os.OpenFile(part, flags, 0o644)
	if err != nil {
		core.Fail(w, http.StatusBadRequest, "%v", err)
		return
	}
	_, copyErr := io.Copy(out, r.Body)
	closeErr := out.Close()
	if err := errors.Join(copyErr, closeErr); err != nil {
		core.Fail(w, http.StatusInternalServerError, "%v", err)
		return
	}
	if q.Get("final") == "1" {
		if err := os.Rename(part, p); err != nil {
			core.Fail(w, http.StatusInternalServerError, "%v", err)
			return
		}
		f.app.Audit(r, "file.upload", p)
	}
	core.OK(w, map[string]bool{"ok": true})
}

// download streams a file, or a directory as tar.gz.
func (f *Files) download(w http.ResponseWriter, r *http.Request) {
	p, err := clean(r.URL.Query().Get("path"))
	if err != nil {
		core.Fail(w, http.StatusBadRequest, "%v", err)
		return
	}
	info, err := os.Stat(p)
	if err != nil {
		core.Fail(w, http.StatusBadRequest, "%v", err)
		return
	}
	name := strings.ReplaceAll(filepath.Base(p), `"`, "")
	if !info.IsDir() {
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, name))
		http.ServeFile(w, r, p)
		return
	}
	f.app.Audit(r, "file.download", p)
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.tar.gz"`, name))
	gz := gzip.NewWriter(w)
	defer gz.Close()
	tw := tar.NewWriter(gz)
	defer tw.Close()
	base := filepath.Dir(p)
	// Headers are already sent, so a mid-stream error can only truncate the download.
	_ = filepath.Walk(p, func(path string, fi fs.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if r.Context().Err() != nil {
			return r.Context().Err()
		}
		var link string
		if fi.Mode()&os.ModeSymlink != 0 {
			link, _ = os.Readlink(path)
		} else if !fi.Mode().IsRegular() && !fi.IsDir() {
			return nil
		}
		hdr, err := tar.FileInfoHeader(fi, link)
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(base, path)
		hdr.Name = rel
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if !fi.Mode().IsRegular() {
			return nil
		}
		in, err := os.Open(path)
		if err != nil {
			return nil
		}
		defer in.Close()
		_, err = io.Copy(tw, in)
		return err
	})
}

func (f *Files) compress(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Paths []string
		Dest  string
	}
	if !core.Read(w, r, &req) {
		return
	}
	dest, err := clean(req.Dest)
	if err != nil || len(req.Paths) == 0 {
		core.Fail(w, http.StatusBadRequest, "choose files and an absolute archive path")
		return
	}
	kind := archiveKindOf(dest)
	if kind != kindZip && kind != kindTarGz {
		dest += ".tar.gz"
		kind = kindTarGz
	}
	if _, err := os.Lstat(dest); err == nil {
		core.Fail(w, http.StatusConflict, "%s already exists", dest)
		return
	}
	// Entries are stored relative to the folder holding the selection, wherever the
	// archive itself is written.
	paths := make([]string, 0, len(req.Paths))
	for _, raw := range req.Paths {
		p, err := clean(raw)
		if err != nil {
			core.Fail(w, http.StatusBadRequest, "%v", err)
			return
		}
		paths = append(paths, p)
	}
	base := filepath.Dir(paths[0])
	task := f.app.Tasks.Start("Compress to "+dest, func(ctx context.Context, t *core.Task) error {
		if kind == kindZip {
			return createZip(ctx, dest, base, paths, t)
		}
		args := []string{"czf", dest, "-C", base, "--"}
		for _, p := range paths {
			rel, err := filepath.Rel(base, p)
			if err != nil {
				return err
			}
			args = append(args, rel)
		}
		return t.Exec("", nil, "tar", args...)
	})
	f.app.Audit(r, "file.compress", dest)
	core.OK(w, map[string]string{"task": task.ID})
}

func (f *Files) extract(w http.ResponseWriter, r *http.Request) {
	var req struct{ Path, Dest string }
	if !core.Read(w, r, &req) {
		return
	}
	p, err1 := clean(req.Path)
	dest, err2 := clean(req.Dest)
	if err := errors.Join(err1, err2); err != nil {
		core.Fail(w, http.StatusBadRequest, "%v", err)
		return
	}
	kind := archiveKindOf(p)
	if kind == notArchive {
		core.Fail(w, http.StatusBadRequest, "unsupported archive type")
		return
	}
	task := f.app.Tasks.Start("Extract "+filepath.Base(p), func(ctx context.Context, t *core.Task) error {
		if kind == kindZip {
			return extractZip(ctx, p, dest, t)
		}
		if err := os.MkdirAll(dest, 0o755); err != nil {
			return err
		}
		// As root, tar would otherwise restore whatever owners the archive names.
		return t.Exec("", nil, "tar", "xf", p, "-C", dest, "--no-same-owner")
	})
	f.app.Audit(r, "file.extract", p+" -> "+dest)
	core.OK(w, map[string]string{"task": task.ID})
}

// size measures a folder on request, since walking a big tree is too slow to do for every
// row of every listing.
func (f *Files) size(w http.ResponseWriter, r *http.Request) {
	p, err := clean(r.URL.Query().Get("path"))
	if err != nil {
		core.Fail(w, http.StatusBadRequest, "%v", err)
		return
	}
	// --one-file-system keeps it out of mounts like /proc. du exits non-zero if a file
	// vanishes mid-walk, but still prints a usable total, so the output decides.
	out, runErr := core.Run(r.Context(), "du", "-sb", "--one-file-system", "--", p)
	lines := strings.Split(strings.TrimSpace(out), "\n")
	fields := strings.Fields(lines[len(lines)-1])
	if len(fields) > 0 {
		if n, err := strconv.ParseInt(fields[0], 10, 64); err == nil {
			core.OK(w, map[string]int64{"size": n})
			return
		}
	}
	core.Fail(w, http.StatusBadRequest, "couldn't measure folder: %v", runErr)
}
