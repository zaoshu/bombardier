package main

import (
	"crypto/tls"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/goware/urlx"
	"github.com/valyala/fasthttp"
	"golang.org/x/net/http2"
)

type client interface {
	do() (code int, msTaken uint64, err error)
}

type bodyStreamProducer func() (io.ReadCloser, error)

type clientOpts struct {
	HTTP2 bool

	maxConns  uint64
	timeout   time.Duration
	tlsConfig *tls.Config

	headers     *headersList
	url, method string

	body    *string
	bodProd bodyStreamProducer

	bytesRead, bytesWritten *int64

	proxy string
}

type fasthttpClient struct {
	client *fasthttp.Client

	headers     *fasthttp.RequestHeader
	url, method string

	body    *string
	bodProd bodyStreamProducer
}

func newFastHTTPClient(opts *clientOpts) client {
	c := new(fasthttpClient)
	c.client = &fasthttp.Client{
		MaxConnsPerHost:               int(opts.maxConns),
		ReadTimeout:                   opts.timeout,
		WriteTimeout:                  opts.timeout,
		DisableHeaderNamesNormalizing: true,
		TLSConfig:                     opts.tlsConfig,
		Dial: fasthttpDialFunc(
			opts.bytesRead, opts.bytesWritten,
		),
	}
	c.headers = headersToFastHTTPHeaders(opts.headers)
	c.url, c.method, c.body = opts.url, opts.method, opts.body
	c.bodProd = opts.bodProd
	return client(c)
}

func (c *fasthttpClient) do() (
	code int, msTaken uint64, err error,
) {
	// prepare the request
	req := fasthttp.AcquireRequest()
	resp := fasthttp.AcquireResponse()
	if c.headers != nil {
		c.headers.CopyTo(&req.Header)
	}
	req.Header.SetMethod(c.method)
	req.SetRequestURI(c.url)
	if c.body != nil {
		req.SetBodyString(*c.body)
	} else {
		bs, bserr := c.bodProd()
		if bserr != nil {
			return 0, 0, bserr
		}
		req.SetBodyStream(bs, -1)
	}

	// fire the request
	start := time.Now()
	err = c.client.Do(req, resp)
	if err != nil {
		code = -1
	} else {
		code = resp.StatusCode()
	}
	msTaken = uint64(time.Since(start).Nanoseconds() / 1000)

	// release resources
	fasthttp.ReleaseRequest(req)
	fasthttp.ReleaseResponse(resp)

	return
}

type httpClient struct {
	client *http.Client

	headers http.Header
	url     *url.URL
	method  string

	body    *string
	bodProd bodyStreamProducer
}

func randomProxy(proxy string) func(*http.Request) (*url.URL, error) {
	var proxyURL []*url.URL
	for _, p := range strings.Split(proxy, ",") {
		u, err := url.Parse(p)
		if err != nil {
			panic(err)
		}
		proxyURL = append(proxyURL, u)
	}

	index := 0
	return func(req *http.Request) (*url.URL, error) {
		if len(proxyURL) == 0 {
			return nil, nil
		}
		u := proxyURL[index]
		index = (index + 1) % len(proxyURL)
		return u, nil
	}
}

func newHTTPClient(opts *clientOpts) client {
	c := new(httpClient)
	tr := &http.Transport{
		Proxy:               randomProxy(opts.proxy),
		TLSClientConfig:     opts.tlsConfig,
		MaxIdleConnsPerHost: int(opts.maxConns),
	}
	tr.DialContext = httpDialContextFunc(opts.bytesRead, opts.bytesWritten)
	if opts.HTTP2 {
		_ = http2.ConfigureTransport(tr)
	} else {
		tr.TLSNextProto = make(
			map[string]func(authority string, c *tls.Conn) http.RoundTripper,
		)
	}

	cl := &http.Client{
		Transport: tr,
		Timeout:   opts.timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	c.client = cl

	c.headers = headersToHTTPHeaders(opts.headers)
	c.method, c.body, c.bodProd = opts.method, opts.body, opts.bodProd
	var err error
	c.url, err = urlx.Parse(opts.url)
	if err != nil {
		// opts.url guaranteed to be valid at this point
		panic(err)
	}

	return client(c)
}

func (c *httpClient) do() (
	code int, msTaken uint64, err error,
) {
	req := &http.Request{}

	req.Header = c.headers
	req.Method = c.method
	req.URL = c.url

	if host := req.Header.Get("Host"); host != "" {
		req.Host = host
	}

	if c.body != nil {
		br := strings.NewReader(*c.body)
		req.ContentLength = int64(len(*c.body))
		req.Body = ioutil.NopCloser(br)
	} else {
		bs, bserr := c.bodProd()
		if bserr != nil {
			return 0, 0, bserr
		}
		req.Body = bs
	}

	start := time.Now()
	resp, err := c.client.Do(req)
	if err != nil {
		code = -1
	} else {
		code = resp.StatusCode

		_, berr := io.Copy(ioutil.Discard, resp.Body)
		if berr != nil {
			err = berr
		}

		if cerr := resp.Body.Close(); cerr != nil {
			err = cerr
		}
	}
	msTaken = uint64(time.Since(start).Nanoseconds() / 1000)

	return
}

func headersToFastHTTPHeaders(h *headersList) *fasthttp.RequestHeader {
	if len(*h) == 0 {
		return nil
	}
	res := new(fasthttp.RequestHeader)
	for _, header := range *h {
		res.Set(header.key, header.value)
	}
	return res
}

func headersToHTTPHeaders(h *headersList) http.Header {
	if len(*h) == 0 {
		return http.Header{}
	}
	headers := http.Header{}

	for _, header := range *h {
		headers[header.key] = []string{header.value}
	}
	return headers
}
