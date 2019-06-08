package brotlihandler // import "github.com/corona10/brotlihandler"

import (
	"bufio"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/andybalholm/brotli"
)

const (
	vary            = "Vary"
	acceptEncoding  = "Accept-Encoding"
	contentEncoding = "Content-Encoding"
	contentType     = "Content-Type"
	contentLength   = "Content-Length"
)

type codings map[string]float64

const (
	// DefaultQValue is the default qvalue to assign to an encoding if no explicit qvalue is set.
	// This is actually kind of ambiguous in RFC 2616, so hopefully it's correct.
	// The examples seem to indicate that it is.
	DefaultQValue = 1.0

	// DefaultMinSize is the default minimum size until we enable brotli compression.
	// 1500 bytes is the MTU size for the internet since that is the largest size allowed at the network layer.
	// If you take a file that is 1300 bytes and compress it to 800 bytes, it’s still transmitted in that same 1500 byte packet regardless, so you’ve gained nothing.
	// That being the case, you should restrict the brotli compression to files with a size greater than a single packet, 1400 bytes (1.4KB) is a safe value.
	DefaultMinSize = 1400
)

// Parsed representation of one of the inputs to ContentTypes.
// See https://golang.org/pkg/mime/#ParseMediaType
type parsedContentType struct {
	mediaType string
	params    map[string]string
}

// equals returns whether this content type matches another content type.
func (pct parsedContentType) equals(mediaType string, params map[string]string) bool {
	if pct.mediaType != mediaType {
		return false
	}
	// if pct has no params, don't care about other's params
	if len(pct.params) == 0 {
		return true
	}

	// if pct has any params, they must be identical to other's.
	if len(pct.params) != len(params) {
		return false
	}
	for k, v := range pct.params {
		if w, ok := params[k]; !ok || v != w {
			return false
		}
	}
	return true
}

// brotliWriterPools stores a sync.Pool for each compression level for reuse of
// brotli.Writers. Use poolIndex to covert a compression level to an index into
// brotliWriterPools.
var brotliWriterPools [brotli.BestCompression - brotli.BestSpeed + 1]*sync.Pool

func init() {
	for i := brotli.BestSpeed; i <= brotli.BestCompression; i++ {
		brotliWriterPools[i] = &sync.Pool{
			New: func() interface{} {
				// NewWriterLevel only returns error on a bad level, we are guaranteeing
				// that this will be a valid level so it is okay to ignore the returned
				// error.
				w := brotli.NewWriterLevel(nil, i)
				return w
			},
		}
	}
}

type BrotliResponseWriter struct {
	http.ResponseWriter
	index int
	br    *brotli.Writer
	code  int // Saves the WriteHeader value.

	minSize int    // Specifed the minimum response size to brotli. If the response length is bigger than this value, it is compressed.
	buf     []byte // Holds the first part of the write before reaching the minSize or the end of the write.
	ignore  bool   // If true, then we immediately passthru writes to the underlying ResponseWriter.

	contentTypes []parsedContentType // Only compress if the response is one of these content-types. All are accepted if empty.
}

type BrotliResponseWriterWithCloseNotify struct {
	*BrotliResponseWriter
}

func (w BrotliResponseWriterWithCloseNotify) CloseNotify() <-chan bool {
	return w.ResponseWriter.(http.CloseNotifier).CloseNotify()
}

func (w *BrotliResponseWriter) Write(b []byte) (int, error) {
	if w.br != nil {
		return w.br.Write(b)
	}

	if w.ignore {
		return w.ResponseWriter.Write(b)
	}

	w.buf = append(w.buf, b...)

	var (
		cl, _ = strconv.Atoi(w.Header().Get(contentLength))
		ct    = w.Header().Get(contentType)
		ce    = w.Header().Get(contentEncoding)
	)
	// Only continue if they didn't already choose an encoding or a known unhandled content length or type.
	if ce == "" && (cl == 0 || cl >= w.minSize) && (ct == "" || handleContentType(w.contentTypes, ct)) {
		// If the current buffer is less than minSize and a Content-Length isn't set, then wait until we have more data.
		if len(w.buf) < w.minSize && cl == 0 {
			return len(b), nil
		}
		// If the Content-Length is larger than minSize or the current buffer is larger than minSize, then continue.
		if cl >= w.minSize || len(w.buf) >= w.minSize {
			// If a Content-Type wasn't specified, infer it from the current buffer.
			if ct == "" {
				ct = http.DetectContentType(w.buf)
				w.Header().Set(contentType, ct)
			}
			// If the Content-Type is acceptable to brotli, initialize the brotli writer.
			if handleContentType(w.contentTypes, ct) {
				if err := w.startBrotli(); err != nil {
					return 0, err
				}
				return len(b), nil
			}
		}
	}
	// If we got here, we should not brotli this response.
	if err := w.startPlain(); err != nil {
		return 0, err
	}
	return len(b), nil
}

