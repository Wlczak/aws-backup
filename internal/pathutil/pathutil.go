// Package pathutil holds canonical path helpers shared by the engine,
// API, and storage layer. Centralised so that prefix matching, S3 list
// prefix normalisation, and key joining behave identically everywhere
// — historically this was duplicated across packages and drifted (#44).
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
// A trailing "/" on prefix is tolerated so "photos/" behaves like "photos".
func HasPrefixPath(full, prefix string) bool {
	prefix = strings.TrimSuffix(prefix, "/")
	if prefix == "" {
		return true
	}
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

// NormalizeS3ListPrefix returns prefix with a trailing "/" appended so that
// Storage.List does not match sibling keys (e.g. prefix "backups" matching
// "backups2/foo"). An empty prefix is returned unchanged so List("") still
// enumerates the whole bucket.
func NormalizeS3ListPrefix(prefix string) string {
	if prefix == "" || strings.HasSuffix(prefix, "/") {
		return prefix
	}
	return prefix + "/"
}
