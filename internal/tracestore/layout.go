package tracestore

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
)

// Site keys are single portable path segments. Reject Windows device names even
// on Linux so archives remain portable, rather than silently renaming sites.
func validSite(site string) bool {
	if len(site) == 0 || len(site) > 128 || site == "." || site == ".." || strings.HasSuffix(site, ".") {
		return false
	}
	for _, c := range site {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' || c == '.') {
			return false
		}
	}
	base := strings.ToUpper(strings.SplitN(site, ".", 2)[0])
	if base == "CON" || base == "PRN" || base == "AUX" || base == "NUL" {
		return false
	}
	if len(base) == 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) && base[3] >= '1' && base[3] <= '9' {
		return false
	}
	return true
}
func batchPath(b Batch, seq uint64) string {
	return filepath.Join(batchDir(b), fmt.Sprintf("%s-%020d.json", digest(b.ExecutionID), seq))
}
func isDigest(s string) bool { return len(s) == 64 && identityComponent(s) == s }

// pathIdentity recognizes both v1's bucket/task/identity/execution layout and
// v2's site/task/execution-sequence layout without trusting record contents.
func pathIdentity(path string) (taskID int, execution string, ok bool) {
	parts := strings.Split(path, string(filepath.Separator))
	if len(parts) != 3 && len(parts) != 5 {
		return 0, "", false
	}
	if !strings.HasPrefix(parts[1], "task-") {
		return 0, "", false
	}
	id, err := strconv.Atoi(strings.TrimPrefix(parts[1], "task-"))
	if err != nil || id <= 0 || parts[1] != fmt.Sprintf("task-%d", id) {
		return 0, "", false
	}
	name := parts[len(parts)-1]
	if _, err := fileSequence(name); err != nil {
		return 0, "", false
	}
	if len(parts) == 5 {
		if parts[0] != fmt.Sprintf("%02x", id&255) || !isDigest(parts[2]) || !isDigest(parts[3]) || len(strings.TrimSuffix(strings.TrimSuffix(name, ".json"), ".pending")) != 20 {
			return 0, "", false
		}
		return id, parts[3], true
	}
	if !validSite(parts[0]) || len(name) < 65 || name[64] != '-' || !isDigest(name[:64]) {
		return 0, "", false
	}
	return id, name[:64], true
}

func recordPathValid(rec record, e entry) bool {
	switch rec.Version {
	case 1:
		return e.path == filepath.Join(legacyBatchDir(rec.Batch), fmt.Sprintf("%020d.json", e.seq))
	case 2:
		return validSite(rec.Site) && e.path == batchPath(rec.Batch, e.seq)
	default:
		return false
	}
}
