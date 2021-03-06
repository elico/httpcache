package httpcache

import (
	"bytes"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// Key represents a unique identifier for a resource in the cache
type Key struct {
	method string
	header http.Header
	u      url.URL
	vary   []string
}

// NewKey returns a new Key instance
func NewKey(method string, u *url.URL, h http.Header) Key {
	return Key{method: method, header: h, u: *u, vary: []string{}}
}

// RequestKey generates a Key for a request
func NewRequestKey(r *http.Request, storeIdUrl *url.URL) Key {
	// Canged the way we treat the URL to prevent the original request URL modification
	URL, _ := url.Parse(r.URL.String())

	debugf("StoreID url", storeIdUrl )
	// Here run a query against the StoreID api
	switch{
	case (strings.HasSuffix(URL.Host,".sdarot.pm") && strings.HasPrefix(URL.Host,"media")  && strings.HasSuffix(URL.Path, ".mp4") ):
		debugf("A sdarot.pm video, about to strip query terms from the request key", URL)
		URL.RawQuery = ""
		URL.Host = "sdarot.pm.media.ngtech.internal"
		debugf("A sdarot.pm video, After striping query terms from the request key", URL)
		debugf("A sdarot.pm video, the request", r)
	case (strings.HasSuffix(URL.Host,".download.windowsupdate.com") && (strings.HasSuffix(URL.Path, ".exe")  || strings.HasSuffix(URL.Path, ".cab") || strings.HasSuffix(URL.Path, ".esd") )):
		debugf("A windows updates domain and file, about to strip query terms from the request key", URL)
		URL.RawQuery = ""
		URL.Host = "windows.update.ngtech.internal"
		debugf("A windows updates file, After striping query terms from the request key", URL)
		debugf("A windows updates file, the request", r)

	default:
		debugf("Not a special file", URL)
	}
	if location := r.Header.Get("Content-Location"); location != "" {
		u, err := url.Parse(location)
		if err == nil {
			if !u.IsAbs() {
				u = r.URL.ResolveReference(u)
			}
			if u.Host != r.Host {
				debugf("illegal host %q in Content-Location", u.Host)
			} else {
				debugf("using Content-Location: %q", u.String())
				URL = u
			}
		} else {
			debugf("failed to parse Content-Location %q", location)
		}
	}

	// Here we can set the URL StoreID
	return NewKey(r.Method, URL, r.Header)
}

// ForKey returns a new Key with a given method
func (k Key) ForMethod(method string) Key {
	k2 := k
	k2.method = method
	return k2
}

// Vary returns a Key that is varied on particular headers in a http.Request
func (k Key) Vary(varyHeader string, r *http.Request) Key {
	k2 := k
//	debugf("Request details before handling Vary", r)
//	debugf("Vary Header before split", varyHeader)
	for _, header := range strings.Split(varyHeader, ", ") {
		k2.vary = append(k2.vary, header+"="+r.Header.Get(header))
	}
	debugf("Vary key2", k)
	return k2
}

func (k Key) String() string {
	URL := strings.ToLower(canonicalURL(&k.u).String())
	b := &bytes.Buffer{}
	b.WriteString(fmt.Sprintf("%s:%s", k.method, URL))

	if len(k.vary) > 0 {
		b.WriteString("::")
		for _, v := range k.vary {
			b.WriteString(v + ":")
		}
	}

	return b.String()
}

func canonicalURL(u *url.URL) *url.URL {
	return u
}
