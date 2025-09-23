package server

import (
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
)

type Router struct {
	mux *chi.Mux
}

type Option func(*Router)

func NewRouter(opts ...Option) *Router {
	r := &Router{
		mux: chi.NewRouter(),
	}

	for _, opt := range opts {
		opt(r)
	}

	r.mux.NotFound(func(w http.ResponseWriter, r *http.Request) {
		slog.ErrorContext(r.Context(), "Route not found", "path", r.URL.Path)
		w.WriteHeader(404)
		w.Write([]byte("404 page not found"))
	})

	return r
}

func WithGet(path string, handler http.HandlerFunc) Option {
	return func(r *Router) {
		r.mux.Get(path, handler)
	}
}

func WithPost(path string, handler http.HandlerFunc) Option {
	return func(r *Router) {
		r.mux.Post(path, handler)
	}
}

func WithHandle(path string, handler http.HandlerFunc) Option {
	return func(r *Router) {
		r.mux.Handle(path, handler)
	}
}

func WithMiddleware(middleware func(http.Handler) http.Handler) Option {
	return func(r *Router) {
		r.mux.Use(middleware)
	}
}
