package api

import (
	"errors"
	"io/fs"
	"net/http"
	"strings"
)

// spaHandler serves files from fsys. The aws-backup SPA uses a hash
// router, so only "/" and real asset paths need to resolve — unknown
// paths return 404 instead of index.html to avoid masking bad asset
// references.
//
// API routes are mounted before "/" in the chi router, so this only
// sees non-/api requests.
func spaHandler(fsys fs.FS) http.Handler {
	fileServer := http.FileServer(http.FS(fsys))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/")
		if p == "" {
			p = "index.html"
		}
		info, err := fs.Stat(fsys, p)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				http.NotFound(w, r)
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if info.IsDir() {
			// Don't expose directory listings.
			http.NotFound(w, r)
			return
		}
		fileServer.ServeHTTP(w, r)
	})
}
