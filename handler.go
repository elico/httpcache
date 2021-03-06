package httpcache

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"
)

const (
	CacheHeader     = "X-Cache"
	ProxyDateHeader = "Proxy-Date"
)

var Writes sync.WaitGroup

var storeable = map[int]bool{
	http.StatusOK:                   true,
	http.StatusFound:                true,
	http.StatusNonAuthoritativeInfo: true,
	http.StatusMultipleChoices:      true,
	http.StatusMovedPermanently:     true,
	http.StatusGone:                 true,
	http.StatusNotFound:             true,
}

var cacheableByDefault = map[int]bool{
	http.StatusOK:                   true,
	http.StatusFound:                true,
	http.StatusNotModified:          true,
	http.StatusNonAuthoritativeInfo: true,
	http.StatusMultipleChoices:      true,
	http.StatusMovedPermanently:     true,
	http.StatusGone:                 true,
	http.StatusPartialContent:       true,
}

type Handler struct {
	Shared    bool
	upstream  http.Handler
	validator *Validator
	cache     Cache
	storeIdUrl *url.URL
}

func NewHandler(cache Cache, upstream http.Handler, storeIdUrl string) *Handler {
	u , err := url.Parse(storeIdUrl)
	if err != nil {
		u, _ = url.Parse("http://dummy/")
	}
	return &Handler{
		upstream:  upstream,
		cache:     cache,
		validator: &Validator{upstream},
		Shared:    false,
		storeIdUrl:	u,
	}
}

func (h *Handler) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	cReq, err := newCacheRequest(r, h.storeIdUrl)

	if err != nil {
		http.Error(rw, "invalid request: "+err.Error(),
			http.StatusBadRequest)
		return
	}

//	debugf("Request headers details after a while1", r.Header)

	if !cReq.isCacheable() {
		debugf("request not cacheable")
		rw.Header().Set(CacheHeader, "SKIP")
		h.pipeUpstream(rw, cReq)
		return
	}
//	debugf("Request headers details after a while2", r.Header)

	res, err := h.lookup(cReq)
	switch {
	case  err != nil && err == ErrNotFoundInCache:
		;;
	case err != nil && err == ErrFoundWithZeroInCache:
		;;
	case err == nil:
		debugf("nil err", err)
		;;
	default:
		http.Error(rw, "lookup error: "+err.Error(),
			http.StatusInternalServerError)
		return
	}
//	debugf("Request headers details after a while3", r.Header)

	cacheType := "private"
	if h.Shared {
		cacheType = "shared"
	}

	if err == ErrNotFoundInCache {
		if cReq.CacheControl.Has("only-if-cached") {
			http.Error(rw, "key not in cache",
				http.StatusGatewayTimeout)
			return
		}
		debugf("%s %s not in %s cache", r.Method, r.URL.String(), cacheType)
		h.passUpstream(rw, cReq)
		return
	} else {
		debugf("%s %s found in %s cache", r.Method, r.URL.String(), cacheType)
	}

	if h.needsValidation(res, cReq) {
		if cReq.CacheControl.Has("only-if-cached") {
			http.Error(rw, "key was in cache, but required validation",
				http.StatusGatewayTimeout)
			return
		}

		debugf("validating cached response")
		if h.validator.Validate(r, res) {
			debugf("response is valid")
			h.cache.Freshen(res, cReq.Key.String())
		} else {
			debugf("response is changed")
			h.passUpstream(rw, cReq)
			return
		}
	}

	debugf("serving from cache")
	res.Header().Set(CacheHeader, "HIT")
	h.serveResource(res, rw, cReq)

	if err := res.Close(); err != nil {
		errorf("Error closing resource: %s", err.Error())
	}
}

// freshness returns the duration that a requested resource will be fresh for
func (h *Handler) freshness(res *Resource, r *cacheRequest) (time.Duration, error) {
	maxAge, err := res.MaxAge(h.Shared)
	if err != nil {
		return time.Duration(0), err
	}

	if r.CacheControl.Has("max-age") {
		reqMaxAge, err := r.CacheControl.Duration("max-age")
		if err != nil {
			return time.Duration(0), err
		}

		if reqMaxAge < maxAge {
			debugf("using request max-age of %s", reqMaxAge.String())
			maxAge = reqMaxAge
		}
	}

	age, err := res.Age()
	if err != nil {
		return time.Duration(0), err
	}

	if res.IsStale() {
		return time.Duration(0), nil
	}

	if hFresh := res.HeuristicFreshness(); hFresh > maxAge {
		debugf("using heuristic freshness of %q", hFresh)
		maxAge = hFresh
	}

	return maxAge - age, nil
}