// WriteHeader just saves the response code until close or Brotli effective writes.
func (w *BrotliResponseWriter) WriteHeader(code int) {
	if w.code == 0 {
		w.code = code
	}
}

func (w *BrotliResponseWriter) init() {
	brw := brotliWriterPools[w.index].Get().(*brotli.Writer)
	brw.Reset(w.ResponseWriter)
	w.br = brw
}

// Close will close the brotli.Writer and will put it back in the brotliWriterPool.
func (w *BrotliResponseWriter) Close() error {
	if w.ignore {
		return nil
	}

	// Brotli not triggered yet, write out regular response.
	if w.br == nil {
		err := w.startPlain()
		if err != nil {
			err = fmt.Errorf("brotlihandler: write to regular responseWriter at close gets error: %q", err.Error())
		}
		return err
	}
	err := w.br.Close()
	brotliWriterPools[w.index].Put(w.br)
	w.br = nil
	return err
}

// Flush flushes the underlying *brotli.Writer and then the underlying
// http.ResponseWriter if it is an http.Flusher. This makes BrotliResponseWriter
// an http.Flusher.
func (w *BrotliResponseWriter) Flush() {
	if w.br == nil && !w.ignore {
		return
	}

	if w.br != nil {
		w.br.Flush()
	}

	if fw, ok := w.ResponseWriter.(http.Flusher); ok {
		fw.Flush()
	}
}

func (w *BrotliResponseWriter) startBrotli() error {
	// Set the brotli header
	w.Header().Set(contentEncoding, "br")

	// if the Content-Length is already set, then calls to Write on brotli
	// will fail to set the Content-Length header since its already set
	// See: https://github.com/golang/go/issues/14975
	w.Header().Del(contentLength)

	if w.code != 0 {
		w.ResponseWriter.WriteHeader(w.code)
		w.code = 0
	}

	if len(w.buf) > 0 {
		// Initialize the Brotli response
		w.init()
		n, err := w.br.Write(w.buf)
		// This should never happen (per io.Writer docs), but if the write didn't
		// accept the entire buffer but returned no specific error, we have no clue
		// what's going on, so abort just to be safe.
		if err == nil && n < len(w.buf) {
			err = io.ErrShortWrite
		}
		return err
	}
	return nil
}

// startPlain writes to sent bytes and buffer the underlying ResponseWriter without brotli.
func (w *BrotliResponseWriter) startPlain() error {
	if w.code != 0 {
		w.ResponseWriter.WriteHeader(w.code)
		// Ensure that no other WriteHeader's happen
		w.code = 0
	}
	w.ignore = true
	// If Write was never called then don't call Write on the underlying ResponseWriter.
	if w.buf == nil {
		return nil
	}
	n, err := w.ResponseWriter.Write(w.buf)
	w.buf = nil
	// This should never happen (per io.Writer docs), but if the write didn't
	// accept the entire buffer but returned no specific error, we have no clue
	// what's going on, so abort just to be safe.
	if err == nil && n < len(w.buf) {
		err = io.ErrShortWrite
	}
	return err
}

// Hijack implements http.Hijacker. If the underlying ResponseWriter is a
// Hijacker, its Hijack method is returned. Otherwise an error is returned.
func (w *BrotliResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if hj, ok := w.ResponseWriter.(http.Hijacker); ok {
		return hj.Hijack()
	}
	return nil, nil, fmt.Errorf("http.Hijacker interface is not supported")
}

// acceptsBrotli returns true if the given HTTP request indicates that it will
// accept a brotli response.
func acceptsBrotli(r *http.Request) bool {
	acceptedEncodings, _ := parseEncodings(r.Header.Get(acceptEncoding))
	return acceptedEncodings["br"] > 0.0
}

// returns true if we've been configured to compress the specific content type.
func handleContentType(contentTypes []parsedContentType, ct string) bool {
	// If contentTypes is empty we handle all content types.
	if len(contentTypes) == 0 {
		return true
	}

	mediaType, params, err := mime.ParseMediaType(ct)
	if err != nil {
		return false
	}

	for _, c := range contentTypes {
		if c.equals(mediaType, params) {
			return true
		}
	}

	return false
}

