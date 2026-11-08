package domain

import "testing"

func TestCanonicalRole(t *testing.T) {
	for in, want := range map[string]string{"": RoleBoth, RoleBoth: RoleBoth, RolePrefill: RolePrefill, RoleDecode: RoleDecode} {
		if got := CanonicalRole(in); got != want {
			t.Errorf("CanonicalRole(%q) = %q, want %q", in, got, want)
		}
		if got := RoleOfVC(VariantCapacity{Role: in}); got != want {
			t.Errorf("RoleOfVC(role %q) = %q, want %q", in, got, want)
		}
	}
}