func (h *Handler) needsValidation(res *Resource, r *cacheRequest) bool {
	if res.MustValidate(h.Shared) {
		return true
	}

	freshness, err := h.freshness(res, r)
	if err != nil {
		debugf("error calculating freshness: %s", err.Error())
		return true
	}

	if r.CacheControl.Has("min-fresh") {
		reqMinFresh, err := r.CacheControl.Duration("min-fresh")
		if err != nil {
			debugf("error parsing request min-fresh: %s", err.Error())
			return true
		}

		if freshness < reqMinFresh {
			debugf("resource is fresh, but won't satisfy min-fresh of %s", reqMinFresh)
			return true
		}
	}

	debugf("resource has a freshness of %s", freshness)

	if freshness <= 0 && r.CacheControl.Has("max-stale") {
		if len(r.CacheControl["max-stale"]) == 0 {
			debugf("resource is stale, but client sent max-stale")
			return false
		} else if maxStale, _ := r.CacheControl.Duration("max-stale"); maxStale >= (freshness * -1) {
			log.Printf("resource is stale, but within allowed max-stale period of %s", maxStale)
			return false
		}
	}

	return freshness <= 0
}

// pipeUpstream makes the request via the upstream handler, the response is not stored or modified
func (h *Handler) pipeUpstream(w http.ResponseWriter, r *cacheRequest) {
	rw := newResponseBuffer(w)

	debugf("piping request upstream")
	h.upstream.ServeHTTP(rw, r.Request)

	if r.Method == "HEAD" || r.isStateChanging() {
		res := rw.Resource()
		defer res.Close()

		if r.Method == "HEAD" {
			h.cache.Freshen(res, r.Key.ForMethod("GET").String())
		} else if res.IsNonErrorStatus() {
			h.invalidateResource(res, r)
		}
	}
}

// passUpstream makes the request via the upstream handler and stores the result
func (h *Handler) passUpstream(w http.ResponseWriter, r *cacheRequest) {
	rw := newResponseBuffer(w)

	t := Clock()
	debugf("passing request upstream")
	rw.Header().Set(CacheHeader, "MISS")
	h.upstream.ServeHTTP(rw, r.Request)
	res := rw.Resource()
	debugf("upstream responded in %s", Clock().Sub(t).String())

	if !h.isCacheable(res, r) {
		debugf("resource is uncacheable")
		rw.Header().Set(CacheHeader, "SKIP")
		return
	}

	if age, err := correctedAge(res.Header(), t, Clock()); err == nil {
		res.Header().Set("Age", strconv.Itoa(int(math.Ceil(age.Seconds()))))
	} else {
		debugf("error calculating corrected age: %s", err.Error())
	}

	rw.Header().Set(ProxyDateHeader, Clock().Format(http.TimeFormat))
	h.storeResource(res, r)
}

// correctedAge adjusts the age of a resource for clock skeq and travel time
// https://httpwg.github.io/specs/rfc7234.html#rfc.section.4.2.3
func correctedAge(h http.Header, reqTime, respTime time.Time) (time.Duration, error) {
	date, err := timeHeader("Date", h)
	if err != nil {
		return time.Duration(0), err
	}

	apparentAge := respTime.Sub(date)
	if apparentAge < 0 {
		apparentAge = 0
	}

	respDelay := respTime.Sub(reqTime)
	ageSeconds, err := intHeader("Age", h)
	age := time.Second * time.Duration(ageSeconds)
	correctedAge := age + respDelay

	if apparentAge > correctedAge {
		correctedAge = apparentAge
	}

	residentTime := Clock().Sub(respTime)
	currentAge := correctedAge + residentTime

	return currentAge, nil
}

func (h *Handler) isCacheable(res *Resource, r *cacheRequest) bool {
	cc, err := res.cacheControl()
	if err != nil {
		errorf("Error parsing cache-control: %s", err.Error())
		return false
	}

	if cc.Has("no-cache") || cc.Has("no-store") {
		return false
	}

	if cc.Has("private") && len(cc["private"]) == 0 && h.Shared {
		return false
	}

	if _, ok := storeable[res.Status()]; !ok {
		return false
	}

	if r.Header.Get("Authorization") != "" && h.Shared {
		return false
	}

	if res.Header().Get("Authorization") != "" && h.Shared &&
		!cc.Has("must-revalidate") && !cc.Has("s-maxage") {
		return false
	}

	if res.HasExplicitExpiration() {
		return true
	}

	if _, ok := cacheableByDefault[res.Status()]; !ok && !cc.Has("public") {
		return false
	}

	if res.HasValidators() {
		return true
	} else if res.HeuristicFreshness() > 0 {
		return true
	}

	return false
}

func (h *Handler) serveResource(res *Resource, w http.ResponseWriter, req *cacheRequest) {
	for key, headers := range res.Header() {
		for _, header := range headers {
			w.Header().Add(key, header)
		}
	}

	age, err := res.Age()
	if err != nil {
		http.Error(w, "Error calculating age: "+err.Error(),
			http.StatusInternalServerError)
		return
	}

	// http://httpwg.github.io/specs/rfc7234.html#warn.113
	if age > (time.Hour*24) && res.HeuristicFreshness() > (time.Hour*24) {
		w.Header().Add("Warning", `113 - "Heuristic Expiration"`)
	}

	// http://httpwg.github.io/specs/rfc7234.html#warn.110
	freshness, err := h.freshness(res, req)
	if err != nil || freshness <= 0 {
		w.Header().Add("Warning", `110 - "Response is Stale"`)
	}

	debugf("resource is %s old, updating age from %s",
		age.String(), w.Header().Get("Age"))

	w.Header().Set("Age", fmt.Sprintf("%.f", math.Floor(age.Seconds())))
	w.Header().Set("Via", res.Via())

	// hacky handler for non-ok statuses
	if res.Status() != http.StatusOK {
		w.WriteHeader(res.Status())
		io.Copy(w, res)
	} else {
		http.ServeContent(w, req.Request, "", res.LastModified(), res)
	}
}