// parseEncodings attempts to parse a list of codings, per RFC 2616, as might
// appear in an Accept-Encoding header. It returns a map of content-codings to
// quality values, and an error containing the errors encountered. It's probably
// safe to ignore those, because silently ignoring errors is how the internet
// works.
//
// See: http://tools.ietf.org/html/rfc2616#section-14.3.
func parseEncodings(s string) (codings, error) {
	c := make(codings)
	var e []string

	for _, ss := range strings.Split(s, ",") {
		coding, qvalue, err := parseCoding(ss)

		if err != nil {
			e = append(e, err.Error())
		} else {
			c[coding] = qvalue
		}
	}

	// TODO (adammck): Use a proper multi-error struct, so the individual errors
	//                 can be extracted if anyone cares.
	if len(e) > 0 {
		return c, fmt.Errorf("errors while parsing encodings: %s", strings.Join(e, ", "))
	}

	return c, nil
}

// parseCoding parses a single conding (content-coding with an optional qvalue),
// as might appear in an Accept-Encoding header. It attempts to forgive minor
// formatting errors.
func parseCoding(s string) (coding string, qvalue float64, err error) {
	for n, part := range strings.Split(s, ";") {
		part = strings.TrimSpace(part)
		qvalue = DefaultQValue

		if n == 0 {
			coding = strings.ToLower(part)
		} else if strings.HasPrefix(part, "q=") {
			qvalue, err = strconv.ParseFloat(strings.TrimPrefix(part, "q="), 64)

			if qvalue < 0.0 {
				qvalue = 0.0
			} else if qvalue > 1.0 {
				qvalue = 1.0
			}
		}
	}

	if coding == "" {
		err = fmt.Errorf("empty content-coding")
	}

	return
}

var _ http.Hijacker = &BrotliResponseWriter{}

// BrotliHandler wraps an HTTP handler, to transparently brotli the response body if
// the client supports it (via the Accept-Encoding header). This will compress at
// the default compression level.
func BrotliHandler(h http.Handler) http.Handler {
	wrapper, _ := NewBrotliLevelHandler(brotli.DefaultCompression)
	return wrapper(h)
}

// MustNewBrotliLevelHandler behaves just like NewBrotliLevelHandler except that in
// an error case it panics rather than returning an error.
func MustNewBrotliLevelHandler(level int) func(http.Handler) http.Handler {
	wrap, err := NewBrotliLevelHandler(level)
	if err != nil {
		panic(err)
	}
	return wrap
}

// Used for functional configuration.
type config struct {
	minSize      int
	level        int
	contentTypes []parsedContentType
}

func (c *config) validate() error {
	if c.level != brotli.DefaultCompression && (c.level < brotli.BestSpeed || c.level > brotli.BestCompression) {
		return fmt.Errorf("invalid compression level requested: %d", c.level)
	}

	if c.minSize < 0 {
		return fmt.Errorf("minimum size must be more than zero")
	}

	return nil
}

type option func(c *config)

func MinSize(size int) option {
	return func(c *config) {
		c.minSize = size
	}
}

func CompressionLevel(level int) option {
	return func(c *config) {
		c.level = level
	}
}

// NewBrotliLevelHandler returns a wrapper function (often known as middleware)
// which can be used to wrap an HTTP handler to transparently brotli the response
// body if the client supports it (via the Accept-Encoding header). Responses will
// be encoded at the given brotli compression level. An error will be returned only
// if an invalid brotli compression level is given, so if one can ensure the level
// is valid, the returned error can be safely ignored.
func NewBrotliLevelHandler(level int) (func(http.Handler) http.Handler, error) {
	return NewBrotliLevelAndMinSize(level, DefaultMinSize)
}

// NewBrotliLevelAndMinSize behave as NewBrotliLevelHandler except it let the caller
// specify the minimum size before compression.
func NewBrotliLevelAndMinSize(level, minSize int) (func(http.Handler) http.Handler, error) {
	return BrotliHandlerWithOpts(CompressionLevel(level), MinSize(minSize))
}

func BrotliHandlerWithOpts(opts ...option) (func(http.Handler) http.Handler, error) {
	c := &config{
		level:   brotli.DefaultCompression,
		minSize: DefaultMinSize,
	}

	for _, o := range opts {
		o(c)
	}

	if err := c.validate(); err != nil {
		return nil, err
	}

	return func(h http.Handler) http.Handler {
		index := c.level

		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Add(vary, acceptEncoding)
			if acceptsBrotli(r) {
				gw := &BrotliResponseWriter{
					ResponseWriter: w,
					index:          index,
					minSize:        c.minSize,
					contentTypes:   c.contentTypes,
				}
				defer gw.Close()

				if _, ok := w.(http.CloseNotifier); ok {
					gwcn := BrotliResponseWriterWithCloseNotify{gw}
					h.ServeHTTP(gwcn, r)
				} else {
					h.ServeHTTP(gw, r)
				}

			} else {
				h.ServeHTTP(w, r)
			}
		})
	}, nil
}
