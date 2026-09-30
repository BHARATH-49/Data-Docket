package ui

import (
	"embed"
	"html/template"
	"io/fs"
	"net/http"
)

//go:embed templates/*.html assets/*
var files embed.FS

var pages = template.Must(template.ParseFS(files, "templates/*.html"))

func Render(w http.ResponseWriter, page string, data any) error {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	return pages.ExecuteTemplate(w, page, data)
}

func Assets() http.Handler {
	root, err := fs.Sub(files, "assets")
	if err != nil {
		panic(err)
	}
	return http.FileServer(http.FS(root))
}
