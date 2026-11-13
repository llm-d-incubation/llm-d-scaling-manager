package whatifapi

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	authnv1 "k8s.io/api/authentication/v1"
	authzv1 "k8s.io/api/authorization/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// Loopback without --auth serves; any other bind refuses to start without
// --auth=kubernetes.
func TestCheckListen(t *testing.T) {
	for _, c := range []struct {
		addr, mode string
		ok         bool
	}{
		{"127.0.0.1:8080", AuthNone, true},
		{"localhost:8080", AuthNone, true},
		{"[::1]:8080", AuthNone, true},
		{"127.0.0.2:8080", AuthNone, true},
		{":8080", AuthNone, false},
		{"0.0.0.0:8080", AuthNone, false},
		{"10.1.2.3:8080", AuthNone, false},
		{"planner.example:8080", AuthNone, false},
		{"0.0.0.0:8443", AuthKubernetes, true},
		{"127.0.0.1:8080", "basic", false},
		{"127.0.0.1", AuthNone, false},
	} {
		err := CheckListen(c.addr, c.mode)
		assert.Equal(t, c.ok, err == nil, "%s %s: %v", c.addr, c.mode, err)
	}
}

// cluster is a fake API server: TokenReview authenticates the token "good" as
// alice; SubjectAccessReview allows alice get on the non-resource path
// allowedPath and on pods in allowedNS. It counts the reviews, keeps the last
// of each, and with fail set answers every review with an error.
type cluster struct {
	*fake.Clientset
	allowedPath, allowedNS string
	fail                   error
	tokens, sars           int
	lastToken              authnv1.TokenReviewSpec
	lastSAR                authzv1.SubjectAccessReviewSpec
}

func newCluster(allowedPath, allowedNS string) *cluster {
	c := &cluster{Clientset: fake.NewClientset(), allowedPath: allowedPath, allowedNS: allowedNS}
	c.PrependReactor("create", "tokenreviews", func(a k8stesting.Action) (bool, runtime.Object, error) {
		c.tokens++
		if c.fail != nil {
			return true, nil, c.fail
		}
		tr := a.(k8stesting.CreateAction).GetObject().(*authnv1.TokenReview)
		c.lastToken = tr.Spec
		out := tr.DeepCopy()
		if tr.Spec.Token == "good" {
			out.Status = authnv1.TokenReviewStatus{Authenticated: true,
				User: authnv1.UserInfo{Username: "alice", Groups: []string{"planners"}, Extra: map[string]authnv1.ExtraValue{"scopes": {"a"}}}}
		}
		return true, out, nil
	})
	c.PrependReactor("create", "subjectaccessreviews", func(a k8stesting.Action) (bool, runtime.Object, error) {
		c.sars++
		sar := a.(k8stesting.CreateAction).GetObject().(*authzv1.SubjectAccessReview)
		c.lastSAR = sar.Spec
		out := sar.DeepCopy()
		nr, r := sar.Spec.NonResourceAttributes, sar.Spec.ResourceAttributes
		out.Status.Allowed = sar.Spec.User == "alice" &&
			(nr != nil && nr.Path == c.allowedPath && nr.Verb == "get" ||
				r != nil && r.Namespace == c.allowedNS && r.Resource == "pods" && r.Verb == "get")
		return true, out, nil
	})
	return c
}

// call sends one request through h.
func call(h http.Handler, path, auth string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, path, nil)
	req.Header.Set("Content-Type", "application/json")
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	h.ServeHTTP(rec, req)
	return rec
}

