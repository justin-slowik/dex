package server

import (
	"time"

	"github.com/dexidp/dex/server/limiter"
	"github.com/dexidp/dex/storage"

	"net/http"
	"strings"
)

// setResponseHeaders configures X-Rate-Limit-Limit and X-Rate-Limit-Duration
func setResponseHeaders(lmt *limiter.RequestLimiter, w http.ResponseWriter, r *http.Request) {
	w.Header().Add("X-Rate-Limit-Duration", "1")
	w.Header().Add("X-Rate-Limit-Request-Forwarded-For", r.Header.Get("X-Forwarded-For"))
	w.Header().Add("X-Rate-Limit-Request-Remote-Addr", r.RemoteAddr)
}

// NewLimiter is a convenience function to limiter.New.
func (s *Server) NewLimiter(requestInterval time.Duration, expireInterval time.Duration, backoff bool) *limiter.RequestLimiter {
	return limiter.NewRequestLimiter(requestInterval, expireInterval, backoff, s.storage, s.logger, s.now)
}

// LimitByRequest generates a key based on the request IP, path, and method, and gets the RequestLimit object if one
// exists, or generates a new one
func LimitByRequest(lmt *limiter.RequestLimiter, w http.ResponseWriter, r *http.Request) (storage.RequestLimit, error) {
	setResponseHeaders(lmt, w, r)
	remoteIP := GetIP(r)
	path := r.URL.Path
	method := r.Method

	key := []string{method, remoteIP, path}
	return lmt.GetLastRequest(strings.Join(key, "-"))
}

// LimitHandler is a middleware that performs rate-limiting given http.Handler struct.
func (s *Server) RequestLimitHandler(lmt *limiter.RequestLimiter, next http.Handler) http.Handler {
	middle := func(w http.ResponseWriter, r *http.Request) {
		req, err := LimitByRequest(lmt, w, r)
		if err != nil {
			s.logger.Errorf("Unexpected error getting request limit: %v", err)
			s.renderError(r, w, http.StatusInternalServerError, "")
			return
		}
		//Update the Request Log
		if err = lmt.UpdateRequest(req); err != nil {
			s.logger.Errorf("Unexpected error updating request limit: %v", err)
			s.renderError(r, w, http.StatusInternalServerError, "")
			return
		}
		if lmt.IsLimited(req) {
			lmt.ExecOnLimitReached(w, r, req.Interval)
			return
		}
		// There's no rate-limit error, serve the next handler.
		next.ServeHTTP(w, r)
	}
	return http.HandlerFunc(middle)
}

// LimitFuncHandler is a middleware that performs rate-limiting given request handler function.
func (s *Server) RequestLimitFuncHandler(lmt *limiter.RequestLimiter, nextFunc func(http.ResponseWriter, *http.Request)) http.Handler {
	return s.RequestLimitHandler(lmt, http.HandlerFunc(nextFunc))
}
