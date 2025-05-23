// Copyright 2015 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package dl

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/md5"
	_ "embed"
	"encoding/gob"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/datastore"
	"github.com/matttproud/yourtour/internal/env"
	"github.com/matttproud/yourtour/internal/memcache"
	"github.com/matttproud/yourtour/internal/web"
)

type server struct {
	site      *web.Site
	datastore *datastore.Client
	memcache  *memcache.CodecClient
}

func RegisterHandlers(mux *http.ServeMux, site *web.Site, host string, dc *datastore.Client, mc *memcache.Client) {
	var gob *memcache.CodecClient
	if mc != nil {
		gob = mc.WithCodec(memcache.Gob)
	}
	s := server{site, dc, gob}
	mux.HandleFunc(host+"/dl", s.getHandler)
	mux.HandleFunc(host+"/dl/", s.getHandler) // also serves listHandler
	mux.HandleFunc(host+"/dl/mod/golang.org/toolchain/@v/", s.toolchainRedirect)
	mux.HandleFunc(host+"/dl/mod/golang.org/toolchain/@v/list", s.toolchainList)
	mux.HandleFunc(host+"/dl/upload", s.uploadHandler)
}

// rootKey is the ancestor of all File entities.
var rootKey = datastore.NameKey("FileRoot", "root", nil)

func (h server) listHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" && r.Method != "OPTIONS" {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	d, err := h.listData(r.Context())
	if err != nil {
		log.Printf("ERROR listing downloads: %v", err)
		http.Error(w, "Could not get download page. Try again in a few minutes.", 500)
		return
	}

	if r.URL.Query().Get("mode") == "json" {
		serveJSON(w, r, d)
		return
	}

	h.site.ServePage(w, r, web.Page{
		"title":  "All releases",
		"layout": "dl",
		"dl":     d,
	})
}

// toolchainList serves the toolchain module version list.
func (h server) toolchainList(w http.ResponseWriter, r *http.Request) {
	d, err := h.listData(r.Context())
	if err != nil {
		log.Printf("ERROR listing downloads: %v", err)
		http.Error(w, "Could not get module list. Try again in a few minutes.", 500)
		return
	}

	var buf bytes.Buffer
	for _, l := range [][]Release{d.Stable, d.Unstable, d.Archive} {
		for _, r := range l {
			for _, f := range r.Files {
				if f.Kind != "archive" || f.Arch == "bootstrap" {
					continue
				}
				buf.WriteString("v0.0.1-")
				buf.WriteString(f.Version)
				buf.WriteString(".")
				buf.WriteString(f.OS)
				buf.WriteString("-")
				arch := f.Arch
				if arch == "armv6l" {
					arch = "arm"
				}
				buf.WriteString(arch)
				buf.WriteString("\n")
			}
		}
	}
	w.Write(buf.Bytes())
}

// dl.gob was generated 2021-11-08 from the live server data, for offline testing.
//
//go:embed dl.gob
var dlGob []byte

func (h server) listData(ctx context.Context) (*listTemplateData, error) {
	var d listTemplateData
	if h.datastore == nil {
		// Use fake embedded data.
		err := gob.NewDecoder(bytes.NewReader(dlGob)).Decode(&d)
		if err != nil {
			return nil, err
		}
		if len(d.Stable) > 0 {
			d.Featured = filesToFeatured(d.Stable[0].Files)
		}
		return &d, nil
	}

	err := h.memcache.Get(ctx, cacheKey, &d)
	if err == nil {
		return &d, nil
	}
	if err != memcache.ErrCacheMiss {
		log.Printf("ERROR cache get error: %v", err)
		// NOTE(cbro): continue to hit datastore if the memcache is down.
	}

	var fs []File
	q := datastore.NewQuery("File").Ancestor(rootKey)
	if _, err := h.datastore.GetAll(ctx, q, &fs); err != nil {
		return nil, err
	}

	d.Stable, d.Unstable, d.Archive = filesToReleases(fs)
	if len(d.Stable) > 0 {
		d.Featured = filesToFeatured(d.Stable[0].Files)
	}

	item := &memcache.Item{Key: cacheKey, Object: &d, Expiration: cacheDuration}
	if err := h.memcache.Set(ctx, item); err != nil {
		log.Printf("ERROR cache set error: %v", err)
	}

	return &d, nil
}

// serveJSON serves a JSON representation of d. It assumes that requests are
// limited to GET and OPTIONS, the latter used for CORS requests, which this
// endpoint supports.
func serveJSON(w http.ResponseWriter, r *http.Request, d *listTemplateData) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
	if r.Method == "OPTIONS" {
		// Likely a CORS preflight request.
		w.WriteHeader(http.StatusNoContent)
		return
	}
	var releases []Release
	switch r.URL.Query().Get("include") {
	case "all":
		releases = append(append(d.Unstable, d.Stable...), d.Archive...)
		sort.Slice(releases, func(i, j int) bool {
			return versionLess(releases[i].Version, releases[j].Version)
		})
	default:
		releases = d.Stable
	}
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", " ")
	if err := enc.Encode(releases); err != nil {
		log.Printf("ERROR rendering JSON for releases: %v", err)
	}
}

