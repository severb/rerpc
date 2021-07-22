package rerpc

import (
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/akshayjshah/rerpc/internal/statuspb/v0"
)

var (
	// Always advertise that reRPC accepts gzip compression.
	acceptEncodingValue    = strings.Join([]string{CompressionGzip, CompressionIdentity}, ",")
	acceptPostValueDefault = strings.Join(
		[]string{TypeDefaultGRPC, TypeProtoGRPC, TypeJSON},
		",",
	)
	acceptPostValueWithoutJSON = strings.Join(
		[]string{TypeDefaultGRPC, TypeProtoGRPC},
		",",
	)
)

type handlerCfg struct {
	MinTimeout          time.Duration
	MaxTimeout          time.Duration
	DisableGzipResponse bool
	DisableJSON         bool
	MaxRequestBytes     int
	Registrar           *Registrar
}

type HandlerOption interface {
	apply(*handlerCfg)
}

type handlerOptionFunc func(*handlerCfg)

func (f handlerOptionFunc) apply(cfg *handlerCfg) { f(cfg) }

// HandlerMinTimeout sets the minimum allowable timeout. Requests with less
// than the minimum timeout fail immediately with CodeDeadlineExceeded.
//
// By default, any positive timeout is allowed.
func HandlerMinTimeout(d time.Duration) HandlerOption {
	return handlerOptionFunc(func(cfg *handlerCfg) {
		cfg.MinTimeout = d
	})
}

// HandlerMaxTimeout sets the maximum allowable timeout. Calls with timeouts
// greater than the max (including calls with no timeout) are clamped to the
// maximum allowed timeout. Setting the max timeout to zero allows any timeout.
//
// By default, there's no enforced max timeout.
func HandlerMaxTimeout(d time.Duration) HandlerOption {
	return handlerOptionFunc(func(cfg *handlerCfg) {
		cfg.MaxTimeout = d
	})
}

// GzipResponses enables or disables gzip compression of the response message.
// Note that even when gzip compression is enabled, it's only used if the
// client supports it.
//
// By default, responses are gzipped whenever possible.
func GzipResponses(enable bool) HandlerOption {
	return handlerOptionFunc(func(cfg *handlerCfg) {
		cfg.DisableGzipResponse = !enable
	})
}

// HandlerMaxRequestBytes sets the maximum allowable request size (after
// compression, if applicable). Requests larger than the configured size fail
// early, and the data is never read into memory. Setting the maximum to zero
// allows any request size.
//
// By default, the client allows any request size.
func HandlerMaxRequestBytes(n int) HandlerOption {
	return handlerOptionFunc(func(cfg *handlerCfg) {
		cfg.MaxRequestBytes = n
	})
}

// HandlerSupportJSON enables or disables support for JSON requests and
// responses.
//
// By default, handlers support JSON.
func HandlerSupportJSON(enable bool) HandlerOption {
	return handlerOptionFunc(func(cfg *handlerCfg) {
		cfg.DisableJSON = !enable
	})
}

// A Handler is the server-side implementation of a single RPC defined by a
// protocol buffer service. It's the interface between the reRPC library and
// the code generated by the reRPC protoc plugin; most users won't ever need to
// deal with it directly.
//
// To see an example of how Handler is used in the generated code, see the
// internal/pingpb/v0 package.
type Handler struct {
	implementation func(context.Context, proto.Message) (proto.Message, error)
	// rawGRPC is used only for our hand-rolled reflection handler, which needs
	// bidi streaming
	rawGRPC func(
		http.ResponseWriter,
		*http.Request,
		string, // request compression
		string, // response compression
	)
	config handlerCfg
}

// NewHandler constructs a Handler.
func NewHandler(
	fqn string, // fully-qualified protobuf method name
	impl func(context.Context, proto.Message) (proto.Message, error),
	opts ...HandlerOption,
) *Handler {
	var cfg handlerCfg
	for _, opt := range opts {
		opt.apply(&cfg)
	}
	if reg := cfg.Registrar; reg != nil {
		reg.register(fqn)
	}
	return &Handler{
		implementation: impl,
		config:         cfg,
	}
}

