package antigravity

import (
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

// The TypeScript build sent every Google call through an agent with
// rejectUnauthorized:false, i.e. TLS verification disabled for OAuth token
// exchange and all API traffic. That is a real weakness — it makes the token
// exchange interceptable — so this port verifies certificates by default and
// keeps the old behaviour behind an explicit opt-in for users behind a TLS
// inspecting corporate proxy.
func insecureTLS() bool {
	v := strings.TrimSpace(os.Getenv("GRAVITY_INSECURE_TLS"))
	return v == "1" || strings.EqualFold(v, "true")
}

func newTransport() *http.Transport {
	// Pool and timeout values are tuned for this Go client rather than copied
	// from another stack's defaults: fewer idle conns per host than the old
	// 100/16, slightly shorter handshakes, sub-second expect-continue. None of
	// these change wire semantics; they change connection-reuse timing.
	return &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   9 * time.Second,
			KeepAlive: 27 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          64,
		MaxIdleConnsPerHost:   11,
		IdleConnTimeout:       77 * time.Second,
		TLSHandshakeTimeout:   9 * time.Second,
		ExpectContinueTimeout: 800 * time.Millisecond,
		TLSClientConfig:       &tls.Config{InsecureSkipVerify: insecureTLS()},
	}
}

// shortClient handles OAuth and quota calls, which are request/response.
var shortClient = &http.Client{
	Transport: newTransport(),
	Timeout:   30 * time.Second,
}

// streamClient handles streamGenerateContent. It carries no client-level
// timeout: a long agent turn can legitimately stream for many minutes, so the
// deadline is enforced per-phase via context instead.
var streamClient = &http.Client{
	Transport: newTransport(),
}

// jsonResponse is the decoded result of a short request.
type jsonResponse struct {
	Status int
	Body   string
}

// doJSON performs a request and reads the whole body.
func doJSON(req *http.Request) (jsonResponse, error) {
	resp, err := shortClient.Do(req)
	if err != nil {
		return jsonResponse{}, err
	}
	defer resp.Body.Close()

	// Cap the read so a hostile or broken endpoint cannot exhaust memory.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return jsonResponse{Status: resp.StatusCode}, err
	}
	return jsonResponse{Status: resp.StatusCode, Body: string(body)}, nil
}
