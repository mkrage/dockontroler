package manager

import (
	"bufio"
	"context"
	"os"
	"regexp"
	"strings"

	"github.com/mkrage/dockontroler/internal/docker"
)

// Docker container ids are 64 lowercase hex characters.
var (
	fullIDPattern  = regexp.MustCompile(`[0-9a-f]{64}`)
	shortIDPattern = regexp.MustCompile(`^[0-9a-f]{12}$`)
)

// DetectSelfID works out which container Dockontroler is running in, so that
// stopping or recreating itself can be refused.
//
// configured wins if set. Otherwise several sources are tried, because none of
// them works everywhere: /proc/self/mountinfo covers cgroup v2, /proc/self/cgroup
// covers cgroup v1, and the hostname covers the rest.
//
// An empty result is not an error — Dockontroler also runs directly on a
// developer machine. The caller decides how loudly to complain.
func DetectSelfID(ctx context.Context, client *docker.Client, configured string) string {
	if configured != "" {
		if resolved := verifySelf(ctx, client, configured); resolved != "" {
			return resolved
		}
		// Wrong value is worth reporting, but not worth refusing to start over.
		return ""
	}

	for _, candidate := range selfCandidates() {
		if resolved := verifySelf(ctx, client, candidate); resolved != "" {
			return resolved
		}
	}
	return ""
}

// verifySelf confirms a candidate id or name actually exists and returns its
// full container id.
func verifySelf(ctx context.Context, client *docker.Client, candidate string) string {
	inspected, err := client.InspectContainer(ctx, candidate)
	if err != nil {
		return ""
	}
	return inspected.ID
}

// selfCandidates collects possible container ids for this process, best source
// first.
func selfCandidates() []string {
	var candidates []string
	seen := map[string]bool{}
	add := func(id string) {
		if id != "" && !seen[id] {
			seen[id] = true
			candidates = append(candidates, id)
		}
	}

	// Under cgroup v2 the cgroup file is just "0::/", but the bind mounts Docker
	// sets up for /etc/hostname and friends still carry the id:
	//   .../var/lib/docker/containers/<id>/hostname /etc/hostname ...
	for _, id := range idsFromFile("/proc/self/mountinfo", `/containers/([0-9a-f]{64})/`) {
		add(id)
	}

	// Under cgroup v1 the id appears in the cgroup path itself.
	for _, id := range idsFromFile("/proc/self/cgroup", "") {
		add(id)
	}

	// Last resort: Docker sets the hostname to the short container id unless the
	// user overrode it. The hex check matters — without it a custom hostname
	// could resolve to some unrelated container that happens to share the name.
	if raw, err := os.ReadFile("/etc/hostname"); err == nil {
		hostname := strings.TrimSpace(string(raw))
		if shortIDPattern.MatchString(hostname) {
			add(hostname)
		}
	}
	return candidates
}

// idsFromFile scans a file for container ids. When pattern is set it must have
// exactly one capturing group holding the id; otherwise any 64-hex run matches.
func idsFromFile(path, pattern string) []string {
	file, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer file.Close()

	matcher := fullIDPattern
	captured := false
	if pattern != "" {
		compiled, err := regexp.Compile(pattern)
		if err != nil {
			return nil
		}
		matcher = compiled
		captured = true
	}

	var ids []string
	scanner := bufio.NewScanner(file)
	// mountinfo lines can be long; the default 64 KiB token limit is plenty but
	// the default 4 KiB buffer is not.
	scanner.Buffer(make([]byte, 0, 8<<10), 64<<10)
	for scanner.Scan() {
		match := matcher.FindStringSubmatch(scanner.Text())
		if match == nil {
			continue
		}
		if captured && len(match) > 1 {
			ids = append(ids, match[1])
		} else {
			ids = append(ids, match[0])
		}
	}
	return ids
}
