package a

import (
	stdhttp "net/http"
)

type router struct{}

func (router) HandleFunc(string, func(stdhttp.ResponseWriter, *stdhttp.Request)) {}
func (router) Handle(string, stdhttp.Handler)                                    {}

func register(mux *stdhttp.ServeMux, typed router, handler stdhttp.Handler) {
	mux.HandleFunc("/raw", func(w stdhttp.ResponseWriter, r *stdhttp.Request) {}) // want "direct net/http route registration"
	mux.Handle("/raw2", handler)                                                  // want "direct net/http route registration"
	stdhttp.HandleFunc("/default", nil)                                           // want "direct net/http route registration"
	stdhttp.Handle("/default2", handler)                                          // want "direct net/http route registration"

	typed.HandleFunc("/typed", nil)
	typed.Handle("/typed2", handler)
	_ = stdhttp.NewServeMux()
}
