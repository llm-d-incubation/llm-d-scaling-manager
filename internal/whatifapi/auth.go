package whatifapi

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"

	authnv1 "k8s.io/api/authentication/v1"
	authzv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// The authentication modes of `whatif serve --auth`.
const (
	// AuthNone serves with no authentication: loopback only.
	AuthNone = "none"
	// AuthKubernetes checks a bearer token with TokenReview and the
	// non-resource URL with SubjectAccessReview.
	AuthKubernetes = "kubernetes"
)

// AuthPathPrefix is what an API path is authorized as: GET on
// /whatif/v1/plan and so on, so that one rule (nonResourceURLs
// ["/whatif/*"], verbs ["get"]) grants the read-only API.
const AuthPathPrefix = "/whatif"

// CheckListen refuses a bind the mode does not cover: without Kubernetes
// authentication the API binds a loopback address only, since it can read
// Prometheus as the server does.
func CheckListen(addr, mode string) error {
	switch mode {
	case AuthNone, AuthKubernetes:
	default:
		return fmt.Errorf("--auth %q, want %s or %s", mode, AuthNone, AuthKubernetes)
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("--listen %q: %w", addr, err)
	}
	if mode == AuthNone && !IsLoopback(host) {
		return fmt.Errorf("--listen %s is not a loopback address: serving it needs --auth=%s (a bearer token checked with TokenReview and SubjectAccessReview)",
			addr, AuthKubernetes)
	}
	return nil
}

// IsLoopback reports whether host names a loopback address: localhost, or
// an IP in 127.0.0.0/8 or ::1. An empty host is every interface.
func IsLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Authenticate wraps next in Kubernetes authentication and authorization, as
// the controller's metrics endpoint does: the bearer token's user, from
// TokenReview, must be allowed GET on the path under AuthPathPrefix by
// SubjectAccessReview. No token, or one TokenReview does not authenticate, is
// 401; a user not allowed is 403.
func Authenticate(c kubernetes.Interface, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || strings.TrimSpace(token) == "" {
			writeError(w, http.StatusUnauthorized, errors.New("a bearer token is required"))
			return
		}
		tr, err := c.AuthenticationV1().TokenReviews().Create(r.Context(),
			&authnv1.TokenReview{Spec: authnv1.TokenReviewSpec{Token: strings.TrimSpace(token)}}, metav1.CreateOptions{})
		if err != nil {
			writeError(w, http.StatusInternalServerError, fmt.Errorf("token review: %w", err))
			return
		}
		if !tr.Status.Authenticated {
			writeError(w, http.StatusUnauthorized, errors.New("the bearer token is not authenticated"))
			return
		}
		u := tr.Status.User
		extra := map[string]authzv1.ExtraValue{}
		for k, v := range u.Extra {
			extra[k] = authzv1.ExtraValue(v)
		}
		path := AuthPathPrefix + r.URL.Path
		sar, err := c.AuthorizationV1().SubjectAccessReviews().Create(r.Context(), &authzv1.SubjectAccessReview{
			Spec: authzv1.SubjectAccessReviewSpec{User: u.Username, Groups: u.Groups, UID: u.UID, Extra: extra,
				NonResourceAttributes: &authzv1.NonResourceAttributes{Path: path, Verb: "get"}},
		}, metav1.CreateOptions{})
		if err != nil {
			writeError(w, http.StatusInternalServerError, fmt.Errorf("subject access review: %w", err))
			return
		}
		if !sar.Status.Allowed {
			writeError(w, http.StatusForbidden, fmt.Errorf("%s may not get %s", u.Username, path))
			return
		}
		next.ServeHTTP(w, r)
	})
}
