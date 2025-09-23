package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
)

type Server struct {
	httpServer *http.Server
	name       string
	port       string
}

func NewServer(name, port string, opts ...Option) (*Server, error) {
	r := NewRouter(opts...)

	s := &Server{name: name, port: port}
	s.httpServer = &http.Server{
		Addr:    ":" + port,
		Handler: r.mux,
	}

	return s, nil
}

func (s *Server) Start() {
	slog.Info(s.name+" server started", "port", s.port)
	go func() {
		err := s.httpServer.ListenAndServe()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("server stopped", "error", err)
			os.Exit(1)
		}
	}()
}

func (s *Server) Shutdown(ctx context.Context) error {
	slog.Info(s.name + " server shutting down")
	return s.httpServer.Shutdown(ctx)
}

