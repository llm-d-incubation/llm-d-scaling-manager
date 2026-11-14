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
	"time"

	authnv1 "k8s.io/api/authentication/v1"
	authzv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// The authentication modes of `capacity-planner serve --auth`.
const (
	// AuthNone serves with no authentication: loopback only.
	AuthNone = "none"
	// AuthKubernetes checks a bearer token with TokenReview and the
	// non-resource URL with SubjectAccessReview.
	AuthKubernetes = "kubernetes"
)

// authPathPrefix is what an API path is authorized as: get on
// /whatif/v1/plan and so on, so that one rule (nonResourceURLs
// ["/whatif/*"], verbs ["get"]) grants the read-only API.
const authPathPrefix = "/whatif"

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
	if mode == AuthNone && !isLoopback(host) {
		return fmt.Errorf("--listen %s is not a loopback address: serving it needs --auth=%s (a bearer token checked with TokenReview and SubjectAccessReview)",
			addr, AuthKubernetes)
	}
	return nil
}

// isLoopback reports whether host names a loopback address: localhost, or
// an IP in 127.0.0.0/8 or ::1. An empty host is every interface. It is the
// one rule for both the address served (CheckListen) and the Host a request
// names (LoopbackOnly).
func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// The review cache's bounds.
const (
	// defaultReviewTTL is how long a TokenReview's and a SubjectAccessReview's
	// answer is reused: a planner page sends a request per slider move.
	defaultReviewTTL = 30 * time.Second
	// maxReviews is how many answers are kept, the least used dropped first,
	// so that tokens sent at random cannot grow the cache.
	maxReviews = 4096
	// reviewTimeout bounds one review: it runs apart from the request that
	// asked for it, which others may share.
	reviewTimeout = 10 * time.Second
)

// Authenticator is Kubernetes authentication and authorization, as the
// controller's metrics endpoint does it: a bearer token's user, from
// TokenReview, must be allowed get on the request's path under
// authPathPrefix, and a Prometheus source's namespace is checked with a
// resource SubjectAccessReview (NamespaceAllowed). Answers are reused for the
// TTL; failures are not kept.
type Authenticator struct {
	client kubernetes.Interface
	// audiences are the TokenReview's; none is the API server's own.
	audiences []string
	ttl       time.Duration
	// logf receives what a failed review says; the client is answered with
	// a generic message, never the client's error text. It is the server's
	// stderr, not ctrl.Log: `capacity-planner serve` discards controller-runtime's
	// logger, which the analyzers a replay runs write to.
	logf func(format string, args ...any)
	now  func() time.Time

	reviews  *lru[cachedReview]
	inFlight flight[cachedReview]
}

// cachedReview is a review's answer and when it lapses: the user, and
// whether the review authenticated or allowed.
type cachedReview struct {
	user    authnv1.UserInfo
	ok      bool
	expires time.Time
}

// NewAuthenticator is an Authenticator on c. ttl 0 is defaultReviewTTL; logf
// nil drops what a failed review says.
func NewAuthenticator(c kubernetes.Interface, audiences []string, ttl time.Duration, logf func(string, ...any)) *Authenticator {
	if ttl <= 0 {
		ttl = defaultReviewTTL
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Authenticator{client: c, audiences: audiences, ttl: ttl, logf: logf, now: time.Now, reviews: newLRU[cachedReview](maxReviews)}
}

// userKey is where the authenticated user sits in a request's context.
type userKey struct{}

// errReview is what a client is told when a review could not be made.
var errReview = errors.New("the request could not be authorized: the server could not reach the Kubernetes API; see its log")

// cached is the answer at key: the one kept while it holds, else review's,
// one review per key at a time. review runs apart from ctx's cancellation,
// with its own timeout: callers waiting on it must not fail for one client
// gone. A failed review is not kept.
func (a *Authenticator) cached(ctx context.Context, key string, review func(context.Context) (cachedReview, error)) (cachedReview, error) {
	now := a.now()
	if c, ok := a.reviews.get(key); ok && now.Before(c.expires) {
		return c, nil
	}
	return a.inFlight.do(key, func() (cachedReview, error) {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), reviewTimeout)
		defer cancel()
		c, err := review(rctx)
		if err != nil {
			return c, err
		}
		c.expires = now.Add(a.ttl)
		a.reviews.put(key, c)
		return c, nil
	})
}

