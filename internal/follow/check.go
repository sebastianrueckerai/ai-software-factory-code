package follow

import (
	"errors"
	"fmt"
)

// Check is the infra repository's check before a merge: every marker is
// well formed and in one part, no digest lacks its marker, and each marked
// value that differs from base's (or is new) is exactly its list line. A
// part may stay behind; the count of references behind comes back. No
// network: a file-derived image's digest and a copy's bytes are left to
// the store's own test, so the check proves the tag and the copy's sha256.
// A nil base compares nothing.
func Check(files, base map[string][]byte, parts []Part) (int, error) {
	res, err := Run(files, parts, Offline{})
	if err != nil {
		return 0, err
	}
	var baseValues map[string]string
	if base != nil {
		baseValues = Values(base)
	}
	var errs []error
	behind := 0
	for _, r := range res {
		behind += r.Behind()
		for _, m := range r.Markers {
			if b, ok := baseValues[m.ID]; base != nil && (!ok || b != m.Have) && m.Have != m.Want {
				errs = append(errs, fmt.Errorf("%s:%d: # pin %s: changed from the base, and is not the list's: it holds %q, the list says %q. Write the list's value, or put the base's value back", m.File, m.Line, m.Key, m.Have, m.Want))
			}
		}
	}
	return behind, errors.Join(errs...)
}