// Serve executes the handler, much like the standard library's http.Handler.
// Unlike http.Handler, it requires a pointer to the protoc-generated request
// struct. See the internal/pingpb/v0 package for an example of how this code
// is used in reRPC's generated code.
//
// As long as the caller allocates a new request struct for each call, this
// method is safe to use concurrently.
func (h *Handler) Serve(w http.ResponseWriter, r *http.Request, msg proto.Message) {
	// To ensure that we can re-use connections, always consume and close the
	// request body.
	defer r.Body.Close()
	defer io.Copy(ioutil.Discard, r.Body)

	if r.Method != http.MethodPost {
		// grpc-go returns a 500 here, but interoperability with non-gRPC HTTP
		// clients is better if we return a 405.
		w.Header().Set("Allow", http.MethodPost)
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	ctype := r.Header.Get("Content-Type")
	if ctype == TypeJSON && h.config.DisableJSON {
		w.Header().Set("Accept-Post", acceptPostValueWithoutJSON)
		w.WriteHeader(http.StatusUnsupportedMediaType)
		return
	}
	if ctype != TypeDefaultGRPC && ctype != TypeProtoGRPC && ctype != TypeJSON {
		// grpc-go returns 500, but the spec recommends 415.
		// https://github.com/grpc/grpc/blob/master/doc/PROTOCOL-HTTP2.md#requests
		w.Header().Set("Accept-Post", acceptPostValueDefault)
		w.WriteHeader(http.StatusUnsupportedMediaType)
		return
	}

	// We're always going to respond with the same content type as the request.
	w.Header().Set("Content-Type", ctype)
	if ctype == TypeJSON {
		h.serveJSON(w, r, msg)
	} else {
		h.serveGRPC(w, r, msg)
	}
}

func (h *Handler) serveJSON(w http.ResponseWriter, r *http.Request, msg proto.Message) {
	if !h.config.DisableGzipResponse {
		var returnToPool func()
		w, returnToPool = maybeGzipWriter(w, r)
		defer returnToPool()
	}

	r, cancel, err := applyTimeout(r, h.config.MinTimeout, h.config.MaxTimeout)
	if err != nil {
		// Errors here indicate that the client sent an invalid timeout header, so
		// the exact error is safe to send back.
		writeErrorJSON(w, wrap(CodeInvalidArgument, err))
		return
	}
	defer cancel()

	body, closeReader, err := maybeGzipReader(r)
	if err != nil {
		// TODO: observability
		writeErrorJSON(w, errorf(CodeUnknown, "can't read gzipped body"))
		return
	}
	defer closeReader()

	if max := h.config.MaxRequestBytes; max > 0 {
		body = &io.LimitedReader{
			R: body,
			N: int64(max),
		}
	}

	if err := unmarshalJSON(body, msg); err != nil {
		// TODO: observability
		writeErrorJSON(w, errorf(CodeInvalidArgument, "can't unmarshal JSON body"))
		return
	}

	res, implErr := h.implementation(r.Context(), msg)
	if implErr != nil {
		// It's the user's job to sanitize the error string.
		writeErrorJSON(w, implErr)
		return
	}

	if err := marshalJSON(w, res); err != nil {
		// TODO: observability
		return
	}
}

func (h *Handler) serveGRPC(w http.ResponseWriter, r *http.Request, msg proto.Message) {
	// We always send grpc-accept-encoding. Set it here so it's ready to go in
	// future error cases.
	w.Header().Set("Grpc-Accept-Encoding", acceptEncodingValue)
	w.Header().Set("User-Agent", UserAgent)
	// Every gRPC response will have these trailers.
	w.Header().Add("Trailer", "Grpc-Status")
	w.Header().Add("Trailer", "Grpc-Message")
	w.Header().Add("Trailer", "Grpc-Status-Details-Bin")

	requestCompression := CompressionIdentity
	if me := r.Header.Get("Grpc-Encoding"); me != "" {
		switch me {
		case CompressionIdentity:
			requestCompression = CompressionIdentity
		case CompressionGzip:
			requestCompression = CompressionGzip
		default:
			// Per https://github.com/grpc/grpc/blob/master/doc/compression.md, we
			// should return CodeUnimplemented and specify acceptable compression(s)
			// (in addition to setting the Grpc-Accept-Encoding header).
			writeErrorGRPC(w, errorf(CodeUnimplemented, "unknown compression %q: accepted grpc-encoding values are %v", me, acceptEncodingValue))
			return
		}
	}

	// Follow https://github.com/grpc/grpc/blob/master/doc/compression.md.
	// (The grpc-go implementation doesn't read the "grpc-accept-encoding" header
	// and doesn't support compression method asymmetry.)
	responseCompression := requestCompression
	if mae := r.Header.Get("Grpc-Accept-Encoding"); mae != "" {
		for _, enc := range strings.FieldsFunc(mae, splitOnCommasAndSpaces) {
			switch enc {
			case CompressionGzip: // prefer gzip
				responseCompression = CompressionGzip
				break
			case CompressionIdentity:
				responseCompression = CompressionIdentity
				break
			}
		}
	}
	if h.config.DisableGzipResponse {
		responseCompression = CompressionIdentity
	}
	w.Header().Set("Grpc-Encoding", responseCompression)

	r, cancel, err := applyTimeout(r, h.config.MinTimeout, h.config.MaxTimeout)
	if err != nil {
		// Errors here indicate that the client sent an invalid timeout header, so
		// the exact error is safe to send back.
		writeErrorGRPC(w, wrap(CodeInvalidArgument, err))
		return
	}
	defer cancel()

	if raw := h.rawGRPC; raw != nil {
		raw(w, r, requestCompression, responseCompression)
		return
	}

	if err := unmarshalLPM(r.Body, msg, requestCompression, h.config.MaxRequestBytes); err != nil {
		// TODO: observability
		writeErrorGRPC(w, errorf(CodeInvalidArgument, "can't unmarshal protobuf request"))
		return
	}

	res, implErr := h.implementation(r.Context(), msg)
	if implErr != nil {
		// It's the user's job to sanitize the error string.
		writeErrorGRPC(w, implErr)
		return
	}

	if err := marshalLPM(w, res, responseCompression, 0 /* maxBytes */); err != nil {
		// It's safe to write gRPC errors even after we've started writing the
		// body.
		// TODO: observability
		writeErrorGRPC(w, errorf(CodeUnknown, "can't marshal protobuf response"))
		return
	}

	writeErrorGRPC(w, nil)
}

func splitOnCommasAndSpaces(c rune) bool {
	return c == ',' || c == ' '
}

func writeErrorJSON(w http.ResponseWriter, err error) {
	s := statusFromError(err)
	bs, err := jsonpbMarshaler.Marshal(s)
	if err != nil {
		// TODO: observability
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintf(w, `{"code": %d, "message": "error marshaling status with code %d"}`, CodeInternal, s.Code)
		return
	}
	w.WriteHeader(Code(s.Code).http())
	if _, err := w.Write(bs); err != nil {
		// TODO: observability
	}
}

func writeErrorGRPC(w http.ResponseWriter, err error) {
	if err == nil {
		w.Header().Set("Grpc-Status", strconv.Itoa(int(CodeOK)))
		w.Header().Set("Grpc-Message", "")
		w.Header().Set("Grpc-Status-Details-Bin", "")
		return
	}
	// gRPC errors are successes at the HTTP level and net/http automatically
	// sends a 200 if we don't set a status code. Leaving the HTTP status
	// implicit lets us use this function when we hit an error partway through
	// writing the body.
	s := statusFromError(err)
	code := strconv.Itoa(int(s.Code))
	// If we ever need to send more trailers, make sure to declare them in the headers
	// above.
	if bin, err := proto.Marshal(s); err != nil {
		w.Header().Set("Grpc-Status", strconv.Itoa(int(CodeInternal)))
		w.Header().Set("Grpc-Message", percentEncode("error marshaling protobuf status with code "+code))
	} else {
		w.Header().Set("Grpc-Status", code)
		w.Header().Set("Grpc-Message", percentEncode(s.Message))
		w.Header().Set("Grpc-Status-Details-Bin", encodeBinaryHeader(bin))
	}
}

func statusFromError(err error) *statuspb.Status {
	s := &statuspb.Status{
		Code:    int32(CodeUnknown),
		Message: err.Error(),
	}
	if re, ok := AsError(err); ok {
		s.Code = int32(re.Code())
		s.Details = re.Details()
		if e := re.Unwrap(); e != nil {
			s.Message = e.Error() // don't repeat code
		}
	}
	return s
}
