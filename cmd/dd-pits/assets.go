package main

import (
	"embed"
	"io/fs"
	"net/http"
)

// brandAssets are the images the local pages use: the app icon, favicons and
// the artwork behind the waiting screen and setup pages. Embedded so the pages
// look the same at a venue with no internet as anywhere else.
//
//go:embed assets
var brandAssets embed.FS

// assetHandler serves /assets/… from the embedded files. The files only change
// with a new build, so browsers may keep them for a day.
func assetHandler() http.Handler {
	sub, err := fs.Sub(brandAssets, "assets")
	if err != nil {
		panic(err)
	}
	files := http.StripPrefix("/assets/", http.FileServer(http.FS(sub)))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=86400")
		files.ServeHTTP(w, r)
	})
}

// headBrand is the favicon and mobile meta every local page shares.
const headBrand = `<meta name="viewport" content="width=device-width,initial-scale=1,viewport-fit=cover">
<meta name="theme-color" content="#07080a">
<link rel="icon" type="image/png" sizes="32x32" href="/assets/favicon-32.png">
<link rel="icon" type="image/png" sizes="64x64" href="/assets/favicon-64.png">
<link rel="apple-touch-icon" href="/assets/apple-touch-icon.png">`
