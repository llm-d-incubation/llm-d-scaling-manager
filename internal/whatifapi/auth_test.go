package whatifapi

import (
	"net/http"
	"net/http/httptest"
	"testing"

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

// fakeCluster is a clientset whose TokenReview authenticates the token
// "good" as user alice, and whose SubjectAccessReview allows alice GET on
// allowed only. It records the last review's attributes.
func fakeCluster(allowed string, got *authzv1.SubjectAccessReviewSpec) *fake.Clientset {
	c := fake.NewClientset()
	c.PrependReactor("create", "tokenreviews", func(a k8stesting.Action) (bool, runtime.Object, error) {
		tr := a.(k8stesting.CreateAction).GetObject().(*authnv1.TokenReview)
		out := tr.DeepCopy()
		if tr.Spec.Token == "good" {
			out.Status = authnv1.TokenReviewStatus{Authenticated: true,
				User: authnv1.UserInfo{Username: "alice", Groups: []string{"planners"}, Extra: map[string]authnv1.ExtraValue{"scopes": {"a"}}}}
		}
		return true, out, nil
	})
	c.PrependReactor("create", "subjectaccessreviews", func(a k8stesting.Action) (bool, runtime.Object, error) {
		sar := a.(k8stesting.CreateAction).GetObject().(*authzv1.SubjectAccessReview)
		*got = sar.Spec
		out := sar.DeepCopy()
		r := sar.Spec.NonResourceAttributes
		out.Status.Allowed = sar.Spec.User == "alice" && r != nil && r.Path == allowed && r.Verb == "get"
		return true, out, nil
	})
	return c
}

// With Kubernetes authentication: no token, or one TokenReview does not
// authenticate, is 401; a token whose user the SubjectAccessReview does not
// allow is 403; an allowed one reaches the API, authorized as GET on the path
// under /whatif.
func TestAuthenticate(t *testing.T) {
	var spec authzv1.SubjectAccessReviewSpec
	h := Authenticate(fakeCluster("/whatif/v1/plan", &spec), New(Config{Resolver: &stubResolver{}}).Handler())
	for _, c := range []struct {
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
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, c.path, nil)
			if c.token != "" {
				req.Header.Set("Authorization", c.token)
			}
			h.ServeHTTP(rec, req)
			assert.Equal(t, c.code, rec.Code, rec.Body.String())
		})
	}
	require.NotNil(t, spec.NonResourceAttributes)
	assert.Equal(t, authzv1.NonResourceAttributes{Path: "/whatif/v1/plan", Verb: "get"}, *spec.NonResourceAttributes)
	assert.Equal(t, []string{"planners"}, spec.Groups)
	assert.Equal(t, authzv1.ExtraValue{"a"}, spec.Extra["scopes"])
}
