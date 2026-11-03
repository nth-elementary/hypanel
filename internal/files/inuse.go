package files

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"hypanel/internal/core"
)

const nginxSites = "/etc/nginx/sites"

// within reports whether inner is outer or something under it.
func within(inner, outer string) bool {
	return inner == outer || strings.HasPrefix(inner, strings.TrimSuffix(outer, "/")+"/")
}

// referencedIn reports whether an nginx config names path, or something under it.
func referencedIn(conf, path string) bool {
	for _, line := range strings.Split(conf, "\n") {
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		for _, field := range strings.Fields(strings.ReplaceAll(line, ";", " ")) {
			if strings.HasPrefix(field, "/") && within(field, path) {
				return true
			}
		}
	}
	return false
}

// usedBy names the websites and containers that would break if path moved or vanished.
// A container counts only if a mount's source is inside path: every PHP container mounts
// all of /www, and that shouldn't make each site's files look "in use".
func usedBy(ctx context.Context, path string) []string {
	users := []string{}
	confs, _ := filepath.Glob(filepath.Join(nginxSites, "*.conf"))
	for _, f := range confs {
		if data, err := os.ReadFile(f); err == nil && referencedIn(string(data), path) {
			users = append(users, "website "+strings.TrimSuffix(filepath.Base(f), ".conf"))
		}
	}
	// A docker failure must not block file management, so errors mean "nothing found".
	ids, err := core.Run(ctx, "docker", "ps", "-aq")
	if err != nil || strings.TrimSpace(ids) == "" {
		return users
	}
	out, err := core.Run(ctx, "docker", append([]string{"inspect"}, strings.Fields(ids)...)...)
	if err != nil {
		return users
	}
	var containers []struct {
		Name   string
		Mounts []struct{ Source string }
		Config struct{ Labels map[string]string }
	}
	if json.Unmarshal([]byte(out), &containers) != nil {
		return users
	}
	for _, c := range containers {
		hit := false
		for _, m := range c.Mounts {
			hit = hit || within(m.Source, path)
		}
		if dir := c.Config.Labels["com.docker.compose.project.working_dir"]; dir != "" {
			hit = hit || within(dir, path)
		}
		if hit {
			users = append(users, "container "+strings.TrimPrefix(c.Name, "/"))
		}
	}
	return users
}
