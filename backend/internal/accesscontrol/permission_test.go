package accesscontrol

import "testing"

func TestEffectiveAccess(t *testing.T) {
	tests := []struct {
		name   string
		grants []DatabaseAccess
		want   DatabaseAccess
	}{
		{name: "no grants", want: AccessNone},
		{name: "view only", grants: []DatabaseAccess{AccessViewOnly}, want: AccessViewOnly},
		{name: "edit grant wins", grants: []DatabaseAccess{AccessViewOnly, AccessViewEdit}, want: AccessViewEdit},
		{name: "unknown grants are ignored", grants: []DatabaseAccess{"unknown"}, want: AccessNone},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := EffectiveAccess(test.grants); got != test.want {
				t.Fatalf("EffectiveAccess(%v) = %v, want %v", test.grants, got, test.want)
			}
		})
	}
}

func TestDatabaseAccessCapabilities(t *testing.T) {
	if AccessNone.CanView() || AccessNone.CanEdit() {
		t.Fatal("AccessNone must not allow viewing or editing")
	}
	if !AccessViewOnly.CanView() || AccessViewOnly.CanEdit() {
		t.Fatal("AccessViewOnly must allow viewing only")
	}
	if !AccessViewEdit.CanView() || !AccessViewEdit.CanEdit() {
		t.Fatal("AccessViewEdit must allow viewing and editing")
	}
}
