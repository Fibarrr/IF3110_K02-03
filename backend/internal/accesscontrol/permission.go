package accesscontrol

// DatabaseAccess menyimpan tingkat akses grup ke sebuah database.
type DatabaseAccess string

const (
	AccessNone     DatabaseAccess = "none"
	AccessViewOnly DatabaseAccess = "view_only"
	AccessViewEdit DatabaseAccess = "view_edit"
)

// EffectiveAccess memilih izin paling tinggi dari semua grup pengguna.
// Kalau ada View + Edit, izin itu yang dipakai. Tanpa izin, aksesnya ditolak.
func EffectiveAccess(grants []DatabaseAccess) DatabaseAccess {
	effective := AccessNone
	for _, grant := range grants {
		if permissionRank(grant) > permissionRank(effective) {
			effective = grant
		}
	}
	return effective
}

func permissionRank(access DatabaseAccess) int {
	switch access {
	case AccessViewOnly:
		return 1
	case AccessViewEdit:
		return 2
	default:
		return 0
	}
}

// CanView mengecek apakah aksesnya mengizinkan pengguna melihat database.
func (access DatabaseAccess) CanView() bool {
	return access == AccessViewOnly || access == AccessViewEdit
}

// CanEdit mengecek apakah aksesnya mengizinkan pengguna mengubah database.
func (access DatabaseAccess) CanEdit() bool {
	return access == AccessViewEdit
}
