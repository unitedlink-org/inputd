package daemon

import (
	_ "embed"
	"log/slog"
	"net"
	"net/http"
	"time"
)

//go:embed ui.html
var uiHTML []byte

func (d *Daemon) runWebUI() {
	defer d.wg.Done()
	addr := d.webAddr

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		slog.Error("web UI listen failed", "addr", addr, "err", err)
		return
	}
	defer ln.Close()
	slog.Info("web UI listening", "addr", addr)

	go func() {
		<-d.ctx.Done()
		ln.Close()
	}()

	mux := http.NewServeMux()
	d.registerRoutes(mux)
	mux.HandleFunc("GET /", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(uiHTML) //nolint:errcheck
	})

	srv := &http.Server{
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}
	if err := srv.Serve(ln); err != nil && d.ctx.Err() == nil {
		slog.Error("web UI error", "err", err)
	}
}
