package crd

import (
	"errors"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// fakeGroupVersion answers ServerResourcesForGroupVersion with a fixed list
// or error.
type fakeGroupVersion struct {
	list *metav1.APIResourceList
	err  error
}

func (f fakeGroupVersion) ServerResourcesForGroupVersion(string) (*metav1.APIResourceList, error) {
	return f.list, f.err
}

func TestKEDAAbsent(t *testing.T) {
	gr := schema.GroupResource{Group: "keda.sh", Resource: "scaledobjects"}
	cases := []struct {
		name       string
		disc       fakeGroupVersion
		wantAbsent bool
		wantErr    bool
	}{
		{name: "served", disc: fakeGroupVersion{list: apiList(kedaGroupVersion, "ScaledObject", "ScaledJob")}},
		{name: "group version not served", disc: fakeGroupVersion{err: apierrors.NewNotFound(gr, "")}, wantAbsent: true},
		{name: "served without ScaledObject", disc: fakeGroupVersion{list: apiList(kedaGroupVersion, "ScaledJob")}, wantAbsent: true},
		// Forbidden is not absent: an unreadable answer must keep KEDA's
		// path, never force every model passive.
		{name: "forbidden", disc: fakeGroupVersion{err: apierrors.NewForbidden(gr, "", errors.New("denied"))}, wantErr: true},
		{name: "timeout", disc: fakeGroupVersion{err: apierrors.NewTimeoutError("slow", 1)}, wantErr: true},
		{name: "transport error", disc: fakeGroupVersion{err: errors.New("connection refused")}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			absent, err := kedaAbsent(tc.disc)
			if absent != tc.wantAbsent || (err != nil) != tc.wantErr {
				t.Fatalf("kedaAbsent = %v, %v; want %v, error %v", absent, err, tc.wantAbsent, tc.wantErr)
			}
		})
	}
}
