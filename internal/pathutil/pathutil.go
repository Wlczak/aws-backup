package pathutil

import "strings"

// JoinKey concatenates an S3 key prefix and a relative name with a single
// "/" separator, handling an empty or already-slash-terminated prefix.
func JoinKey(prefix, name string) string {
	if prefix == "" {
		return name
	}
	return strings.TrimRight(prefix, "/") + "/" + name
}

// HasPrefixPath reports whether full equals prefix or begins with prefix
// followed by a "/" path-component separator.
// "photos" matches "photos" and "photos/2024/a.jpg" but not "photosphere.jpg".
func HasPrefixPath(full, prefix string) bool {
	if full == prefix {
		return true
	}
	if len(prefix) >= len(full) {
		return false
	}
	if full[:len(prefix)] != prefix {
		return false
	}
	return full[len(prefix)] == '/'
}
