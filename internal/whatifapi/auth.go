package whatifapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

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

// DefaultReviewTTL is how long a TokenReview's and a SubjectAccessReview's
// answer is reused: a planner page sends a request per slider move.
const DefaultReviewTTL = 30 * time.Second

// Authenticator is Kubernetes authentication and authorization, as the
// controller's metrics endpoint does it: a bearer token's user, from
// TokenReview, must be allowed get on the request's path under
// AuthPathPrefix, and a Prometheus source's namespace is checked with a
// resource SubjectAccessReview (NamespaceAllowed). Answers are reused for TTL.
type Authenticator struct {
	client kubernetes.Interface
	// audiences are the TokenReview's; none is the API server's own.
	audiences []string
	ttl       time.Duration
	// logf receives what a failed review says; the client is answered with
	// a generic message, never the client's error text.
	logf func(format string, args ...any)
	now  func() time.Time

	mu      sync.Mutex
	reviews map[string]cachedReview
}

// cachedReview is a review's answer and when it lapses: the user, or
// whether a review allowed.
type cachedReview struct {
	user    authnv1.UserInfo
	ok      bool
	expires time.Time
}

// NewAuthenticator is an Authenticator on c. ttl 0 is DefaultReviewTTL; logf
// nil drops what a failed review says.
func NewAuthenticator(c kubernetes.Interface, audiences []string, ttl time.Duration, logf func(string, ...any)) *Authenticator {
	if ttl <= 0 {
		ttl = DefaultReviewTTL
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Authenticator{client: c, audiences: audiences, ttl: ttl, logf: logf, now: time.Now, reviews: map[string]cachedReview{}}
}

// userKey is where the authenticated user sits in a request's context.
type userKey struct{}

// errReview is what a client is told when a review could not be made.
var errReview = errors.New("the request could not be authorized: the server could not reach the Kubernetes API; see its log")

// cached is the review at key, priced by review on a miss or a lapsed answer.
// A failed review is not cached.
func (a *Authenticator) cached(key string, review func() (cachedReview, error)) (cachedReview, error) {
	a.mu.Lock()
	c, ok := a.reviews[key]
	a.mu.Unlock()
	if ok && a.now().Before(c.expires) {
		return c, nil
	}
	c, err := review()
	if err != nil {
		return c, err
	}
	c.expires = a.now().Add(a.ttl)
	a.mu.Lock()
	for k, old := range a.reviews {
		if !a.now().Before(old.expires) {
			delete(a.reviews, k)
		}
	}
	a.reviews[key] = c
	a.mu.Unlock()
	return c, nil
}

// tokenKey is a token's cache key: its hash, so the cache holds no token.
func tokenKey(token string) string {
	sum := sha256.Sum256([]byte(token))
	return "token\x00" + hex.EncodeToString(sum[:])
}

// authenticate is the token's user, ok false when TokenReview does not
// authenticate it.
func (a *Authenticator) authenticate(ctx context.Context, token string) (cachedReview, error) {
	return a.cached(tokenKey(token), func() (cachedReview, error) {
		tr, err := a.client.AuthenticationV1().TokenReviews().Create(ctx,
			&authnv1.TokenReview{Spec: authnv1.TokenReviewSpec{Token: token, Audiences: a.audiences}}, metav1.CreateOptions{})
		if err != nil {
			return cachedReview{}, err
		}
		return cachedReview{user: tr.Status.User, ok: tr.Status.Authenticated}, nil
	})
}

// allowed asks a SubjectAccessReview whether u may do attrs (one of the
// two set).
func (a *Authenticator) allowed(ctx context.Context, u authnv1.UserInfo, res *authzv1.ResourceAttributes, nonRes *authzv1.NonResourceAttributes) (bool, error) {
	key, err := json.Marshal(struct {
		U      authnv1.UserInfo
		Res    *authzv1.ResourceAttributes
		NonRes *authzv1.NonResourceAttributes
	}{u, res, nonRes})
	if err != nil {
		return false, err
	}
	c, err := a.cached("sar\x00"+string(key), func() (cachedReview, error) {
		extra := map[string]authzv1.ExtraValue{}
		for k, v := range u.Extra {
			extra[k] = authzv1.ExtraValue(v)
		}
		sar, err := a.client.AuthorizationV1().SubjectAccessReviews().Create(ctx, &authzv1.SubjectAccessReview{
			Spec: authzv1.SubjectAccessReviewSpec{User: u.Username, Groups: u.Groups, UID: u.UID, Extra: extra,
				ResourceAttributes: res, NonResourceAttributes: nonRes},
		}, metav1.CreateOptions{})
		if err != nil {
			return cachedReview{}, err
		}
		return cachedReview{ok: sar.Status.Allowed}, nil
	})
	return c.ok, err
}

// Wrap wraps next: no token, or one TokenReview does not authenticate, is
// 401; a user not allowed get on AuthPathPrefix plus the path is 403; a review
// that fails is 500, with its error logged and not returned.
func (a *Authenticator) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		token = strings.TrimSpace(token)
		if !ok || token == "" {
			writeError(w, http.StatusUnauthorized, errors.New("a bearer token is required"))
			return
		}
		u, err := a.authenticate(r.Context(), token)
		if err != nil {
			a.logf("whatif: token review: %v", err)
			writeError(w, http.StatusInternalServerError, errReview)
			return
		}
		if !u.ok {
			writeError(w, http.StatusUnauthorized, errors.New("the bearer token is not authenticated"))
			return
		}
		path := AuthPathPrefix + r.URL.Path
		allowed, err := a.allowed(r.Context(), u.user, nil, &authzv1.NonResourceAttributes{Path: path, Verb: "get"})
		if err != nil {
			a.logf("whatif: subject access review: %v", err)
			writeError(w, http.StatusInternalServerError, errReview)
			return
		}
		if !allowed {
			writeError(w, http.StatusForbidden, fmt.Errorf("%s may not get %s", u.user.Username, path))
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userKey{}, u.user)))
	})
}

// NamespaceResource is what a user must be allowed to get in a namespace to
// read it through the server's Prometheus: the Pods its models run in.
var NamespaceResource = authzv1.ResourceAttributes{Verb: "get", Resource: "pods"}

// NamespaceAllowed reports, as an error, whether the request's user may read
// namespace: get on its Pods (NamespaceResource), by SubjectAccessReview. A
// request Wrap did not authenticate is refused.
func (a *Authenticator) NamespaceAllowed(ctx context.Context, namespace string) error {
	u, ok := ctx.Value(userKey{}).(authnv1.UserInfo)
	if !ok {
		return ErrForbidden{Err: errors.New("no authenticated user")}
	}
	res := NamespaceResource
	res.Namespace = namespace
	allowed, err := a.allowed(ctx, u, &res, nil)
	if err != nil {
		a.logf("whatif: subject access review: %v", err)
		return errReview
	}
	if !allowed {
		return ErrForbidden{Err: fmt.Errorf("%s may not get pods in namespace %s, so may not read it through this server", u.Username, namespace)}
	}
	return nil
}

// ErrForbidden is a request the user is not allowed: the server answers 403.
type ErrForbidden struct{ Err error }

func (e ErrForbidden) Error() string { return e.Err.Error() }

func (e ErrForbidden) Unwrap() error { return e.Err }