// With Kubernetes authentication: no token, or one TokenReview does not
// authenticate, is 401; a token whose user the SubjectAccessReview does not
// allow is 403; an allowed one reaches the API, authorized as get on the path
// under /whatif, for the audiences asked.
func TestAuthenticate(t *testing.T) {
	c := newCluster("/whatif/v1/plan", "")
	h := NewAuthenticator(c, []string{"whatif"}, 0, nil).Wrap(New(Config{Resolver: &stubResolver{}}).Handler())
	for _, tc := range []struct {
		name, path, token string
		code              int
	}{
		{"no token", "/v1/plan", "", http.StatusUnauthorized},
		{"bad token", "/v1/plan", "Bearer bad", http.StatusUnauthorized},
		{"not a bearer", "/v1/plan", "Basic good", http.StatusUnauthorized},
		{"no SAR for the path", "/v1/inputs", "Bearer good", http.StatusForbidden},
		// Allowed: the API answers (a 400, the empty body), past the filter.
		{"allowed", "/v1/plan", "Bearer good", http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := call(h, tc.path, tc.token)
			assert.Equal(t, tc.code, rec.Code, rec.Body.String())
		})
	}
	require.NotNil(t, c.lastSAR.NonResourceAttributes)
	assert.Equal(t, authzv1.NonResourceAttributes{Path: "/whatif/v1/plan", Verb: "get"}, *c.lastSAR.NonResourceAttributes)
	assert.Equal(t, []string{"planners"}, c.lastSAR.Groups)
	assert.Equal(t, authzv1.ExtraValue{"a"}, c.lastSAR.Extra["scopes"])
	assert.Equal(t, []string{"whatif"}, c.lastToken.Audiences)
}

// A token's review and a path's are reused for the TTL, then asked again.
func TestAuthenticateCachesReviews(t *testing.T) {
	c := newCluster("/whatif/v1/plan", "")
	a := NewAuthenticator(c, nil, time.Minute, nil)
	now := time.Unix(1000, 0)
	a.now = func() time.Time { return now }
	h := a.Wrap(New(Config{Resolver: &stubResolver{}}).Handler())
	for range 3 {
		require.Equal(t, http.StatusBadRequest, call(h, "/v1/plan", "Bearer good").Code)
	}
	assert.Equal(t, 1, c.tokens, "token reviews")
	assert.Equal(t, 1, c.sars, "access reviews")
	now = now.Add(2 * time.Minute)
	require.Equal(t, http.StatusBadRequest, call(h, "/v1/plan", "Bearer good").Code)
	assert.Equal(t, 2, c.tokens, "a lapsed review is asked again")
	assert.Equal(t, 2, c.sars)
}

// A review that fails is a 500 that says nothing of the client's error, which
// goes to the log.
func TestAuthenticateHidesReviewErrors(t *testing.T) {
	c := newCluster("/whatif/v1/plan", "")
	c.fail = errors.New("dial tcp 10.0.0.1:6443: secret-internal-detail")
	var logged []string
	h := NewAuthenticator(c, nil, 0, func(format string, args ...any) {
		logged = append(logged, fmt.Sprintf(format, args...))
	}).Wrap(New(Config{Resolver: &stubResolver{}}).Handler())
	rec := call(h, "/v1/plan", "Bearer good")
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.NotContains(t, rec.Body.String(), "secret-internal-detail")
	require.Len(t, logged, 1)
	assert.Contains(t, logged[0], "secret-internal-detail")
}

// A namespace is read only by a user allowed get on its Pods.
func TestNamespaceAllowed(t *testing.T) {
	c := newCluster("/whatif/v1/inputs", "llm")
	a := NewAuthenticator(c, nil, 0, nil)
	var got map[string]error
	h := a.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = map[string]error{"llm": a.NamespaceAllowed(r.Context(), "llm"), "other": a.NamespaceAllowed(r.Context(), "other")}
	}))
	call(h, "/v1/inputs", "Bearer good")
	require.NotNil(t, got)
	assert.NoError(t, got["llm"])
	var forbidden ForbiddenError
	assert.ErrorAs(t, got["other"], &forbidden)
	require.NotNil(t, c.lastSAR.ResourceAttributes)
	assert.Equal(t, authzv1.ResourceAttributes{Namespace: "other", Verb: "get", Resource: "pods"}, *c.lastSAR.ResourceAttributes)
	assert.ErrorAs(t, a.NamespaceAllowed(t.Context(), "llm"), &forbidden, "no authenticated user")
}
