package steam

import (
	"net/http"
	"net/url"
	"strings"
)

// Secret redaction for everything auth-adjacent. Refresh tokens, access
// tokens and session cookies are password-equivalent: they must never
// reach logs, errors, traces or metrics. Dumps pass through here first.

// redactHeaders returns a copy of h with secret-bearing values replaced.
func redactHeaders(h http.Header) http.Header {
	out := make(http.Header, len(h))
	for k, v := range h {
		switch strings.ToLower(strings.TrimSpace(k)) {
		case "authorization", "cookie", "set-cookie", "x-steam-auth":
			cp := make([]string, len(v))
			for i := range cp {
				cp[i] = "[redacted]"
			}
			out[k] = cp
		default:
			out[k] = append([]string(nil), v...)
		}
	}
	return out
}

var redactQueryKeys = []string{
	"key", "nonce", "access_token", "refresh_token", "steamLoginSecure", "sessionid",
}

// redactURL strips secret query values, keeping the URL shape for debugging.
func redactURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "[unparseable url]"
	}
	q := u.Query()
	changed := false
	for _, k := range redactQueryKeys {
		for param := range q {
			if strings.EqualFold(param, k) {
				q[param] = []string{"[redacted]"}
				changed = true
			}
		}
	}
	if changed {
		u.RawQuery = q.Encode()
	}
	return u.String()
}

// redactValue replaces a bare secret with a placeholder for error paths.
func redactValue(s string) string {
	if strings.TrimSpace(s) == "" {
		return ""
	}
	return "[redacted]"
}