func (h *Handler) invalidateResource(res *Resource, r *cacheRequest) {
	Writes.Add(1)

	go func() {
		defer Writes.Done()
		debugf("invalidating resource %+v", res)
	}()
}

func (h *Handler) storeResource(res *Resource, r *cacheRequest) {
	Writes.Add(1)

	go func() {
		defer Writes.Done()
		t := Clock()
		keys := []string{r.Key.String()}
		headers := res.Header()

		if h.Shared {
			res.RemovePrivateHeaders()
		}

		// store a secondary vary version
		if vary := headers.Get("Vary"); vary != "" {
			debugf("Store secondary vary version key", r.Key.Vary(vary, r.Request).String())
			keys = append(keys, r.Key.Vary(vary, r.Request).String())
			debugf("Vary keys, might need cleanup", keys)
		}

		if err := h.cache.Store(res, keys...); err != nil {
			errorf("storing resources %#v failed with error: %s", keys, err.Error())
		}

		debugf("stored resources %+v in %s", keys, Clock().Sub(t))
	}()
}

// lookupResource finds the best matching Resource for the
// request, or nil and ErrNotFoundInCache if none is found
func (h *Handler) lookup(req *cacheRequest) (*Resource, error) {
	res, err := h.cache.Retrieve(req.Key.String())
	debugf("Ran one lookup with the key", req.Key.String())
	debugf("Result is", res)
	debugf("Result err is", err)

	// HEAD requests can possibly be served from GET
	if err == ErrNotFoundInCache && req.Method == "HEAD" {
		res, err = h.cache.Retrieve(req.Key.ForMethod("GET").String())
		if err != nil {
			return nil, err
		}

		if res.HasExplicitExpiration() && req.isCacheable() {
			debugf("using cached GET request for serving HEAD")
			return res, nil
		} else {
			return nil, ErrNotFoundInCache
		}
	} else if err != nil {
		return res, err
	}

	// Secondary lookup for Vary
	if vary := res.Header().Get("Vary"); vary != "" {
		debugf("Running a secondary lookup with Vary")
		debugf("Vary", vary)
		debugf("Vary Key", req.Key.Vary(vary, req.Request).String())
		res, err = h.cache.Retrieve(req.Key.Vary(vary, req.Request).String())
		if err != nil {
			debugf("Vary fetch err", err)
			return res, err
		}
	}

	return res, nil
}

type cacheRequest struct {
	*http.Request
	Key          Key
	Time         time.Time
	CacheControl CacheControl
}

func newCacheRequest(r *http.Request, storeIdUrl *url.URL) (*cacheRequest, error) {
//	debugf("newCacheRequest headers status", r)
	cc, err := ParseCacheControl(r.Header.Get("Cache-Control"))
	if err != nil {
		return nil, err
	}

	if r.Proto == "HTTP/1.1" && r.Host == "" {
		return nil, errors.New("Host header can't be empty")
	}

//	debugf("newCacheRequest headers status", r)
	return &cacheRequest{
		Request:      r,
		Key:          NewRequestKey(r, storeIdUrl),
		Time:         Clock(),
		CacheControl: cc,
	}, nil
}

func (r *cacheRequest) isStateChanging() bool {
	if !(r.Method == "POST" || r.Method == "PUT" || r.Method == "DELETE") {
		return true
	}

	return false
}

func (r *cacheRequest) isCacheable() bool {
	if !(r.Method == "GET" || r.Method == "HEAD") {
		debugf("Method not GET or HEAD non cachable")
		return false
	}

	if r.Header.Get("If-Match") != "" ||
		r.Header.Get("If-Unmodified-Since") != "" ||
		r.Header.Get("If-Range") != "" {
		debugf("If-Match/Unmofified-Since/Range non cacheable")
		return false
	}

	if maxAge, ok := r.CacheControl.Get("max-age"); ok && maxAge == "0" {
		debugf("Max Age == 0, should non-cachable") 
		return false //Bypassing resulted in a catastrophy
	}

	if r.CacheControl.Has("no-store") || r.CacheControl.Has("no-cache") {
		debugf("Cache Request no-cache or no-store non-cachable", r.CacheControl)
		return false
	}

	return true
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

// Resource returns a copy of the responseBuffer as a Resource object
func (rw *responseBuffer) Resource() *Resource {
	return NewResourceBytes(rw.StatusCode, rw.Buffer.Bytes(), rw.Header())
}