// hashKey is a cache key for kind and parts: their hash, so that the cache
// holds no token and no user's attributes in the clear.
func hashKey(kind string, parts ...[]byte) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write(p)
		h.Write([]byte{0})
	}
	return kind + "\x00" + hex.EncodeToString(h.Sum(nil))
}

// authenticate is the token's review: the user, and ok, whether TokenReview
// authenticated the token.
func (a *Authenticator) authenticate(ctx context.Context, token string) (cachedReview, error) {
	return a.cached(ctx, hashKey("token", []byte(token)), func(ctx context.Context) (cachedReview, error) {
		tr, err := a.client.AuthenticationV1().TokenReviews().Create(ctx,
			&authnv1.TokenReview{Spec: authnv1.TokenReviewSpec{Token: token, Audiences: a.audiences}}, metav1.CreateOptions{})
		if err != nil {
			return cachedReview{}, err
		}
		return cachedReview{user: tr.Status.User, ok: tr.Status.Authenticated}, nil
	})
}

// allowed asks a SubjectAccessReview whether u may do res or nonRes (one of
// the two set).
func (a *Authenticator) allowed(ctx context.Context, u authnv1.UserInfo, res *authzv1.ResourceAttributes, nonRes *authzv1.NonResourceAttributes) (bool, error) {
	attrs, err := json.Marshal(struct {
		U      authnv1.UserInfo
		Res    *authzv1.ResourceAttributes
		NonRes *authzv1.NonResourceAttributes
	}{u, res, nonRes})
	if err != nil {
		return false, err
	}
	c, err := a.cached(ctx, hashKey("sar", attrs), func(ctx context.Context) (cachedReview, error) {
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
// 401; a user not allowed get on authPathPrefix plus the path is 403; a review
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
			a.logf("capacity-planner: token review: %v", err)
			writeError(w, http.StatusInternalServerError, errReview)
			return
		}
		if !u.ok {
			writeError(w, http.StatusUnauthorized, errors.New("the bearer token is not authenticated"))
			return
		}
		path := authPathPrefix + r.URL.Path
		allowed, err := a.allowed(r.Context(), u.user, nil, &authzv1.NonResourceAttributes{Path: path, Verb: "get"})
		if err != nil {
			a.logf("capacity-planner: subject access review: %v", err)
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

// namespaceResource is what a user must be allowed in a namespace to read it
// through the server's Prometheus: get on the Pods its models run in.
func namespaceResource(namespace string) *authzv1.ResourceAttributes {
	return &authzv1.ResourceAttributes{Namespace: namespace, Verb: "get", Resource: "pods"}
}

// NamespaceAllowed reports, as an error, whether the request's user may read
// namespace: get on its Pods (namespaceResource), by SubjectAccessReview. A
// request Wrap did not authenticate is refused.
func (a *Authenticator) NamespaceAllowed(ctx context.Context, namespace string) error {
	u, ok := ctx.Value(userKey{}).(authnv1.UserInfo)
	if !ok {
		return ForbiddenError{Err: errors.New("no authenticated user")}
	}
	allowed, err := a.allowed(ctx, u, namespaceResource(namespace), nil)
	if err != nil {
		a.logf("capacity-planner: subject access review: %v", err)
		return errReview
	}
	if !allowed {
		return ForbiddenError{Err: fmt.Errorf("%s may not get pods in namespace %s, so may not read it through this server", u.Username, namespace)}
	}
	return nil
}

// ForbiddenError is a request the user is not allowed: the server answers
// 403.
type ForbiddenError struct{ Err error }

// Error is the reason the request is not allowed.
func (e ForbiddenError) Error() string {
	if e.Err == nil {
		return "forbidden"
	}
	return e.Err.Error()
}

// Unwrap is the reason, for errors.Is and errors.As.
func (e ForbiddenError) Unwrap() error { return e.Err }
