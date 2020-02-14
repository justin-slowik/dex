package limiter

import (
	"net/http"
	"sync"
	"time"

	"github.com/dexidp/dex/pkg/log"
	"github.com/dexidp/dex/storage"
)

// RequestLimiter is a config struct to limit a particular request handler.
type RequestLimiter struct {
	baseRequestInterval time.Duration
	defaultExpireTime   time.Duration
	store               storage.Storage
	logger              log.Logger
	backoff             bool

	// A function to call when a request is rejected.
	onLimitReached func(w http.ResponseWriter, r *http.Request, delaySeconds int)
	sync.RWMutex
	now func() time.Time
}

// NewRequestLimit constructs the limiter
func NewRequestLimiter(requestInterval time.Duration, expireDuration time.Duration, backoff bool, storage storage.Storage, logger log.Logger, now func() time.Time) *RequestLimiter {
	lmt := &RequestLimiter{
		baseRequestInterval: requestInterval,
		defaultExpireTime:   expireDuration,
		backoff:             backoff,
		store:               storage,
		logger:              logger,
		now:                 now,
	}
	return lmt
}

// SetOnLimitReached is thread-safe way of setting after-rejection function when limit is reached.
func (l *RequestLimiter) SetOnLimitReached(fn func(w http.ResponseWriter, r *http.Request, delaySeconds int)) *RequestLimiter {
	l.Lock()
	l.onLimitReached = fn
	l.Unlock()

	return l
}

// ExecOnLimitReached is thread-safe way of executing after-rejection function when limit is reached.
func (l *RequestLimiter) ExecOnLimitReached(w http.ResponseWriter, r *http.Request, delaySeconds int) {
	l.RLock()
	defer l.RUnlock()

	fn := l.onLimitReached
	if fn != nil {
		fn(w, r, delaySeconds)
	}
}

// GetLastRequest retrieves the last request received from the given key.  If there is no request found with this key,
// a new request object will be created.
func (l *RequestLimiter) GetLastRequest(key string) (storage.RequestLimit, error) {
	l.Lock()
	defer l.Unlock()

	req, err := l.store.GetRequestLimit(key)
	if err != nil {
		if err != storage.ErrNotFound {
			return req, err
		} else {
			req = storage.RequestLimit{
				Key:      key,
				Interval: 0,
				LastSeen: l.now(),
				Expiry:   l.now().Add(l.defaultExpireTime),
			}
			err = l.store.CreateRequestLimit(req)
		}
	}
	return req, err
}

// UpdateRequest updates the storage option with the newest request.
func (l *RequestLimiter) UpdateRequest(req storage.RequestLimit) error {
	updater := func(limit storage.RequestLimit) (storage.RequestLimit, error) {
		if l.IsLimited(limit) && l.backoff {
			limit.Interval = limit.Interval + int(l.baseRequestInterval.Seconds())
		} else {
			limit.Interval = int(l.baseRequestInterval.Seconds())
		}
		limit.LastSeen = l.now()
		limit.Expiry = l.now().Add(l.defaultExpireTime)
		return limit, nil
	}
	if err := l.store.UpdateRequestLimit(req.Key, updater); err != nil {
		l.logger.Errorf("failed to update device token: %v", err)
		return err
	}
	return nil
}

// IsLimited returns true if the current time is before the LastSeen time plus the defined interval
func (l *RequestLimiter) IsLimited(r storage.RequestLimit) bool {
	d := time.Duration(r.Interval) * time.Second
	return l.now().Before(r.LastSeen.Add(d))
}
