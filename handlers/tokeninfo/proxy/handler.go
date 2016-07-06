package tokeninfoproxy

import (
	"bytes"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"

	"time"

	"github.com/afex/hystrix-go/hystrix"
	"github.com/karlseguin/ccache"
	"github.com/rcrowley/go-metrics"
	"github.com/zalando/planb-tokeninfo/handlers/tokeninfo"
	"github.com/zalando/planb-tokeninfo/options"
)

type tokenInfoProxyHandler struct {
	upstream *httputil.ReverseProxy
	cache    *ccache.Cache
	cacheTTL time.Duration
}

// NewTokenInfoProxyHandler returns an tokeninfo.Handler that proxies every Request to the server
// at the upstreamURL when the env var UPSTREAM_UUID_TOKENS is set to false. When set to a true
// value the upstreamURL will just get tokens which look like a UUID
func NewTokenInfoProxyHandler(upstreamURL *url.URL, cacheMaxSize int64, cacheTTL time.Duration) tokeninfo.Handler {
	log.Printf("Upstream tokeninfo is %s with %v cache (%d max size)", upstreamURL, cacheTTL, cacheMaxSize)
	p := httputil.NewSingleHostReverseProxy(upstreamURL)
	p.Director = hostModifier(upstreamURL, p.Director)
	cache := ccache.New(ccache.Configure().MaxSize(cacheMaxSize))
	return &tokenInfoProxyHandler{upstream: p, cache: cache, cacheTTL: cacheTTL}
}

func newResponseBuffer(w http.ResponseWriter) *responseBuffer {
	return &responseBuffer{
		ResponseWriter: w,
		Buffer:         &bytes.Buffer{},
	}
}

type responseBuffer struct {
	http.ResponseWriter
	Buffer     *bytes.Buffer
	StatusCode int
}

func (rw *responseBuffer) WriteHeader(status int) {
	rw.StatusCode = status
	rw.ResponseWriter.WriteHeader(status)
}

func (rw *responseBuffer) Write(b []byte) (int, error) {
	rw.Buffer.Write(b)
	return rw.ResponseWriter.Write(b)
}

func incCounter(key string) {
	if c, ok := metrics.DefaultRegistry.GetOrRegister(key, metrics.NewCounter).(metrics.Counter); ok {
		c.Inc(1)
	}
}

// ServeHTTP proxies the Request with an Access Token to the upstream and sends back the response
// from the upstream
func (h *tokenInfoProxyHandler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	token := tokeninfo.AccessTokenFromRequest(req)
	if token == "" {
		tokeninfo.ErrInvalidRequest.Write(w)
		return
	}
	start := time.Now()
	item := h.cache.Get(token)
	if item != nil {
		if !item.Expired() {
			incCounter("planb.tokeninfo.proxy.cache.hits")
			w.Header().Set("Content-Type", "application/json;charset=UTF-8")
			w.Header().Set("X-Cache", "HIT")
			w.Write(item.Value().([]byte))
			return
		} else {
			incCounter("planb.tokeninfo.proxy.cache.expirations")
		}
	}
	incCounter("planb.tokeninfo.proxy.cache.misses")
	err := hystrix.Do("proxy", func() error {
		upstreamStart := time.Now()
		rw := newResponseBuffer(w)
		rw.Header().Set("X-Cache", "MISS")
		h.upstream.ServeHTTP(rw, req)
		if rw.StatusCode == http.StatusOK && h.cacheTTL > 0 {
			h.cache.Set(token, rw.Buffer.Bytes(), h.cacheTTL)
		}
		upstreamTimer := metrics.DefaultRegistry.GetOrRegister("planb.tokeninfo.proxy.upstream", metrics.NewTimer).(metrics.Timer)
		upstreamTimer.UpdateSince(upstreamStart)
		return nil
	}, nil)

	if err != nil {
		status := http.StatusInternalServerError
		w.Header().Set("Content-Type", "text/plain; charset=UTF-8")
		switch err {
		case hystrix.ErrTimeout:
			{
				status = http.StatusGatewayTimeout
				incCounter("planb.tokeninfo.proxy.upstream.timeouts")
			}
		case hystrix.ErrMaxConcurrency:
			{
				status = http.StatusTooManyRequests
				incCounter("planb.tokeninfo.proxy.upstream.overruns")
			}
		case hystrix.ErrCircuitOpen:
			{
				status = http.StatusBadGateway
				incCounter("planb.tokeninfo.proxy.upstream.openrequests")
			}
		}
		w.WriteHeader(status)
		w.Write([]byte(http.StatusText(status)))
		return
	}

	t := metrics.DefaultRegistry.GetOrRegister("planb.tokeninfo.proxy", metrics.NewTimer).(metrics.Timer)
	t.UpdateSince(start)
}

func hostModifier(upstreamURL *url.URL, original func(req *http.Request)) func(req *http.Request) {
	return func(req *http.Request) {
		original(req)
		req.Host = upstreamURL.Host
		req.URL.Path = upstreamURL.Path
	}
}

func (h *tokenInfoProxyHandler) Match(req *http.Request) bool {
	if !options.AppSettings.UpstreamHasUUIDTokens {
		return true
	}
	token := tokeninfo.AccessTokenFromRequest(req)
	if token == "" {
		return false
	}
	if len(token) != 36 {
		return false
	}
	for i, c := range token {
		// 82fb6428-767d-411e-a3a1-6c6007a15df4
		// 012345678901234567890123456789012345
		//           1         2         3
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			switch {
			case c < '0':
				return false
			case c > '9' && c < 'a':
				return false
			case c > 'f':
				return false
			}
		}
	}
	return true
}