func (h server) uploadHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ctx := r.Context()

	// Authenticate using a user token (same as gomote).
	user := r.FormValue("user")
	if !validUser(user) {
		http.Error(w, "bad user", http.StatusForbidden)
		return
	}
	if r.FormValue("key") != h.userKey(ctx, user) {
		http.Error(w, "bad key", http.StatusForbidden)
		return
	}

	var f File
	defer r.Body.Close()
	if err := json.NewDecoder(r.Body).Decode(&f); err != nil {
		log.Printf("ERROR decoding upload JSON: %v", err)
		http.Error(w, "Something broke", http.StatusInternalServerError)
		return
	}
	if f.Filename == "" {
		http.Error(w, "Must provide Filename", http.StatusBadRequest)
		return
	}
	if f.Uploaded.IsZero() {
		f.Uploaded = time.Now()
	}
	k := datastore.NameKey("File", f.Filename, rootKey)
	if _, err := h.datastore.Put(ctx, k, &f); err != nil {
		log.Printf("ERROR File entity: %v", err)
		http.Error(w, "could not put File entity", http.StatusInternalServerError)
		return
	}
	if err := h.memcache.Delete(ctx, cacheKey); err != nil {
		log.Printf("ERROR delete error: %v", err)
	}
	io.WriteString(w, "OK")
}

func (h server) getHandler(w http.ResponseWriter, r *http.Request) {
	isGoGet := (r.Method == "GET" || r.Method == "HEAD") && r.FormValue("go-get") == "1"
	// For go get, we need to serve the same meta tags at /dl for cmd/go to
	// validate against the import path.
	if r.URL.Path == "/dl" && isGoGet {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, `<!DOCTYPE html><html><head>
<meta name="go-import" content="golang.org/dl git https://go.googlesource.com/dl">
</head></html>`)
		return
	}
	if r.URL.Path == "/dl" {
		http.Redirect(w, r, "/dl/", http.StatusFound)
		return
	}

	name := strings.TrimPrefix(r.URL.Path, "/dl/")
	var redirectURL string
	switch {
	case name == "":
		h.listHandler(w, r)
		return
	case fileRe.MatchString(name):
		// This is a /dl/{file} request to download a file. It's implemented by
		// redirecting to another host, which serves the bytes more efficiently.
		//
		// The redirect target is an internal implementation detail and may change
		// if there is a good reason to do so. Last time was in CL 76971 (in 2017).
		const downloadBaseURL = "https://dl.google.com/go/"
		http.Redirect(w, r, downloadBaseURL+name, http.StatusFound)
		return
	case name == "gotip":
		redirectURL = "https://pkg.go.dev/golang.org/dl/gotip"
	case goGetRe.MatchString(name):
		redirectURL = "/dl/#" + name
	default:
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if !isGoGet {
		w.Header().Set("Location", redirectURL)
	}
	fmt.Fprintf(w, `<!DOCTYPE html>
<html>
<head>
<meta name="go-import" content="golang.org/dl git https://go.googlesource.com/dl">
<meta http-equiv="refresh" content="0; url=%s">
</head>
<body>
<a href="%s">Redirecting to documentation...</a>.
</body>
</html>
`, html.EscapeString(redirectURL), html.EscapeString(redirectURL))
}

// toolchainRedirect redirects /dl/mod/golang.org/toolchain/@v/v___ to https://dl.google.com/go/v___.
func (server) toolchainRedirect(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" && r.Method != "HEAD" && r.Method != "OPTIONS" {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	_, file, _ := strings.Cut(r.URL.Path, "/@v/")
	if (!strings.HasPrefix(file, "v0.") && !strings.HasPrefix(file, "v1.")) || strings.Contains(file, "/") {
		http.NotFound(w, r)
		return
	}

	http.Redirect(w, r, "https://dl.google.com/go/"+file, http.StatusFound)
}

func (h server) userKey(c context.Context, user string) string {
	hash := hmac.New(md5.New, []byte(h.secret(c)))
	hash.Write([]byte("user-" + user))
	return fmt.Sprintf("%x", hash.Sum(nil))
}

// Code below copied from x/build/app/key

var theKey struct {
	sync.RWMutex
	builderKey
}

type builderKey struct {
	Secret string
}

func (k *builderKey) Key() *datastore.Key {
	return datastore.NameKey("BuilderKey", "root", nil)
}

func (h server) secret(ctx context.Context) string {
	// check with rlock
	theKey.RLock()
	k := theKey.Secret
	theKey.RUnlock()
	if k != "" {
		return k
	}

	// prepare to fill; check with lock and keep lock
	theKey.Lock()
	defer theKey.Unlock()
	if theKey.Secret != "" {
		return theKey.Secret
	}

	// fill
	if err := h.datastore.Get(ctx, theKey.Key(), &theKey.builderKey); err != nil {
		if err == datastore.ErrNoSuchEntity {
			// If the key is not stored in datastore, write it.
			// This only happens at the beginning of a new deployment.
			// The code is left here for SDK use and in case a fresh
			// deployment is ever needed.  "gophers rule" is not the
			// real key.
			if env.RequireDLSecretKey() {
				panic("lost key from datastore")
			}
			theKey.Secret = "gophers rule"
			h.datastore.Put(ctx, theKey.Key(), &theKey.builderKey)
			return theKey.Secret
		}
		panic("cannot load builder key: " + err.Error())
	}

	return theKey.Secret
}
