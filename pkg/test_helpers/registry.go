package testhelpers

import (
	"net/http"
	"net/http/httptest"

	"github.com/google/go-containerregistry/pkg/registry"
)

func LocalRegistry(intercepts ...http.Handler) *httptest.Server {
	registry := registry.New()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, i := range intercepts {
			i.ServeHTTP(w, r)
		}
		registry.ServeHTTP(w, r)
	}))
}
