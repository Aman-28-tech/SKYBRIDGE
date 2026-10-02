// Local console CORS: browser (http://localhost:3000, http://127.0.0.1:3000)
// to control-plane GET/HEAD/OPTIONS integration only. No wildcard origin,
// no credentials, no policy/ownership/cutover behavior change.
package main

import "net/http"

// Explicit local-console allowlist (never "*").
var corsAllowedOrigins = map[string]struct{}{
	"http://localhost:3000": {},
	"http://127.0.0.1:3000": {},
}

func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if _, ok := corsAllowedOrigins[origin]; ok {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Add("Vary", "Origin")
		}
		if r.Method == http.MethodOptions {
			w.Header().Set("Access-Control-Allow-Methods", "GET, HEAD, OPTIONS")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
