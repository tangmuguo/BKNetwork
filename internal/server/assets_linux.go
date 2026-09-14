package server

import (
	webui "bknetwork/web"
	"net/http"
)

func platformWebHandler() (http.Handler, bool) {
	files := http.FileServer(http.FS(webui.Assets))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store, max-age=0")
		files.ServeHTTP(w, r)
	}), true
}
