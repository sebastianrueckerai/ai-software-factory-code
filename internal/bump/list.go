package bump

import (
	"strings"
)

// splitImage splits name:tag@sha256:<hex>.
func splitImage(ref string) (name, tag, digest string, ok bool) {
	image, digest, ok := strings.Cut(ref, "@")
	if !ok {
		return "", "", "", false
	}
	at := strings.LastIndex(image, ":")
	if at < 0 || strings.Contains(image[at:], "/") {
		return "", "", "", false
	}
	return image[:at], image[at+1:], digest, true
}

// rewriteLines moves each line of list that moved holds, old to new, and
// keeps every other line as it is.
func rewriteLines(list string, moved map[string]string) string {
	lines := strings.Split(list, "\n")
	for i, line := range lines {
		if now, ok := moved[line]; ok {
			lines[i] = now
		}
	}
	return strings.Join(lines, "\n")
}

// rewriteImages moves each pin line of an upstream-images.txt whose
// reference moved holds, old to new, keeping its words, and an onto base
// with its image.
func rewriteImages(list string, moved map[string]string) string {
	bases := map[string]string{}
	for old, now := range moved {
		if o, _, ok := strings.Cut(old, "@"); ok && !strings.HasPrefix(old, "https://") {
			bases[o], _, _ = strings.Cut(now, "@")
		}
	}
	lines := strings.Split(list, "\n")
	for i, line := range lines {
		f := strings.Fields(line)
		if len(f) == 0 || strings.HasPrefix(f[0], "#") {
			continue
		}
		if now, ok := moved[f[0]]; ok {
			f[0] = now
		}
		if len(f) > 2 && f[1] == "onto" && bases[f[2]] != "" {
			f[2] = bases[f[2]]
		}
		lines[i] = strings.Join(f, " ")
	}
	return strings.Join(lines, "\n")
}
