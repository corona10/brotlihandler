# brotlihandler
Go middleware to brotli HTTP responses
This it the ported version from [NYTimes gziphandler](https://github.com/nytimes/gziphandler)

This is a tiny Go package which wraps HTTP handlers to transparently brotli the
response body, for clients which support it. Although it's usually simpler to
leave that to a reverse proxy (like nginx or Varnish), this package is useful
when that's undesirable.

## Install
```bash
go get -u github.com/corona10/brotlihandler
```

## Usage

Call `BrotliHandler` with any handler (an object which implements the
`http.Handler` interface), and it'll return a new handler which brotlis the
response. For example:

```go
package main

import (
	"io"
	"net/http"
	"github.com/corona10/brotlihandler
)

func main() {
	withoutBrotli := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		io.WriteString(w, "Hello, World")
	})

	withBrotli := brotlihandler.BrotliHandler(withoutBrotli)

	http.Handle("/", withBrotli)
	http.ListenAndServe("0.0.0.0:8000", nil)
}
```
